# 板包接口 v1

板包是一个含 `board.json` 的本地目录。它可以来自任何 Git 仓库。路径都相对于该目录；板包内容（包括补丁和 overlay）是构建输入，只对由适配者审核的板包运行构建。

| 字段 | 用途 |
| --- | --- |
| `schema`、`board_id`、`revision`、`name`、`soc`、`compatible` | 接口版本 `1`、规范板型和修订号、显示名、`rk3588`、Linux DT 的 compatible |
| `default_profile`、`profiles` | 默认模式和可构建的 `spi`、`emmc` 模式。每种模式有自己的下面一组设置 |
| 顶层 `firmware.ddr`、`firmware.bl31` | DDR/TPL 和 BL31；每项写 `sha256`，再写 `url`（HTTPS）、`path`（包内相对路径）或 `file`（由 `--firmware-dir` 提供）之一 |
| `rootfs_overlay` | 可选。附加恢复文件；`/etc/rkdev-modules` 指定启动时加载的模块，`/etc/rkdev-init.d` 可提供板级初始化 |
| `rootfs_packages` | 可选。在恢复系统的基础 Alpine 软件包之外加装软件包。包名写在数组内，可用于提供板级网络或诊断工具 |
| `rootfs_programs` | 可选。把 `usr/bin/` 或 `usr/sbin/` 中的目标路径映射到板包内的单文件 C 程序。构建器交叉编译成 ARM64 静态程序，用于板级初始化或守护进程 |

如果 overlay 提供 `/etc/rkdev-network`，公共启动脚本在回环接口和板级初始化就绪后后台运行它，代替对物理 Ethernet 接口逐个启动 DHCP。overlay 可以提供 `/etc/inittab` 设置板上的登录终端；保留 `/usr/sbin/rkdev-start` 与 `/usr/bin/rkdev` 两个公共入口。

要启用本地恢复界面，在 overlay 放置 `/etc/rkdev-display-enabled`，将 inittab 的 tty1 入口设为 `tty1::respawn:/usr/sbin/rkdev-display`。内核应启用 VT、键盘输入和 HDMI/DP 对应的 DRM 驱动及 fbdev 兼容层。显示初始化失败时 tty1 回到原登录终端；串口可独立设置。

可配置网络使用 `/etc/rkdev-network.json`。其中 `ports` 定义逻辑 ID、标签和设备树硬件路径，以免枚举顺序改变 WAN/LAN 角色；`defaults` 定义 WAN、LAN 与 Wi-Fi，`station` 和 `ap_bands` 描述无线能力。`iptables` 选择防火墙后端。启动失败时可回到板包原有的 `/etc/rkdev-network`。eMMC 板包可以指定 `store_offset`，在引导与恢复之间预留两块 16 KiB 的配置区，偏移要求 4 KiB 对齐且不与环境变量重叠。

公共恢复系统管理 OpenSSH、主机密钥和恢复账号。板包可放置 `/etc/rkdev-managed-ssh` 来默认开启 SSH。如果 eMMC 板包在引导和恢复分区之间预留了配置区，可在 `/etc/rkdev-settings-offset` 指定十进制起始字节位置（4 KiB 对齐，需连续 32 KiB）；已有网络配置区的板包复用 `store_offset`。

每种 profile 定义：

| 字段 | 用途 |
| --- | --- |
| `uboot`、`linux` | 各有 `config` 和 `dts`；可选 `patches`（按顺序应用）和 `overlay`（按上游树相对位置覆盖文件） |
| `device`、`capacity` | Linux 下的实际启动介质，以及产物的整盘长度（字节）。SPI 要求介质容量相同；eMMC 实际介质可以比恢复镜像大 |
| `loader` | `offset`、`length`、`format`。RK3588 打包的 loader 从 `0x8000` 写入，格式为 `rockchip-spi` 或 `rockchip-sd` |
| `environment` | `offsets` 和单份 `size`。SPI 一份、eMMC 可一份或两份。SPI env 必须独占其擦除块 |
| `recovery` | `offset`、`length`、ext4 `label`。SPI 还须有 RAM `load_address`；eMMC 是 GPT `partition: 1`，同名 |
| eMMC `system` / SPI `erase_size`、`mtd_name` | eMMC 定义系统区 `offset` 与占位分区 `label`。SPI 定义擦除大小及 MTD 名；指定 `/dev/mtdblockN` |
| `boot_order`、`system_targets`、`recovery_args` | U-Boot 默认启动设备顺序、Web 允许写入的系统目标（默认同顺序）、恢复内核参数。设备值为 `emmc`、`sd`、`usb`、`nvme`、`sata` |
| `key` | 可选。 `type: adc` 填 `channel`、`minimum`、`maximum`，或 `type: button` 填 DT 按键 `label`；可填 `samples`、`delay_ms`。 `action` 为 `recovery`、`maskrom` 或 `host-or-recovery`；最后一种另填 `usb_node`，可设 `wait_ms` |
| `mmc_device`、`sd_device`、`loader_index`、`boot_fallback` | U-Boot 设备号（默认 eMMC 为 0，SD 为 1，Loader 为目标设备号）；本地恢复失败后回到 `console`、进入 `maskrom`，或在 eMMC 模式下进入 `loader` |
| profile 下的 `firmware`、`release_url` | 可选。前者把固件描述映射到 `/lib/firmware` 相对路径；后者是 HTTPS 上的板型升级清单 |
| `rkfw` | SPI profile 的可选 Rockchip 原始分区引导：`kernel`、`initrd`、`resource`、`root` 为相应分区号，`args` 是额外的内核参数 |

U-Boot 的启动 Logo、F2 恢复和 F10 引导菜单由 RKdev 提供。板包在 U-Boot 设备树的 VOP 节点用 `u-boot,hdmi` 列出实际连接的 HDMI 发射端，并为各端配置引脚以及所需的使能 GPIO。若板上有限制，可在发射端设置 `u-boot,preferred-vic`（例如 4 为 720p60），仅在显示器声明支持时优先选择该模式。没有可用的 HDMI 显示器时，快捷键和菜单仍可通过串口使用。启用了 USB 主机的板也可用 USB 键盘。

布局以字节表示。容量、偏移、长度必须满足扇区或擦除块对齐，载荷不可重叠。eMMC v1 预留首尾 GPT，并使用 512 B 逻辑扇区。将较小恢复镜像写到较大 eMMC 后，须将备用 GPT 移到实际磁盘末尾；验收时以真实设备的两份 GPT 为准。G98 的 SPI 与 eMMC 引脚互斥属于实际板的电气配置。

构建器从布局生成 SHA-256 `layout_id`，绑定 `/chosen/rkdev,*`、镜像清单、恢复 ext4 UUID 和默认 `rkdev_layout`。同一个 ID 的兼容更新可替换恢复和引导；偏移或分区规则改变应另做迁移包。环境的 `rkdev_targets` 记录用户启动顺序，可额外用 `recovery` 提前尝试本地恢复；未指定时本地恢复仍是回退项。`rkdev_installing` 由恢复程序在 eMMC 系统安装期间管理。恢复会核对运行中的 DT 身份、目标设备的几何和恢复区标识，才启用该布局的写入功能。

在线清单文件名可以不同于镜像；其 `image.name` 必须是同目录下的 `.img`。离线升级从网页选对应 `.img` 和 `.json`。哈希用于检测不匹配或损坏；在线信任来自板包选定的 HTTPS 发布地址。新增板包应先在非生产介质上验证冷启动、恢复、USB 下载模式和镜像读回，再对外发固件。
