package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/timoxa0/gofastboot/fastboot"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gofastboot", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	serial := fs.String("serial", "", "fastboot serial number")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() == 0 {
		return usageError()
	}

	switch fs.Arg(0) {
	case "devices":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: gofastboot devices")
		}

		return listDevices(os.Stdout)
	case "getvar":
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: gofastboot [-serial SERIAL] getvar <name>")
		}

		dev, err := openDevice(*serial)
		if err != nil {
			return err
		}
		defer dev.Close()

		name := fs.Arg(1)
		if name == "all" {
			return printGetVarAll(dev, os.Stdout)
		}

		value, err := dev.GetVar(name)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "%s: %s\n", name, value)
		return nil
	case "erase":
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: gofastboot [-serial SERIAL] erase <partition>")
		}

		dev, err := openDevice(*serial)
		if err != nil {
			return err
		}
		defer dev.Close()
		dev.Progress = os.Stderr

		partition := fs.Arg(1)
		if err := dev.Erase(partition); err != nil {
			return err
		}

		fmt.Printf("erased %s\n", partition)
		return nil
	case "oem":
		if fs.NArg() < 2 {
			return fmt.Errorf("usage: gofastboot [-serial SERIAL] oem <command> [args...]")
		}

		dev, err := openDevice(*serial)
		if err != nil {
			return err
		}
		defer dev.Close()
		dev.Progress = os.Stderr

		oemArgs := fs.Args()[1:]
		if err := dev.OEM(oemArgs...); err != nil {
			return err
		}

		fmt.Printf("completed oem %s\n", strings.Join(oemArgs, " "))
		return nil
	case "flash":
		if fs.NArg() != 3 {
			return fmt.Errorf("usage: gofastboot [-serial SERIAL] flash <partition> <image>")
		}

		dev, err := openDevice(*serial)
		if err != nil {
			return err
		}
		defer dev.Close()
		dev.Progress = os.Stderr

		partition := fs.Arg(1)
		imagePath := fs.Arg(2)
		if err := dev.FlashFile(partition, imagePath); err != nil {
			return err
		}

		fmt.Printf("flashed %s from %s\n", partition, filepath.Base(imagePath))
		return nil
	default:
		return usageError()
	}
}

func listDevices(out *os.File) error {
	devs, err := fastboot.FindDevices()
	if err != nil {
		return err
	}
	defer closeDevices(devs)

	for _, dev := range devs {
		serial, err := dev.Device.SerialNumber()
		if err != nil || serial == "" {
			serial = "<unknown>"
		}
		fmt.Fprintf(out, "%s\tfastboot\n", serial)
	}

	return nil
}

func printGetVarAll(dev *fastboot.FastbootDevice, out *os.File) error {
	lines, err := dev.GetVarAll()
	if err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	return nil
}

func openDevice(serial string) (*fastboot.FastbootDevice, error) {
	if serial != "" {
		dev, err := fastboot.FindDevice(serial)
		if err != nil {
			return nil, fmt.Errorf("failed to find device %q: %w", serial, err)
		}
		return dev, nil
	}

	devs, err := fastboot.FindDevices()
	if err != nil {
		return nil, err
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("no fastboot device found")
	}
	if len(devs) == 1 {
		return devs[0], nil
	}

	return nil, fmt.Errorf("multiple fastboot devices found, use -serial to choose one")
}

func closeDevices(devs []*fastboot.FastbootDevice) {
	closedContexts := make(map[string]struct{}, len(devs))
	for _, dev := range devs {
		if dev == nil {
			continue
		}
		if dev.Unclaim != nil {
			dev.Unclaim()
		}
		if dev.Device != nil {
			_ = dev.Device.Close()
		}
		if dev.Context == nil {
			continue
		}

		key := fmt.Sprintf("%p", dev.Context)
		if _, ok := closedContexts[key]; ok {
			continue
		}
		_ = dev.Context.Close()
		closedContexts[key] = struct{}{}
	}
}

func usageError() error {
	return fmt.Errorf("usage:\n  gofastboot devices\n  gofastboot [-serial SERIAL] getvar <name>\n  gofastboot [-serial SERIAL] erase <partition>\n  gofastboot [-serial SERIAL] oem <command> [args...]\n  gofastboot [-serial SERIAL] flash <partition> <image>")
}
