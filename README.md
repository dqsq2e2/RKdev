# RKdev

RKdev 是面向 Rockchip 设备的启动与恢复系统，支持 SPI NOR 和 eMMC。`boot/` 提供主线 U-Boot 的启动策略，`recovery/` 提供基于 Linux 和 Alpine 的 Web 恢复助手。设备树、分区布局和外设配置由独立的板包提供。

Linux 使用 `ophub/linux-6.18.y`，U-Boot 使用 `u-boot/u-boot`。源码版本见 [sources.lock](sources.lock)。官方板包和固件下载见 [RKdev-boards](https://github.com/dqsq2e2/RKdev-boards)。

## 用外部板包构建

在 Linux 或 WSL 中运行。需要 ARM64 交叉工具链和 Go；在 x86_64 上用 QEMU user-mode binfmt 运行 Alpine 的 `apk`。Ubuntu 示例：

```sh
sudo apt install build-essential gcc-aarch64-linux-gnu git curl bc bison flex \
  libssl-dev libgnutls28-dev libelf-dev libncurses-dev device-tree-compiler \
  python3 python3-dev python3-pyelftools python3-setuptools swig \
  u-boot-tools gdisk util-linux e2fsprogs cpio pigz xz-utils kmod \
  qemu-user-static binfmt-support
go version                       # recovery/app/go.mod 中指定最低版本
```

板包可以来自 RKdev-boards，也可以是第三方自己的 Git 仓库。下面从 RKdev 根目录执行：

```sh
board=../my-boards/boards/my-board/v1
python3 tools/rkdev.py lint --board "$board" --profile emmc
sudo env "PATH=$PATH" python3 tools/rkdev.py build \
  --board "$board" --profile emmc --locked \
  --firmware-dir /path/to/board-firmware --out out/my-board
python3 tools/rkdev.py verify --board "$board" --profile emmc \
  --image out/my-board/my-board-v1-emmc-11.26-r1.img
```

官方板包的云编译入口在 [RKdev-boards 的 GitHub Actions](https://github.com/dqsq2e2/RKdev-boards/actions)。在 ARM64 Linux 上本机构建也可传 `--cross-compile=`，直接使用本机 GCC。

版本按北京时间的构建日期编号，例如 `11.26-r1`。本地构建在输出目录内递增当天序号，也可用 `--version` 指定版本。

支持 SPI 的板包用 `--profile spi`。这一步选定镜像及写入介质；Web 会显示实际恢复模式。要分别构建 U-Boot 和内核可执行 `boot`、`kernel`；`prepare` 会生成布局、环境位置和 U-Boot 配置头。开发者可用 `pack` 组装已有组件，用 `verify` 独立验证。

每套镜像有 `.img`、同名 `.json` 和 `SHA256SUMS`。清单记录板型、修订、profile、布局摘要、区段 SHA-256、上游提交与构建信息。Alpine 的软件包版本随指定发行分支仓库更新，构建记录实际安装版本；目前不保证每次构建的字节完全相同。

## 启动与恢复

- SPI 从板包定义的 NOR 区域加载恢复文件系统；系统写入可用的外部目标盘。
- eMMC 在同一用户区保留 loader、环境和恢复分区，系统安装进入独立系统区。整盘镜像和浏览器里的恢复升级是两种操作。恢复升级只更新布局允许的引导及恢复区段；安装系统时先标记未完成，写入及读回完成后才允许 U-Boot 扫描 eMMC 系统区。
- 按键可选恢复、Maskrom 或依据 USB 主机握手分流。没有可用系统时尝试本地恢复，随后按板包的设置进入 Maskrom、RockUSB Loader 或返回串口。Maskrom 属于 BootROM，Loader 由 U-Boot 提供。串口可执行 `rkdevboot recovery`；其他动作取决于板包配置。
- 在本地网络打开 `http://设备IP/` 进入恢复网页，无需网页账号。网页提供刷写、引导设置、硬件检测、备份与还原、终端、防火墙和恢复更新。SSH、串口与 HDMI 登录由板包定义。

接口与位置定义见 [板包契约](docs/board-package.md)。源码和第三方材料见 [来源记录](SOURCES.md)。

## 致谢

感谢 [yifengyou/rockchip-ophub-linux-6.18.y](https://github.com/yifengyou/rockchip-ophub-linux-6.18.y) 项目，以及 yifengyou 对 Rockchip 恢复系统和 RKDevTool Web 的开发。RKdev 的恢复网页与刷机功能在这些工作基础上移植。
