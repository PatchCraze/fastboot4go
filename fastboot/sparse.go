package fastboot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	androidSparseMagic            uint32 = 0xed26ff3a
	androidSparseMajorVersion     uint16 = 1
	androidSparseMinorVersion     uint16 = 0
	androidSparseFileHeaderSize   uint16 = 28
	androidSparseChunkHeaderSize  uint16 = 12
	androidSparseDefaultBlockSize uint32 = 4096

	androidSparseChunkRaw      uint16 = 0xCAC1
	androidSparseChunkFill     uint16 = 0xCAC2
	androidSparseChunkDontCare uint16 = 0xCAC3
	androidSparseChunkCRC32    uint16 = 0xCAC4

	autoSparseMaxDownloadSize uint64 = 512 << 20
)

type androidSparseHeader struct {
	Magic           uint32
	MajorVersion    uint16
	MinorVersion    uint16
	FileHeaderSize  uint16
	ChunkHeaderSize uint16
	BlockSize       uint32
	TotalBlocks     uint32
	TotalChunks     uint32
	ImageChecksum   uint32
}

type androidSparseChunkHeader struct {
	ChunkType uint16
	Reserved1 uint16
	ChunkSize uint32
	TotalSize uint32
}

type androidSparseChunk struct {
	chunkType  uint16
	block      uint32
	length     uint64
	data       []byte
	dataOffset int64
	fillValue  uint32
}

type androidSparseImage struct {
	blockSize   uint32
	totalLength uint64
	chunks      []androidSparseChunk
}

func splitFlashData(data []byte, maxDownloadSize uint64) ([][]byte, error) {
	parts := make([][]byte, 0, 1)
	err := forEachFlashData(data, maxDownloadSize, func(_ int, payload []byte) error {
		parts = append(parts, payload)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return parts, nil
}

func forEachFlashData(data []byte, maxDownloadSize uint64, fn func(index int, payload []byte) error) error {
	if maxDownloadSize == 0 {
		return fmt.Errorf("invalid max download size: %d", maxDownloadSize)
	}
	if uint64(len(data)) <= maxDownloadSize {
		return fn(1, data)
	}

	img, err := newAndroidSparseImage(data)
	if err != nil {
		return err
	}

	return img.forEachEncodedPiece(maxDownloadSize, fn)
}

func forEachRawFileFlashData(r io.ReaderAt, totalLength uint64, maxDownloadSize uint64, fn func(index int, payload []byte) error) error {
	return forEachRawFileFlashStream(r, totalLength, maxDownloadSize, func(index int, size uint64, reader io.Reader) error {
		payload, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		if uint64(len(payload)) != size {
			return fmt.Errorf("streamed sparse chunk %d size mismatch: got %d want %d", index, len(payload), size)
		}
		return fn(index, payload)
	})
}

func forEachRawFileFlashStream(r io.ReaderAt, totalLength uint64, maxDownloadSize uint64, fn func(index int, size uint64, reader io.Reader) error) error {
	if totalLength == 0 {
		return fn(1, 0, bytes.NewReader(nil))
	}

	img := androidSparseImage{
		blockSize:   androidSparseDefaultBlockSize,
		totalLength: totalLength,
	}

	startBlock := uint32(0)
	fileOffset := uint64(0)
	remainingBytes := totalLength
	pieceIndex := 0

	for remainingBytes > 0 {
		blocks, err := img.maxRawChunkBlocks(startBlock, remainingBytes, maxDownloadSize)
		if err != nil {
			return err
		}

		readLength := minUint64(remainingBytes, uint64(blocks)*uint64(img.blockSize))
		reader, size, err := img.rawSparsePieceReader(r, int64(fileOffset), startBlock, readLength)
		if err != nil {
			return err
		}

		pieceIndex++
		if err := fn(pieceIndex, size, reader); err != nil {
			return err
		}

		startBlock += blocks
		fileOffset += readLength
		remainingBytes -= readLength
	}

	return nil
}

func newAndroidSparseImage(data []byte) (*androidSparseImage, error) {
	if isAndroidSparse(data) {
		return parseAndroidSparseImage(data)
	}
	img := &androidSparseImage{
		blockSize:   androidSparseDefaultBlockSize,
		totalLength: uint64(len(data)),
	}
	if len(data) > 0 {
		img.chunks = []androidSparseChunk{
			{
				chunkType: androidSparseChunkRaw,
				block:     0,
				length:    uint64(len(data)),
				data:      data,
			},
		}
	}
	return img, nil
}

func isAndroidSparse(data []byte) bool {
	return len(data) >= 4 && binary.LittleEndian.Uint32(data[:4]) == androidSparseMagic
}

func parseAndroidSparseImage(data []byte) (*androidSparseImage, error) {
	img, err := parseAndroidSparseImageReaderAt(bytes.NewReader(data), uint64(len(data)))
	if err != nil {
		return nil, err
	}

	for i := range img.chunks {
		if img.chunks[i].chunkType != androidSparseChunkRaw {
			continue
		}

		start := int(img.chunks[i].dataOffset)
		end := start + int(img.chunks[i].length)
		rawData := make([]byte, end-start)
		copy(rawData, data[start:end])
		img.chunks[i].data = rawData
		img.chunks[i].dataOffset = -1
	}

	return img, nil
}

func parseAndroidSparseImageReaderAt(r io.ReaderAt, totalSize uint64) (*androidSparseImage, error) {
	if totalSize < uint64(androidSparseFileHeaderSize) {
		return nil, fmt.Errorf("invalid sparse image: file too small")
	}

	var header androidSparseHeader
	headerBytes, err := readAt(r, 0, int(androidSparseFileHeaderSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read sparse header: %w", err)
	}
	if err := binary.Read(bytes.NewReader(headerBytes), binary.LittleEndian, &header); err != nil {
		return nil, fmt.Errorf("failed to read sparse header: %w", err)
	}

	switch {
	case header.Magic != androidSparseMagic:
		return nil, fmt.Errorf("invalid sparse image magic: %#x", header.Magic)
	case header.MajorVersion > androidSparseMajorVersion:
		return nil, fmt.Errorf("unsupported sparse major version: %d", header.MajorVersion)
	case header.FileHeaderSize < androidSparseFileHeaderSize:
		return nil, fmt.Errorf("invalid sparse file header size: %d", header.FileHeaderSize)
	case header.ChunkHeaderSize < androidSparseChunkHeaderSize:
		return nil, fmt.Errorf("invalid sparse chunk header size: %d", header.ChunkHeaderSize)
	case header.BlockSize == 0 || header.BlockSize%4 != 0:
		return nil, fmt.Errorf("invalid sparse block size: %d", header.BlockSize)
	}

	offset := int(header.FileHeaderSize)
	if uint64(offset) > totalSize {
		return nil, fmt.Errorf("invalid sparse file header size: %d", header.FileHeaderSize)
	}

	chunks := make([]androidSparseChunk, 0, int(header.TotalChunks))
	var currentBlock uint32

	for i := uint32(0); i < header.TotalChunks; i++ {
		if uint64(offset+int(header.ChunkHeaderSize)) > totalSize {
			return nil, fmt.Errorf("invalid sparse chunk header at index %d", i)
		}

		var chunkHeader androidSparseChunkHeader
		chunkHeaderBytes, err := readAt(r, int64(offset), int(androidSparseChunkHeaderSize))
		if err != nil {
			return nil, fmt.Errorf("failed to read sparse chunk header %d: %w", i, err)
		}
		if err := binary.Read(bytes.NewReader(chunkHeaderBytes), binary.LittleEndian, &chunkHeader); err != nil {
			return nil, fmt.Errorf("failed to read sparse chunk header %d: %w", i, err)
		}
		offset += int(header.ChunkHeaderSize)

		payloadSize := int(chunkHeader.TotalSize) - int(header.ChunkHeaderSize)
		if payloadSize < 0 || uint64(offset+payloadSize) > totalSize {
			return nil, fmt.Errorf("invalid sparse chunk payload at index %d", i)
		}

		chunkLength := uint64(chunkHeader.ChunkSize) * uint64(header.BlockSize)

		switch chunkHeader.ChunkType {
		case androidSparseChunkRaw:
			expectedSize := uint32(header.ChunkHeaderSize) + uint32(chunkLength)
			if chunkHeader.TotalSize != expectedSize {
				return nil, fmt.Errorf("invalid raw sparse chunk size at index %d", i)
			}

			chunks = append(chunks, androidSparseChunk{
				chunkType:  androidSparseChunkRaw,
				block:      currentBlock,
				length:     chunkLength,
				dataOffset: int64(offset),
			})
			currentBlock += chunkHeader.ChunkSize
		case androidSparseChunkFill:
			if chunkHeader.TotalSize != uint32(header.ChunkHeaderSize)+4 {
				return nil, fmt.Errorf("invalid fill sparse chunk size at index %d", i)
			}
			fillBytes, err := readAt(r, int64(offset), 4)
			if err != nil {
				return nil, fmt.Errorf("failed to read sparse fill chunk %d: %w", i, err)
			}
			chunks = append(chunks, androidSparseChunk{
				chunkType: androidSparseChunkFill,
				block:     currentBlock,
				length:    chunkLength,
				fillValue: binary.LittleEndian.Uint32(fillBytes),
			})
			currentBlock += chunkHeader.ChunkSize
		case androidSparseChunkDontCare:
			if chunkHeader.TotalSize != uint32(header.ChunkHeaderSize) {
				return nil, fmt.Errorf("invalid don't-care sparse chunk size at index %d", i)
			}
			currentBlock += chunkHeader.ChunkSize
		case androidSparseChunkCRC32:
			if chunkHeader.TotalSize != uint32(header.ChunkHeaderSize)+4 {
				return nil, fmt.Errorf("invalid crc32 sparse chunk size at index %d", i)
			}
		default:
			return nil, fmt.Errorf("unsupported sparse chunk type %#x at index %d", chunkHeader.ChunkType, i)
		}

		offset += payloadSize
	}

	if currentBlock > header.TotalBlocks {
		return nil, fmt.Errorf("invalid sparse image: blocks exceed total size")
	}

	return &androidSparseImage{
		blockSize:   header.BlockSize,
		totalLength: uint64(header.TotalBlocks) * uint64(header.BlockSize),
		chunks:      chunks,
	}, nil
}

func (img *androidSparseImage) forEachEncodedPiece(maxDownloadSize uint64, fn func(index int, payload []byte) error) error {
	if img.encodedSize(img.chunks) <= maxDownloadSize {
		part, err := img.encode(img.chunks)
		if err != nil {
			return err
		}
		return fn(1, part)
	}

	remaining := append([]androidSparseChunk(nil), img.chunks...)
	pieceIndex := 0

	for len(remaining) > 0 {
		piece, next, err := img.takeNextPiece(remaining, maxDownloadSize)
		if err != nil {
			return err
		}

		part, err := img.encode(piece)
		if err != nil {
			return err
		}

		pieceIndex++
		if err := fn(pieceIndex, part); err != nil {
			return err
		}

		remaining = next
	}

	return nil
}

func (img *androidSparseImage) maxRawChunkBlocks(startBlock uint32, remainingBytes uint64, maxDownloadSize uint64) (uint32, error) {
	totalBlocks := img.totalBlocks()
	if uint64(startBlock) >= totalBlocks {
		return 0, fmt.Errorf("raw chunk start block %d exceeds image size", startBlock)
	}

	remainingBlocks := uint32(totalBlocks - uint64(startBlock))
	var best uint32
	low, high := uint32(1), remainingBlocks

	for low <= high {
		mid := low + (high-low)/2
		candidateLength := minUint64(remainingBytes, uint64(mid)*uint64(img.blockSize))
		candidate := androidSparseChunk{
			chunkType: androidSparseChunkRaw,
			block:     startBlock,
			length:    candidateLength,
		}

		if img.encodedSize([]androidSparseChunk{candidate}) <= maxDownloadSize {
			best = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	if best == 0 {
		return 0, fmt.Errorf("raw chunk at block %d cannot fit within %d bytes", startBlock, maxDownloadSize)
	}

	return best, nil
}

func (img *androidSparseImage) forEachEncodedPieceStream(source io.ReaderAt, maxDownloadSize uint64, fn func(index int, size uint64, reader io.Reader) error) error {
	if img.encodedSize(img.chunks) <= maxDownloadSize {
		reader, size, err := img.pieceReaderFromChunks(source, img.chunks)
		if err != nil {
			return err
		}
		return fn(1, size, reader)
	}

	remaining := append([]androidSparseChunk(nil), img.chunks...)
	pieceIndex := 0

	for len(remaining) > 0 {
		piece, next, err := img.takeNextPiece(remaining, maxDownloadSize)
		if err != nil {
			return err
		}

		reader, size, err := img.pieceReaderFromChunks(source, piece)
		if err != nil {
			return err
		}

		pieceIndex++
		if err := fn(pieceIndex, size, reader); err != nil {
			return err
		}

		remaining = next
	}

	return nil
}

func (img *androidSparseImage) rawSparsePieceReader(r io.ReaderAt, dataOffset int64, startBlock uint32, rawLength uint64) (io.Reader, uint64, error) {
	chunk := androidSparseChunk{
		chunkType: androidSparseChunkRaw,
		block:     startBlock,
		length:    rawLength,
	}

	header, err := img.sparseFileHeaderBytes([]androidSparseChunk{chunk})
	if err != nil {
		return nil, 0, err
	}

	readers := []io.Reader{bytes.NewReader(header)}

	if startBlock > 0 {
		skipChunk, err := sparseSkipChunkBytes(startBlock)
		if err != nil {
			return nil, 0, err
		}
		readers = append(readers, bytes.NewReader(skipChunk))
	}

	rawChunkHeader, padding, err := rawSparseChunkHeaderBytes(rawLength, img.blockSize)
	if err != nil {
		return nil, 0, err
	}
	readers = append(readers, bytes.NewReader(rawChunkHeader))
	readers = append(readers, io.NewSectionReader(r, dataOffset, int64(rawLength)))
	if padding > 0 {
		readers = append(readers, bytes.NewReader(make([]byte, padding)))
	}

	lastBlock := startBlock + img.chunkBlocks(chunk)
	if uint64(lastBlock) < img.totalBlocks() {
		skipChunk, err := sparseSkipChunkBytes(uint32(img.totalBlocks() - uint64(lastBlock)))
		if err != nil {
			return nil, 0, err
		}
		readers = append(readers, bytes.NewReader(skipChunk))
	}

	return io.MultiReader(readers...), img.encodedSize([]androidSparseChunk{chunk}), nil
}

func (img *androidSparseImage) pieceReaderFromChunks(source io.ReaderAt, chunks []androidSparseChunk) (io.Reader, uint64, error) {
	header, err := img.sparseFileHeaderBytes(chunks)
	if err != nil {
		return nil, 0, err
	}

	readers := []io.Reader{bytes.NewReader(header)}
	var lastBlock uint32

	for _, chunk := range chunks {
		if chunk.block > lastBlock {
			skipChunk, err := sparseSkipChunkBytes(chunk.block - lastBlock)
			if err != nil {
				return nil, 0, err
			}
			readers = append(readers, bytes.NewReader(skipChunk))
		}

		chunkReaders, err := img.chunkReaders(source, chunk)
		if err != nil {
			return nil, 0, err
		}
		readers = append(readers, chunkReaders...)
		lastBlock = chunk.block + img.chunkBlocks(chunk)
	}

	totalBlocks := img.totalBlocks()
	if uint64(lastBlock) < totalBlocks {
		skipChunk, err := sparseSkipChunkBytes(uint32(totalBlocks - uint64(lastBlock)))
		if err != nil {
			return nil, 0, err
		}
		readers = append(readers, bytes.NewReader(skipChunk))
	}

	return io.MultiReader(readers...), img.encodedSize(chunks), nil
}

func (img *androidSparseImage) chunkReaders(source io.ReaderAt, chunk androidSparseChunk) ([]io.Reader, error) {
	switch chunk.chunkType {
	case androidSparseChunkRaw:
		header, padding, err := rawSparseChunkHeaderBytes(chunk.length, img.blockSize)
		if err != nil {
			return nil, err
		}

		readers := []io.Reader{bytes.NewReader(header)}
		switch {
		case len(chunk.data) > 0:
			readers = append(readers, bytes.NewReader(chunk.data))
		case source != nil && chunk.dataOffset >= 0:
			readers = append(readers, io.NewSectionReader(source, chunk.dataOffset, int64(chunk.length)))
		default:
			return nil, fmt.Errorf("missing raw sparse chunk data at block %d", chunk.block)
		}
		if padding > 0 {
			readers = append(readers, bytes.NewReader(make([]byte, padding)))
		}
		return readers, nil
	case androidSparseChunkFill:
		fillChunk, err := sparseFillChunkBytes(chunk.length, chunk.fillValue, img.blockSize)
		if err != nil {
			return nil, err
		}
		return []io.Reader{bytes.NewReader(fillChunk)}, nil
	default:
		return nil, fmt.Errorf("unsupported sparse chunk type %#x", chunk.chunkType)
	}
}

func (img *androidSparseImage) takeNextPiece(chunks []androidSparseChunk, maxDownloadSize uint64) ([]androidSparseChunk, []androidSparseChunk, error) {
	piece := make([]androidSparseChunk, 0, 1)
	remaining := append([]androidSparseChunk(nil), chunks...)

	for len(remaining) > 0 {
		candidate := append(append([]androidSparseChunk(nil), piece...), remaining[0])
		if img.encodedSize(candidate) <= maxDownloadSize {
			piece = append(piece, remaining[0])
			remaining = remaining[1:]
			continue
		}

		if len(piece) > 0 {
			break
		}

		prefix, suffix, err := img.splitChunkToFit(remaining[0], maxDownloadSize)
		if err != nil {
			return nil, nil, err
		}

		piece = append(piece, prefix)
		if suffix.length > 0 {
			remaining[0] = suffix
		} else {
			remaining = remaining[1:]
		}
		break
	}

	if len(piece) == 0 {
		return nil, nil, fmt.Errorf("unable to fit sparse data into %d bytes", maxDownloadSize)
	}

	return piece, remaining, nil
}

func (img *androidSparseImage) splitChunkToFit(chunk androidSparseChunk, maxDownloadSize uint64) (androidSparseChunk, androidSparseChunk, error) {
	totalBlocks := img.chunkBlocks(chunk)
	if totalBlocks <= 1 {
		return androidSparseChunk{}, androidSparseChunk{}, fmt.Errorf("chunk at block %d cannot fit within %d bytes", chunk.block, maxDownloadSize)
	}

	var best uint32
	low, high := uint32(1), totalBlocks-1
	for low <= high {
		mid := low + (high-low)/2
		prefix := img.chunkPrefix(chunk, mid)
		if img.encodedSize([]androidSparseChunk{prefix}) <= maxDownloadSize {
			best = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	if best == 0 {
		return androidSparseChunk{}, androidSparseChunk{}, fmt.Errorf("chunk at block %d cannot be split small enough for %d bytes", chunk.block, maxDownloadSize)
	}

	return img.chunkPrefix(chunk, best), img.chunkSuffix(chunk, best), nil
}

func (img *androidSparseImage) encode(chunks []androidSparseChunk) ([]byte, error) {
	totalBlocks := img.totalBlocks()
	buf := bytes.NewBuffer(make([]byte, 0, int(img.encodedSize(chunks))))

	header, err := img.sparseFileHeaderBytes(chunks)
	if err != nil {
		return nil, err
	}
	if _, err := buf.Write(header); err != nil {
		return nil, fmt.Errorf("failed to write sparse header: %w", err)
	}

	var lastBlock uint32
	for _, chunk := range chunks {
		if chunk.block > lastBlock {
			if err := writeSparseSkipChunk(buf, chunk.block-lastBlock); err != nil {
				return nil, err
			}
		}

		if err := writeSparseDataChunk(buf, chunk, img.blockSize); err != nil {
			return nil, err
		}
		lastBlock = chunk.block + img.chunkBlocks(chunk)
	}

	if uint64(lastBlock) < totalBlocks {
		if err := writeSparseSkipChunk(buf, uint32(totalBlocks-uint64(lastBlock))); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func (img *androidSparseImage) encodedSize(chunks []androidSparseChunk) uint64 {
	size := uint64(androidSparseFileHeaderSize)
	totalBlocks := img.totalBlocks()
	var lastBlock uint32

	for _, chunk := range chunks {
		if chunk.block > lastBlock {
			size += uint64(androidSparseChunkHeaderSize)
		}

		size += img.chunkEncodedSize(chunk)
		lastBlock = chunk.block + img.chunkBlocks(chunk)
	}

	if uint64(lastBlock) < totalBlocks {
		size += uint64(androidSparseChunkHeaderSize)
	}

	return size
}

func (img *androidSparseImage) sparseChunkCount(chunks []androidSparseChunk) int {
	count := 0
	totalBlocks := img.totalBlocks()
	var lastBlock uint32

	for _, chunk := range chunks {
		if chunk.block > lastBlock {
			count++
		}
		count++
		lastBlock = chunk.block + img.chunkBlocks(chunk)
	}

	if uint64(lastBlock) < totalBlocks {
		count++
	}

	return count
}

func (img *androidSparseImage) chunkEncodedSize(chunk androidSparseChunk) uint64 {
	switch chunk.chunkType {
	case androidSparseChunkRaw:
		return uint64(androidSparseChunkHeaderSize) + alignUp(chunk.length, uint64(img.blockSize))
	case androidSparseChunkFill:
		return uint64(androidSparseChunkHeaderSize) + 4
	default:
		return uint64(androidSparseChunkHeaderSize)
	}
}

func (img *androidSparseImage) totalBlocks() uint64 {
	return divRoundUp(img.totalLength, uint64(img.blockSize))
}

func (img *androidSparseImage) chunkBlocks(chunk androidSparseChunk) uint32 {
	return uint32(divRoundUp(chunk.length, uint64(img.blockSize)))
}

func (img *androidSparseImage) chunkPrefix(chunk androidSparseChunk, blocks uint32) androidSparseChunk {
	if blocks >= img.chunkBlocks(chunk) {
		return chunk
	}

	length := uint64(blocks) * uint64(img.blockSize)
	prefix := chunk
	prefix.length = length
	if chunk.chunkType == androidSparseChunkRaw && len(chunk.data) > 0 {
		prefix.data = chunk.data[:int(length)]
	}

	return prefix
}

func (img *androidSparseImage) chunkSuffix(chunk androidSparseChunk, blocks uint32) androidSparseChunk {
	totalBlocks := img.chunkBlocks(chunk)
	if blocks >= totalBlocks {
		return androidSparseChunk{}
	}

	suffix := chunk
	suffix.block = chunk.block + blocks
	if chunk.chunkType == androidSparseChunkRaw {
		offset := uint64(blocks) * uint64(img.blockSize)
		if len(chunk.data) > 0 {
			suffix.data = chunk.data[int(offset):]
			suffix.length = uint64(len(suffix.data))
		} else {
			suffix.dataOffset = chunk.dataOffset + int64(offset)
			suffix.length = chunk.length - offset
		}
	} else {
		suffix.length = uint64(totalBlocks-blocks) * uint64(img.blockSize)
	}

	return suffix
}

func writeSparseSkipChunk(buf *bytes.Buffer, blocks uint32) error {
	header := androidSparseChunkHeader{
		ChunkType: androidSparseChunkDontCare,
		ChunkSize: blocks,
		TotalSize: uint32(androidSparseChunkHeaderSize),
	}
	if err := writeSparseChunkHeader(buf, header, "skip"); err != nil {
		return err
	}
	return nil
}

func writeSparseDataChunk(buf *bytes.Buffer, chunk androidSparseChunk, blockSize uint32) error {
	switch chunk.chunkType {
	case androidSparseChunkRaw:
		alignedLength := alignUp(chunk.length, uint64(blockSize))
		header := androidSparseChunkHeader{
			ChunkType: androidSparseChunkRaw,
			ChunkSize: uint32(alignedLength / uint64(blockSize)),
			TotalSize: uint32(uint64(androidSparseChunkHeaderSize) + alignedLength),
		}
		if err := writeSparseChunkHeader(buf, header, "raw"); err != nil {
			return err
		}
		if _, err := buf.Write(chunk.data); err != nil {
			return fmt.Errorf("failed to write sparse raw chunk data: %w", err)
		}
		padding := int(alignedLength - chunk.length)
		if padding > 0 {
			if _, err := buf.Write(make([]byte, padding)); err != nil {
				return fmt.Errorf("failed to write sparse raw chunk padding: %w", err)
			}
		}
	case androidSparseChunkFill:
		header := androidSparseChunkHeader{
			ChunkType: androidSparseChunkFill,
			ChunkSize: uint32(divRoundUp(chunk.length, uint64(blockSize))),
			TotalSize: uint32(androidSparseChunkHeaderSize + 4),
		}
		if err := writeSparseChunkHeader(buf, header, "fill"); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.LittleEndian, chunk.fillValue); err != nil {
			return fmt.Errorf("failed to write sparse fill chunk data: %w", err)
		}
	default:
		return fmt.Errorf("unsupported sparse chunk type %#x", chunk.chunkType)
	}

	return nil
}

func (img *androidSparseImage) sparseFileHeaderBytes(chunks []androidSparseChunk) ([]byte, error) {
	totalBlocks := img.totalBlocks()
	if totalBlocks > uint64(^uint32(0)) {
		return nil, fmt.Errorf("sparse image too large: %d blocks", totalBlocks)
	}

	header := androidSparseHeader{
		Magic:           androidSparseMagic,
		MajorVersion:    androidSparseMajorVersion,
		MinorVersion:    androidSparseMinorVersion,
		FileHeaderSize:  androidSparseFileHeaderSize,
		ChunkHeaderSize: androidSparseChunkHeaderSize,
		BlockSize:       img.blockSize,
		TotalBlocks:     uint32(totalBlocks),
		TotalChunks:     uint32(img.sparseChunkCount(chunks)),
		ImageChecksum:   0,
	}

	buf := bytes.NewBuffer(make([]byte, 0, androidSparseFileHeaderSize))
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		return nil, fmt.Errorf("failed to marshal sparse header: %w", err)
	}

	return buf.Bytes(), nil
}

func sparseSkipChunkBytes(blocks uint32) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, androidSparseChunkHeaderSize))
	if err := writeSparseSkipChunk(buf, blocks); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sparseFillChunkBytes(length uint64, fillValue uint32, blockSize uint32) ([]byte, error) {
	header := androidSparseChunkHeader{
		ChunkType: androidSparseChunkFill,
		ChunkSize: uint32(divRoundUp(length, uint64(blockSize))),
		TotalSize: uint32(androidSparseChunkHeaderSize + 4),
	}

	buf := bytes.NewBuffer(make([]byte, 0, androidSparseChunkHeaderSize+4))
	if err := writeSparseChunkHeader(buf, header, "fill"); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, fillValue); err != nil {
		return nil, fmt.Errorf("failed to write sparse fill chunk data: %w", err)
	}

	return buf.Bytes(), nil
}

func rawSparseChunkHeaderBytes(rawLength uint64, blockSize uint32) ([]byte, int, error) {
	alignedLength := alignUp(rawLength, uint64(blockSize))
	header := androidSparseChunkHeader{
		ChunkType: androidSparseChunkRaw,
		ChunkSize: uint32(alignedLength / uint64(blockSize)),
		TotalSize: uint32(uint64(androidSparseChunkHeaderSize) + alignedLength),
	}

	buf := bytes.NewBuffer(make([]byte, 0, androidSparseChunkHeaderSize))
	if err := writeSparseChunkHeader(buf, header, "raw"); err != nil {
		return nil, 0, err
	}

	return buf.Bytes(), int(alignedLength - rawLength), nil
}

func writeSparseChunkHeader(buf *bytes.Buffer, header androidSparseChunkHeader, chunkKind string) error {
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		return fmt.Errorf("failed to write sparse %s chunk header: %w", chunkKind, err)
	}
	return nil
}

func readAt(r io.ReaderAt, offset int64, size int) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(r, offset, int64(size)), buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func alignUp(value, alignment uint64) uint64 {
	if alignment == 0 {
		return value
	}
	remainder := value % alignment
	if remainder == 0 {
		return value
	}
	return value + alignment - remainder
}

func divRoundUp(value, divisor uint64) uint64 {
	if value == 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func parseMaxDownloadSize(value string) (uint64, error) {
	size, err := strconv.ParseUint(strings.TrimSpace(value), 0, 64)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		return 0, fmt.Errorf("max-download-size must be greater than zero")
	}
	return size, nil
}
