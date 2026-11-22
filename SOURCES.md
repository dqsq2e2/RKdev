# 源码与素材来源

构建输入由 [sources.lock](sources.lock) 固定。板级二进制按板包清单来源和 SHA-256 取用。旧仓库是下列归属记录中的迁移参考，构建器不会从它们获取代码。

| 材料 | 迁移参考 |
| --- | --- |
| Go/Web 恢复助手 | yifengyou 的 RKDevTool Web，原导入提交 `d0a153b71`；本仓库迁自 PHX 恢复快照 `dc2a6d388d0bcbe9ab97a1fd7d9aecf013965481` 的 `rkdev/`，保留原来的七个页面与资源，替换布局、启动环境和升级流程 |
| U-Boot 按键及 USB 分流 | Rockchip 主线启动模式代码及 PHX U-Boot 快照 `ecb17469b4ae87e4da67a0742914cd4fb3e06a8b` |
| U-Boot PCIe 补丁 | dqsq2e2 的 G98 共享 PHY 变更 `96ad8e83c4e`，以及可选复位 GPIO 的清理修正；U-Boot 衍生部分沿用文件 SPDX |
| Lucide CPU 图标 | 随附 [许可证](recovery/app/res/lucide-LICENSE) |
| xterm.js / addon-fit | `@xterm/xterm@5.5.0` 和 `@xterm/addon-fit@0.10.0`，随附 [上游 MIT 许可证](recovery/app/res/xtermjs-LICENSE) |
| 系统图标 | 沿用 RKDevTool Web 固件下载页静态资源，对应项目的产品标识 |

原 G98 恢复分支 README 将 RKDevTool Web 标为开源，`rkdev/` 未找到单独的授权文本。对外发布前需核对该目录的适用许可证及系统图标的再分发条件。此条与两份锁定上游的内核/U-Boot 许可分开记录。
