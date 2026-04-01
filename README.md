# fastboot4go

`fastboot4go` 是一个基于 Go 的 fastboot USB 实现，仓库里提供了命令行工具 `gofastboot`，可直接对处于 fastboot 模式的 Android 设备执行常见操作。

当前 CLI 支持：

- 列出 fastboot 设备
- 查询设备变量（`getvar`）
- 擦除分区（`erase`）
- 执行 OEM 命令（`oem`）
- 刷写镜像（`flash`）

## 环境要求

- Go 1.22 或更高版本
- 系统可用的 `libusb`
- 设备已经进入 fastboot 模式
- 当前用户对 USB 设备有访问权限

## 构建

```bash
go build -o gofastboot ./cmd/gofastboot
```

也可以直接运行：

```bash
go run ./cmd/gofastboot --help
```

说明：

- `--help` 会显示全局参数（当前主要是 `-serial`）
- 不带参数运行，或子命令参数不正确时，会输出完整命令用法

## 命令总览

```text
gofastboot devices
gofastboot [-serial SERIAL] getvar <name>
gofastboot [-serial SERIAL] erase <partition>
gofastboot [-serial SERIAL] oem <command> [args...]
gofastboot [-serial SERIAL] flash <partition> <image>
```

全局参数：

- `-serial SERIAL`：指定要操作的 fastboot 设备序列号

设备选择规则：

- 未指定 `-serial` 且只连接了一台设备时，工具会自动选中该设备
- 未指定 `-serial` 且没有检测到设备时，会报错 `no fastboot device found`
- 未指定 `-serial` 且检测到多台设备时，会报错 `multiple fastboot devices found, use -serial to choose one`

## 命令用法

### `devices`

列出当前处于 fastboot 模式的设备。

```bash
./gofastboot devices
```

示例输出：

```text
ABCDEF012345	fastboot
```

适用场景：

- 确认 USB 连接是否正常
- 获取 `-serial` 需要使用的序列号

### `getvar <name>`

读取 fastboot 变量。

```bash
./gofastboot getvar product
./gofastboot getvar current-slot
./gofastboot -serial ABCDEF012345 getvar max-download-size
```

示例输出：

```text
product: panther
```

如果要读取设备支持的全部变量，可使用：

```bash
./gofastboot getvar all
```

`getvar all` 会把设备返回的全部信息逐行打印出来。

常见变量示例：

- `product`
- `current-slot`
- `max-download-size`
- `has-slot:boot`

### `erase <partition>`

擦除指定分区。

```bash
./gofastboot erase userdata
./gofastboot -serial ABCDEF012345 erase metadata
```

成功时输出：

```text
erased userdata
```

说明：

- 对支持 A/B 的分区，工具会尝试根据设备当前 slot 自动解析目标分区
- 例如传入 `boot`，实际可能执行到 `boot_a` 或 `boot_b`

### `oem <command> [args...]`

向设备发送 OEM fastboot 命令。

```bash
./gofastboot oem device-info
./gofastboot oem unlock
./gofastboot oem off-mode-charge 0
```

成功时输出类似：

```text
completed oem device-info
```

说明：

- `oem` 后面的内容会按空格拼接后发送给设备
- 具体支持哪些 OEM 命令取决于设备厂商 bootloader

### `flash <partition> <image>`

把镜像刷写到指定分区。

```bash
./gofastboot flash boot boot.img
./gofastboot flash vendor_boot vendor_boot.img
./gofastboot -serial ABCDEF012345 flash super super.img
```

成功时输出类似：

```text
flashed boot from boot.img
```

说明：

- 传入的是普通分区名时，工具会优先尝试自动解析当前 slot
- 例如 `flash boot boot.img` 在 A/B 设备上可能实际写入 `boot_a` 或 `boot_b`
- 进度信息输出到标准错误（stderr），适合在终端直接观察

## `flash` 的自动处理行为

`flash` 是这个工具里行为最复杂的命令，README 建议提前了解这几个点：

### 1. 自动读取设备 `max-download-size`

工具会先读取设备的 `max-download-size`，决定单次下载能发送的最大数据量。

- 如果设备没有返回这个变量，会使用默认阈值
- 当前实现会把单次下载上限控制在不超过 `512 MiB`

### 2. 大镜像自动转 sparse / 分片

当镜像大于单次可下载上限时，工具不会直接失败，而是自动处理：

- 如果输入是普通原始镜像，会按 Android sparse 格式拆分后分段刷写
- 如果输入本身已经是 Android sparse 镜像，但单个文件仍然过大，会重新分片后刷写

这意味着像 `super.img`、较大的 `system.img` 这类文件也可以直接尝试使用 `flash`。

### 3. 流式处理大文件

刷写大文件时，工具会尽量使用流式读取，而不是把整份镜像一次性全部读入内存。

## 常见用法

### 查看设备并读取关键信息

```bash
./gofastboot devices
./gofastboot getvar product
./gofastboot getvar current-slot
./gofastboot getvar max-download-size
```

### 指定某台设备刷写

```bash
./gofastboot devices
./gofastboot -serial ABCDEF012345 flash boot boot.img
```

### 刷写当前 slot 的启动镜像

```bash
./gofastboot getvar current-slot
./gofastboot flash boot boot.img
```

如果设备支持 slot，程序会根据 `current-slot` 自动解析为 `boot_a` 或 `boot_b`。

## 失败排查

- 看不到设备：先执行 `./gofastboot devices`，确认设备已进入 fastboot 且 USB 权限正常
- 多设备报错：补上 `-serial SERIAL`
- 变量查询失败：确认设备 bootloader 是否支持对应 `getvar`
- OEM 命令失败：很多 `oem` 子命令是厂商私有能力，需要设备本身支持
- 刷写失败：优先检查分区名、镜像文件路径、bootloader 锁状态，以及设备是否允许对该分区写入
