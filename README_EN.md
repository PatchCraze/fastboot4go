# fastboot4go

[中文](README.md) | [English](README_EN.md)

`fastboot4go` is a Go-based fastboot USB implementation. This repository provides the `gofastboot` command-line tool, which can perform common operations on Android devices already in fastboot mode.

The current CLI supports:

- Listing fastboot devices
- Querying device variables (`getvar`)
- Erasing partitions (`erase`)
- Running OEM commands (`oem`)
- Flashing images (`flash`)

## Requirements

- Go 1.22 or later
- System `libusb`
- The device is already in fastboot mode
- The current user has permission to access USB devices

## Build

```bash
go build -o gofastboot ./cmd/gofastboot
```

You can also run it directly:

```bash
go run ./cmd/gofastboot --help
```

Notes:

- `--help` shows the global flags, currently mainly `-serial`
- Running without arguments, or with invalid subcommand arguments, prints the full command usage

## Command Overview

```text
gofastboot devices
gofastboot [-serial SERIAL] getvar <name>
gofastboot [-serial SERIAL] erase <partition>
gofastboot [-serial SERIAL] oem <command> [args...]
gofastboot [-serial SERIAL] flash <partition> <image>
```

Global flags:

- `-serial SERIAL`: selects the target fastboot device by serial number

Device selection rules:

- If `-serial` is not specified and exactly one device is connected, the tool selects it automatically
- If `-serial` is not specified and no device is found, the tool returns `no fastboot device found`
- If `-serial` is not specified and multiple devices are found, the tool returns `multiple fastboot devices found, use -serial to choose one`

## Command Usage

### `devices`

Lists devices currently in fastboot mode.

```bash
./gofastboot devices
```

Example output:

```text
ABCDEF012345	fastboot
```

Typical use cases:

- Verifying that the USB connection works
- Getting the serial number needed for `-serial`

### `getvar <name>`

Reads a fastboot variable.

```bash
./gofastboot getvar product
./gofastboot getvar current-slot
./gofastboot -serial ABCDEF012345 getvar max-download-size
```

Example output:

```text
product: panther
```

To read every variable supported by the device, use:

```bash
./gofastboot getvar all
```

`getvar all` prints all returned lines from the device.

Common variable examples:

- `product`
- `current-slot`
- `max-download-size`
- `has-slot:boot`

### `erase <partition>`

Erases the specified partition.

```bash
./gofastboot erase userdata
./gofastboot -serial ABCDEF012345 erase metadata
```

Successful output:

```text
erased userdata
```

Notes:

- For A/B-capable partitions, the tool tries to resolve the target partition automatically from the current slot
- For example, passing `boot` may end up operating on `boot_a` or `boot_b`

### `oem <command> [args...]`

Sends an OEM fastboot command to the device.

```bash
./gofastboot oem device-info
./gofastboot oem unlock
./gofastboot oem off-mode-charge 0
```

Successful output looks like:

```text
completed oem device-info
```

Notes:

- Everything after `oem` is joined with spaces and sent to the device
- Supported OEM commands depend on the device vendor's bootloader implementation

### `flash <partition> <image>`

Flashes an image to the specified partition.

```bash
./gofastboot flash boot boot.img
./gofastboot flash vendor_boot vendor_boot.img
./gofastboot -serial ABCDEF012345 flash super super.img
```

Successful output looks like:

```text
flashed boot from boot.img
```

Notes:

- When a regular partition name is provided, the tool first tries to resolve the current slot automatically
- For example, `flash boot boot.img` may actually write to `boot_a` or `boot_b` on an A/B device
- Progress information is written to stderr, which is convenient for direct terminal use

## Automatic `flash` Behavior

`flash` is the most complex command in this tool. The README recommends understanding these points first:

### 1. Automatically reads device `max-download-size`

The tool first reads `max-download-size` from the device to determine the largest amount of data that can be sent in a single download.

- If the device does not return this variable, a default threshold is used
- The current implementation caps each single download at no more than `512 MiB`

### 2. Large images are automatically converted to sparse format or split

When the image is larger than the single-download limit, the tool does not fail immediately. Instead, it handles the image automatically:

- If the input is a regular raw image, it is converted into Android sparse chunks and flashed in parts
- If the input is already an Android sparse image but one file is still too large, it is re-split before flashing

This means large files such as `super.img` or large `system.img` images can be flashed directly with `flash`.

### 3. Streaming for large files

When flashing large files, the tool tries to use streaming reads instead of loading the full image into memory at once.

## Common Workflows

### Inspect the device and read key information

```bash
./gofastboot devices
./gofastboot getvar product
./gofastboot getvar current-slot
./gofastboot getvar max-download-size
```

### Flash a specific connected device

```bash
./gofastboot devices
./gofastboot -serial ABCDEF012345 flash boot boot.img
```

### Flash the boot image for the current slot

```bash
./gofastboot getvar current-slot
./gofastboot flash boot boot.img
```

If the device supports slots, the program automatically resolves this to `boot_a` or `boot_b` based on `current-slot`.

## Troubleshooting

- Device not visible: run `./gofastboot devices` first and verify the device is in fastboot mode with correct USB permissions
- Multiple-device error: add `-serial SERIAL`
- `getvar` failed: verify that the bootloader supports the requested variable
- OEM command failed: many `oem` subcommands are vendor-specific and require device support
- Flash failed: first check the partition name, image file path, bootloader lock state, and whether the device allows writes to that partition
