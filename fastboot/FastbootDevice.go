package fastboot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/gousb"
)

type FastbootResponseStatus string

var Status = struct {
	OKAY FastbootResponseStatus
	FAIL FastbootResponseStatus
	DATA FastbootResponseStatus
	INFO FastbootResponseStatus
}{
	OKAY: "OKAY",
	FAIL: "FAIL",
	DATA: "DATA",
	INFO: "INFO",
}

var Error = struct {
	VarNotFound    error
	DeviceNotFound error
}{
	VarNotFound:    errors.New("variable not found"),
	DeviceNotFound: errors.New("device not found"),
}

type FastbootDevice struct {
	Device   *gousb.Device
	Context  *gousb.Context
	In       *gousb.InEndpoint
	Out      *gousb.OutEndpoint
	Progress io.Writer
	Unclaim  func()
}

func FindDevices() ([]*FastbootDevice, error) {
	ctx := gousb.NewContext()
	var fastbootDevices []*FastbootDevice
	devs, err := ctx.OpenDevices(func(desc *gousb.DeviceDesc) bool {
		for _, cfg := range desc.Configs {
			for _, ifc := range cfg.Interfaces {
				for _, alt := range ifc.AltSettings {
					return alt.Protocol == 0x03 && alt.Class == 0xff && alt.SubClass == 0x42
				}
			}
		}
		return true
	})

	if err != nil && len(devs) == 0 {
		return nil, err
	}

	for _, dev := range devs {
		intf, done, err := dev.DefaultInterface()
		if err != nil {
			continue
		}
		inEndpoint, err := intf.InEndpoint(0x81)
		if err != nil {
			continue
		}
		outEndpoint, err := intf.OutEndpoint(0x01)
		if err != nil {
			continue
		}
		fastbootDevices = append(fastbootDevices, &FastbootDevice{
			Device:  dev,
			Context: ctx,
			In:      inEndpoint,
			Out:     outEndpoint,
			Unclaim: done,
		})
	}

	return fastbootDevices, nil
}

func FindDevice(serial string) (*FastbootDevice, error) {
	devs, err := FindDevices()

	if err != nil {
		return &FastbootDevice{}, err
	}

	for _, dev := range devs {
		s, e := dev.Device.SerialNumber()
		if e != nil {
			continue
		}
		if serial != s {
			continue
		}
		return dev, nil
	}

	return &FastbootDevice{}, Error.DeviceNotFound
}

func (d *FastbootDevice) Close() {
	d.Unclaim()
	d.Device.Close()
	d.Context.Close()
}

func (d *FastbootDevice) Send(data []byte) error {
	_, err := d.Out.Write(data)
	return err
}

func (d *FastbootDevice) GetMaxPacketSize() (int, error) {
	return d.Out.Desc.MaxPacketSize, nil
}

func (d *FastbootDevice) Recv() (FastbootResponseStatus, []byte, error) {
	var data []byte
	buf := make([]byte, d.In.Desc.MaxPacketSize)

	stopWaitLog := d.beginBlockingLog(fmt.Sprintf("usb read pending, max-packet=%d", len(buf)))
	defer stopWaitLog()

	n, err := d.In.Read(buf)
	if err != nil {
		return Status.FAIL, []byte{}, err
	}
	data = append(data, buf[:n]...)
	var status FastbootResponseStatus
	switch string(data[:4]) {
	case "OKAY":
		status = Status.OKAY
	case "FAIL":
		status = Status.FAIL
	case "DATA":
		status = Status.DATA
	case "INFO":
		status = Status.INFO
	}
	return status, data[4:], nil
}

func (d *FastbootDevice) GetVar(variable string) (string, error) {
	lines, value, err := d.getVarResponses(variable)
	if err != nil {
		return "", err
	}
	if value != "" {
		return value, nil
	}
	if len(lines) == 1 {
		return lines[0], nil
	}
	return strings.Join(lines, "\n"), nil
}

func (d *FastbootDevice) GetVarAll() ([]string, error) {
	lines, value, err := d.getVarResponses("all")
	if err != nil {
		return nil, err
	}
	if value != "" {
		lines = append(lines, value)
	}
	return lines, nil
}

func (d *FastbootDevice) BootImage(data []byte) error {
	d.progressf("boot image requested, payload=%s\n", formatBinarySize(uint64(len(data))))
	err := d.Download(data)
	if err != nil {
		return err
	}

	err = d.sendCommand("boot")
	if err != nil {
		return err
	}

	status, data, _, err := d.recvFinalResponse()
	switch {
	case err != nil:
		return err
	case status != Status.OKAY:
		return fmt.Errorf("failed to boot image: %s %s", status, data)
	}
	return nil
}

func (d *FastbootDevice) Erase(partition string) error {
	d.progressf("erase requested, partition=%s\n", partition)
	resolvedPartition, err := d.resolvePartition(partition)
	if err != nil {
		return err
	}

	return d.runCommand(fmt.Sprintf("erase:%s", resolvedPartition), fmt.Sprintf("erase %s", resolvedPartition))
}

func (d *FastbootDevice) OEM(args ...string) error {
	command := strings.TrimSpace(strings.Join(args, " "))
	if command == "" {
		return fmt.Errorf("oem command must not be empty")
	}

	d.progressf("oem requested, command=%s\n", command)
	return d.runCommand("oem "+command, "oem "+command)
}

func (d *FastbootDevice) Flash(partition string, data []byte) error {
	d.progressf("flash requested, partition=%s payload=%s\n", partition, formatBinarySize(uint64(len(data))))
	resolvedPartition, err := d.resolvePartition(partition)
	if err != nil {
		return err
	}

	maxDownloadSize := d.maxDownloadSize()
	d.progressf("flash plan, partition=%s max-download-size=%s\n", resolvedPartition, formatBinarySize(maxDownloadSize))
	if uint64(len(data)) <= maxDownloadSize {
		return d.flashSingle(resolvedPartition, data)
	}

	d.progressf("payload exceeds max-download-size, using sparse split for %s\n", resolvedPartition)

	return forEachFlashData(data, maxDownloadSize, func(index int, payload []byte) error {
		label := fmt.Sprintf("%s sparse chunk %d", resolvedPartition, index)
		if err := d.flashSingleWithLabel(resolvedPartition, payload, label); err != nil {
			return fmt.Errorf("failed to flash sparse chunk %d: %w", index, err)
		}
		return nil
	})
}

func (d *FastbootDevice) FlashFile(partition string, path string) error {
	d.progressf("flash file requested, partition=%s path=%s\n", partition, path)
	resolvedPartition, err := d.resolvePartition(partition)
	if err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open image %q: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat image %q: %w", path, err)
	}
	if info.Size() < 0 {
		return fmt.Errorf("invalid image size for %q", path)
	}

	maxDownloadSize := d.maxDownloadSize()
	imageSize := uint64(info.Size())
	d.progressf("flash file opened, partition=%s size=%s max-download-size=%s\n", resolvedPartition, formatBinarySize(imageSize), formatBinarySize(maxDownloadSize))

	if imageSize <= maxDownloadSize {
		d.progressf("using direct download path for %s\n", resolvedPartition)
		return d.flashSingleReaderWithLabel(resolvedPartition, file, imageSize, resolvedPartition)
	}

	magic, err := readFileMagic(file)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to rewind image %q: %w", path, err)
	}

	if magic == androidSparseMagic {
		d.progressf("detected android sparse image for %s, using streamed sparse re-split\n", resolvedPartition)
		img, err := parseAndroidSparseImageReaderAt(file, imageSize)
		if err != nil {
			return fmt.Errorf("failed to parse sparse image %q: %w", path, err)
		}
		return img.forEachEncodedPieceStream(file, maxDownloadSize, func(index int, size uint64, reader io.Reader) error {
			label := fmt.Sprintf("%s sparse chunk %d", resolvedPartition, index)
			if err := d.flashSingleReaderWithLabel(resolvedPartition, reader, size, label); err != nil {
				return fmt.Errorf("failed to flash sparse chunk %d: %w", index, err)
			}
			return nil
		})
	}

	d.progressf("detected raw image for %s, using streamed sparse conversion\n", resolvedPartition)
	return forEachRawFileFlashStream(file, imageSize, maxDownloadSize, func(index int, size uint64, reader io.Reader) error {
		label := fmt.Sprintf("%s sparse chunk %d", resolvedPartition, index)
		if err := d.flashSingleReaderWithLabel(resolvedPartition, reader, size, label); err != nil {
			return fmt.Errorf("failed to flash sparse chunk %d: %w", index, err)
		}
		return nil
	})
}

func (d *FastbootDevice) flashSingle(partition string, data []byte) error {
	return d.flashSingleWithLabel(partition, data, partition)
}

func (d *FastbootDevice) flashSingleWithLabel(partition string, data []byte, label string) error {
	return d.flashSingleReaderWithLabel(partition, bytes.NewReader(data), uint64(len(data)), label)
}

func (d *FastbootDevice) maxDownloadSize() uint64 {
	maxSize := autoSparseMaxDownloadSize

	value, err := d.GetVar("max-download-size")
	if err != nil {
		d.progressf("max-download-size unavailable, using default %s\n", formatBinarySize(maxSize))
		return maxSize
	}

	size, err := parseMaxDownloadSize(value)
	if err != nil {
		d.progressf("failed to parse max-download-size %q, using default %s\n", value, formatBinarySize(maxSize))
		return maxSize
	}

	if size < maxSize {
		d.progressf("device max-download-size=%s\n", formatBinarySize(size))
		return size
	}

	d.progressf("device max-download-size=%s, capped to auto-sparse threshold %s\n", formatBinarySize(size), formatBinarySize(maxSize))
	return maxSize
}

func (d *FastbootDevice) resolvePartition(partition string) (string, error) {
	hasSlot, err := d.GetVar(fmt.Sprintf("has-slot:%s", partition))
	switch {
	case err == nil:
	case errors.Is(err, Error.VarNotFound):
		return partition, nil
	default:
		return "", fmt.Errorf("failed to query slot support for %q: %w", partition, err)
	}

	if !strings.EqualFold(strings.TrimSpace(hasSlot), "yes") {
		return partition, nil
	}

	currentSlot, err := d.GetVar("current-slot")
	if err != nil {
		return "", fmt.Errorf("failed to query current slot for %q: %w", partition, err)
	}

	suffix, err := normalizeSlotSuffix(currentSlot)
	if err != nil {
		return "", fmt.Errorf("failed to resolve current slot for %q: %w", partition, err)
	}

	resolvedPartition := partition + suffix
	d.progressf("resolved partition %s -> %s\n", partition, resolvedPartition)
	return resolvedPartition, nil
}

func normalizeSlotSuffix(value string) (string, error) {
	slot := strings.ToLower(strings.TrimSpace(value))
	slot = strings.TrimPrefix(slot, "_")
	if slot == "" {
		return "", fmt.Errorf("empty current-slot value")
	}

	for _, r := range slot {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", fmt.Errorf("unexpected current-slot value %q", value)
		}
	}

	return "_" + slot, nil
}

func (d *FastbootDevice) getVarResponses(variable string) ([]string, string, error) {
	d.progressf("query getvar %s\n", variable)
	if err := d.sendCommand(fmt.Sprintf("getvar:%s", variable)); err != nil {
		return nil, "", err
	}

	status, resp, infos, err := d.recvFinalResponse()
	if status == Status.FAIL {
		err = Error.VarNotFound
	}
	if err != nil {
		return nil, "", err
	}

	lines := make([]string, 0, len(infos))
	for _, info := range infos {
		lines = append(lines, string(info))
	}
	d.progressf("getvar %s completed, info-lines=%d final-bytes=%d\n", variable, len(lines), len(resp))
	return lines, string(resp), nil
}

func (d *FastbootDevice) Download(data []byte) error {
	return d.DownloadReader(bytes.NewReader(data), uint64(len(data)))
}

func (d *FastbootDevice) DownloadReader(r io.Reader, size uint64) error {
	return d.downloadReaderWithLabel(r, size, "")
}

func (d *FastbootDevice) downloadReaderWithLabel(r io.Reader, size uint64, label string) error {
	if size > uint64(^uint32(0)) {
		return fmt.Errorf("download too large: %d bytes", size)
	}

	d.progressf("starting download, label=%s size=%s\n", labelOrDefault(label, "payload"), formatBinarySize(size))
	err := d.sendCommand(fmt.Sprintf("download:%08x", size))
	if err != nil {
		return err
	}

	status, _, _, err := d.recvFinalResponse()
	switch {
	case err != nil:
		return err
	case status != Status.DATA:
		return fmt.Errorf("failed to start data phase: %s", status)
	}

	chunk_size := 0x40040
	buffer := make([]byte, chunk_size)
	progress := newDownloadProgress(d.Progress, label, size)

	for remaining := size; remaining > 0; {
		readSize := chunk_size
		if uint64(readSize) > remaining {
			readSize = int(remaining)
		}

		if _, err := io.ReadFull(r, buffer[:readSize]); err != nil {
			return err
		}
		if err := d.Send(buffer[:readSize]); err != nil {
			return err
		}
		remaining -= uint64(readSize)
		progress.advance(uint64(readSize))
	}
	status, resp, _, err := d.recvFinalResponse()
	switch {
	case err != nil:
		return err
	case status != Status.OKAY:
		return fmt.Errorf("failed to finish data phase: %s %s", status, resp)
	}
	progress.finish()
	return nil
}

func (d *FastbootDevice) flashSingleReader(partition string, r io.Reader, size uint64) error {
	return d.flashSingleReaderWithLabel(partition, r, size, partition)
}

func (d *FastbootDevice) flashSingleReaderWithLabel(partition string, r io.Reader, size uint64, label string) error {
	if err := d.downloadReaderWithLabel(r, size, label); err != nil {
		return err
	}

	d.progressf("flashing %s\n", label)
	return d.runCommand(fmt.Sprintf("flash:%s", partition), label)
}

type downloadProgress struct {
	writer     io.Writer
	label      string
	total      uint64
	sent       uint64
	lastBucket int
}

func newDownloadProgress(writer io.Writer, label string, total uint64) *downloadProgress {
	if writer == nil || label == "" || total == 0 {
		return nil
	}

	p := &downloadProgress{
		writer:     writer,
		label:      label,
		total:      total,
		lastBucket: 0,
	}
	p.print(0, 0)
	return p
}

func (p *downloadProgress) advance(written uint64) {
	if p == nil {
		return
	}

	p.sent += written
	bucket := int((p.sent * 100) / p.total / 10)
	if p.sent == p.total {
		bucket = 10
	}
	if bucket <= p.lastBucket {
		return
	}

	p.lastBucket = bucket
	percent := bucket * 10
	if percent > 100 {
		percent = 100
	}
	p.print(percent, p.sent)
}

func (p *downloadProgress) finish() {
	if p == nil {
		return
	}
	if p.sent < p.total {
		p.sent = p.total
	}
	if p.lastBucket < 10 {
		p.lastBucket = 10
		p.print(100, p.total)
	}
}

func (p *downloadProgress) print(percent int, sent uint64) {
	fmt.Fprintf(p.writer, "[%s] downloading %s: %3d%% (%s/%s)\n", time.Now().Format("15:04:05.000"), p.label, percent, formatBinarySize(sent), formatBinarySize(p.total))
}

func (d *FastbootDevice) progressf(format string, args ...any) {
	if d.Progress == nil {
		return
	}
	prefix := time.Now().Format("15:04:05.000")
	fmt.Fprintf(d.Progress, "[%s] %s", prefix, fmt.Sprintf(format, args...))
}

func (d *FastbootDevice) beginBlockingLog(operation string) func() {
	if d.Progress == nil {
		return func() {}
	}

	const (
		blockedLogThreshold = 3 * time.Second
		blockedLogInterval  = 3 * time.Second
	)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(blockedLogThreshold)
		defer timer.Stop()
		ticker := time.NewTicker(blockedLogInterval)
		defer ticker.Stop()

		warned := false
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				warned = true
				d.progressf("%s still waiting after %s\n", operation, formatDuration(time.Since(start)))
			case <-ticker.C:
				if warned {
					d.progressf("%s still waiting after %s\n", operation, formatDuration(time.Since(start)))
				}
			}
		}
	}()

	return func() {
		close(done)
	}
}

func formatDuration(value time.Duration) string {
	if value < time.Millisecond {
		return value.String()
	}
	return value.Round(time.Millisecond).String()
}

func formatBinarySize(value uint64) string {
	const (
		kiB = 1024
		miB = 1024 * kiB
		giB = 1024 * miB
	)

	switch {
	case value >= giB:
		return fmt.Sprintf("%.1f GiB", float64(value)/float64(giB))
	case value >= miB:
		return fmt.Sprintf("%.1f MiB", float64(value)/float64(miB))
	case value >= kiB:
		return fmt.Sprintf("%.1f KiB", float64(value)/float64(kiB))
	default:
		return fmt.Sprintf("%d B", value)
	}
}

func (d *FastbootDevice) recvFinalResponse() (FastbootResponseStatus, []byte, [][]byte, error) {
	status, resp, infos, err := recvFinalResponse(d.Recv)
	for _, info := range infos {
		d.progressf("info: %s\n", string(info))
	}
	if err != nil {
		d.progressf("recv error: %v\n", err)
		return status, resp, infos, err
	}
	d.progressf("response: %s %s\n", status, strings.TrimSpace(string(resp)))
	return status, resp, infos, err
}

func (d *FastbootDevice) sendCommand(command string) error {
	d.progressf("command: %s\n", command)
	stopWaitLog := d.beginBlockingLog(fmt.Sprintf("command send pending, command=%s", command))
	defer stopWaitLog()
	return d.Send([]byte(command))
}

func (d *FastbootDevice) runCommand(command string, label string) error {
	if err := d.sendCommand(command); err != nil {
		return err
	}

	status, resp, _, err := d.recvFinalResponse()
	switch {
	case err != nil:
		return err
	case status != Status.OKAY:
		return fmt.Errorf("failed to %s: %s %s", label, status, resp)
	}

	d.progressf("finished %s\n", label)
	return nil
}

func labelOrDefault(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func recvFinalResponse(recv func() (FastbootResponseStatus, []byte, error)) (FastbootResponseStatus, []byte, [][]byte, error) {
	var infos [][]byte
	for {
		status, resp, err := recv()
		if err != nil {
			return status, nil, infos, err
		}
		if status != Status.INFO {
			return status, resp, infos, nil
		}

		info := make([]byte, len(resp))
		copy(info, resp)
		infos = append(infos, info)
	}
}

func readFileMagic(file *os.File) (uint32, error) {
	buf := make([]byte, 4)
	n, err := io.ReadFull(file, buf)
	switch {
	case err == nil:
		return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24, nil
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return 0, fmt.Errorf("failed to rewind image: %w", seekErr)
		}
		if n == 4 {
			return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("failed to read image header: %w", err)
	}
}
