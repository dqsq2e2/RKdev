# RKdev

RKdev 在同一个 `main` 分支维护两件东西：`boot/` 是主线 U-Boot 的公共启动策略，`recovery/` 是 Alpine、Linux 和 Go Web 恢复助手。两者由同一次构建共享布局及环境变量接口。板子的 DTS、Kconfig、引脚、分区偏移和固件来源由外部板包提供；构建一个新板包不需要给 RKdev 增加分支。

Linux 使用 `ophub/linux-6.18.y`，U-Boot 使用 `u-boot/u-boot`。精确版本见 [sources.lock](sources.lock)。内核从 ophub 下载并校验归档 SHA-256 和提交；U-Boot 直接从官方仓库取锁定提交。构建不会检出迁移参考仓库。

## 用外部板包构建

在 Linux 或 WSL 中运行。需要 ARM64 交叉工具链和 Go；在 x86_64 上用 QEMU user-mode binfmt 运行 Alpine 的 `apk`。Ubuntu 示例：

```sh
sudo apt install build-essential gcc-aarch64-linux-gnu git curl bc bison flex +  libssl-dev libgnutls28-dev libelf-dev libncurses-dev device-tree-compiler +  python3 python3-dev python3-pyelftools python3-setuptools swig +  u-boot-tools gdisk util-linux e2fsprogs cpio pigz xz-utils kmod +  qemu-user-static binfmt-support
go version                       # recovery/app/go.mod 中指定最低版本
```

板包可以来自 RKdev-boards，也可以是第三方自己的 Git 仓库。下面从 RKdev 根目录执行：

```sh
board=../my-boards/boards/my-board/v1
python3 tools/rkdev.py lint --board "$board" --profile emmc
sudo env "PATH=$PATH" python3 tools/rkdev.py build +  --board "$board" --profile emmc --locked --version 0.1-dev +  --firmware-dir /path/to/board-firmware --out out/my-board
python3 tools/rkdev.py verify --board "$board" --profile emmc +  --image out/my-board/my-board-v1-emmc-0.1-dev.img
```

支持 SPI 的板包用 `--profile spi`。这一步选定镜像及写入介质；Web 会显示实际恢复模式。要分别构建 U-Boot 和内核可执行 `boot`、`kernel`；`prepare` 会生成布局、环境位置和 U-Boot 配置头。开发者可用 `pack` 组装已有组件，用 `verify` 独立验证。

每套镜像有 `.img`、同名 `.json` 和 `SHA256SUMS`。清单记录板型、修订、profile、布局摘要、区段 SHA-256、上游提交与构建信息。Alpine 的软件包版本随指定发行分支仓库更新，构建记录实际安装版本；目前不保证每次构建的字节完全相同。

## 启动与恢复

- SPI 从板包定义的 NOR 区域加载恢复文件系统；系统写入可用的外部目标盘。
- eMMC 在同一用户区保留 loader、环境和恢复分区，系统安装进入独立系统区。整盘镜像和浏览器里的恢复升级是两种操作。恢复升级只更新布局允许的引导及恢复区段；安装系统时先标记未完成，写入及读回完成后才允许 U-Boot 扫描 eMMC 系统区。
- 按键可选恢复、Maskrom 或依据 USB 主机握手分流。没有可用系统时尝试本地恢复，随后按板包的设置进入 Maskrom、RockUSB Loader 或返回串口。Maskrom 属于 BootROM，Loader 由 U-Boot 提供。串口可执行 `rkdevboot recovery`；其他动作取决于板包配置。
- Web 通过 HTTPS 提供刷写、引导设置、硬件检测、备份与还原、终端、防火墙以及恢复系统更新。使用 `https://设备IP/`，用户名为 `rkdev`，每次启动的登录口令在串口显示。可用存储和在线更新源由板包决定。

接口与位置定义见 [板包契约](docs/board-package.md)。源码和第三方材料见 [来源记录](SOURCES.md)。
