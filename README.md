<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/AppIcon-dark.png">
    <img alt="OneWIFI" src="docs/AppIcon-light.png" width="128">
  </picture>
</p>

<h1 align="center">OneWIFI for Mac</h1>

<p align="center">
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/License-PolyForm%20Noncommercial-orange"></a>
  <img alt="macOS" src="https://img.shields.io/badge/macOS-27%2B-black?logo=apple&logoColor=white">
  <img alt="SwiftUI" src="https://img.shields.io/badge/SwiftUI-5-orange?logo=swift&logoColor=white">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26.3-blue?logo=go&logoColor=white">
  <img alt="Rust" src="https://img.shields.io/badge/Rust-1.98-black?logo=rust&logoColor=white">
  <img alt="Version" src="https://img.shields.io/badge/version-0.1.0-blue">
</p>

## 简介

**OneWIFI for Mac** 是面向 macOS 的个人蜂窝设备控制中心。SwiftUI 前端与本机后端通过 Unix domain socket 通信，在一个应用中完成模块与读卡器状态、短信、VoWiFi、eSIM、蜂窝通话和诊断。当前生产架构由 Rust 网关承载本机 API，并监督 Go 硬件子进程；协议核心、存储与原生服务经内部接口协作，Go 仍负责实际硬件及部分 IMS/VoWiFi 会话路径。两者都是运行依赖，不是重复的备用后端。

### 血统说明

本树技术底座源自 **Switch 2.67**（`ventor/Switch-2.67-source` 只读种子）。对外品牌与显示名一律为 **OneWIFI**；原 Switch / Proletarius 迭代与迁移记录仅作为历史证据保留，不代表 OneWIFI 的指令或已验收能力。

当前保留 Swift/Rust/Go 混合架构，以稳定性和可测收益为优化门槛，不再以迁移比例为目标。

### 家族位置与边界

**OneWIFI for Mac** 是 OneWIFI 桌面端正式树（`ONEWIFIX/Mac`）：本机蜂窝能力与远端网关能力双通道并存。它与 `OneIms-Ultra`、Shizuku/OneKuku、Onetools、Stellar 等旁系产品并列于家族地图，但**不**混仓、不共享特权限通道、不把旁系工具嵌进本机模块页。激活签发旁系（OneActive）本阶段不嵌入；若日后需要，仅允许独立 App / 外链，禁止塞进模块页。

应用内「调试与更新」提供正式版 / 测试版渠道与手动或自动更新检查；发布与公证流程以 Release 成文为准，本 README 不代行公证或密钥步骤。第三方必须保留的版权与许可证见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。

## 功能

| 模块 | 功能 |
| --- | --- |
| 模块页 | DJI 模块、SIM、信号和设备信息；模块 Wi-Fi Calling、本次与累计流量、USB 网卡、网络服务排序、4G 出口检查和模块重启；模块断开时仍保持完整布局，不折叠功能 |
| 读卡器页 | PC/SC 自动枚举与连接、当前卡片身份、eSIM Profile 管理、运营商模板、自定义 ePDG、手动前置代理和读卡器 VoWiFi 状态 |
| 短信 | 会话收发与回复、单条删除、验证码标记；SIM / 模块存储扫描与选择性清理；短信接管、本机归档和系统通知 |
| eSIM | 实体 SIM / eUICC 识别、卡片与 Profile 信息；Profile 下载、启用、改名和删除；号码资料与模块通讯录检测 |
| 1卡N待 | 同一已选号卡内多Profile独立SWu/IMS短信会话；分别启动/停止、独立收件和发件归档、后台通知；软件和模拟回归通过，实卡/运营商并发能力待验收 |
| 通话 | 独立通话页面与首次启用引导；拨号、接听、挂断、Mac 麦克风与扬声器桥接、来电卡片、铃声、通话记录，以及配置备份的导入、导出、删除与安全还原 |
| 应用与菜单栏 | 开机自启、静默启动、关闭窗口后后台运行；菜单栏可显示信号强度与实时上下行速率，并可打开主界面或退出应用 |
| 调试与更新 | 网络诊断、AT 指令和通知模拟；正式版 / 测试版渠道、手动或自动更新检查及版本跳过 |

## 接入准备

- 大疆第一代 4G 模块
- 可正常使用的实体 SIM，或与当前实现兼容的实体 eUICC / eSIM 卡片
- 支持数据传输的 USB-C 线缆
- Apple Silicon Mac，macOS 27.0 及以上

模块的 USB 设备标识通常为 `2ca3:4006`。连接后若 macOS 完全识别不到设备，请先确认线缆支持数据传输。

| 指示灯 | 常见含义 |
| --- | --- |
| 红色常亮 | 未插入 SIM 卡 |
| 红色闪烁 | SIM 卡未被正常识别 |
| 绿色常亮 | SIM 已识别，蜂窝信号通常较好 |
| 绿色闪烁 | SIM 已识别，蜂窝信号可能较弱或仍在注册 |

## 通话模式

通话页支持拨号、接听和挂断，并通过 Mac 的麦克风与扬声器传输语音。首次启用时，应用会在用户确认后备份模块配置、开启通话所需接口、重启模块，并下载和校验适配 QDC507 的语音运行时；运行时仅部署到临时目录，不写入模块闪存。

配置备份可在通话页导入、导出和删除，也可在设备身份匹配且没有通话进行时安全还原。导入只保存通过格式与一致性校验的 JSON，不会立即修改模块；还原前会再次备份当前配置，过程中可能重启模块并暂时中断网络、短信和 eSIM 操作。

首次启用可能持久修改 USB 组合并授权 ADB。当前语音运行时仅支持模块内核 `3.18.44`；实际通话仍取决于 SIM、套餐以及运营商的蜂窝语音或 VoLTE 支持。

## 构建与运行

源码位于 `Mac/`（正式树），缓存、日志和测试数据位于构建目录，应用产物为 `OneWIFI.app`（对外已换牌；工程文件名如 `Switch.xcodeproj` 等对内血统标识另记债）。在源码目录执行：

```sh
scripts/build.sh                    # 生成应用；不生成 DMG
scripts/launch.sh                   # 以 build/runtime 为隔离数据目录启动
scripts/quit.sh                     # 正常退出 OneWIFI 和它拥有的后端
scripts/test-migration-gates.sh      # Rust/Go 门禁；不执行真实号卡操作
scripts/clean-generated.sh          # 只列出缓存；不删除源码和产物
```

需要支持 arm64 macOS 27 部署目标的 Xcode、XcodeGen、Go 1.26.3、Rust 1.98、`pkg-config` 和 arm64 `libusb`。不要求安装 mise；已有 mise 时可使用同名 build、launch、quit、test:gates 任务。

独立安装并直接打开的应用默认保存数据到 `~/Library/Application Support/OneWIFI`。开发启动和测试统一使用绝对路径数据目录指向工作区的 `build/runtime`，不会自动导入原项目数据。内部 Go module、Rust package 与部分私有 IPC 环境变量名称可能仍保留 Switch/Proletarius 血统标识以兼容原实现。

应用启动后自动管理后端进程，关闭窗口后继续在菜单栏运行，通过「退出」停止自有服务。现有语音页面继续保留；新增 1 卡 N 待页面采用莫比乌斯环图标，只面向多 Profile VoWiFi 短信，不接入语音。多待应逐层报告身份、隧道、IMS 与短信就绪状态，真实卡片和运营商兼容性以实测为准。

默认构建和 CI 都不打包磁盘映像。`scripts/release.sh` 仅作为将来显式发行时的独立工具保留；本轮不执行该发行工具。

## 验证范围

当前构建基线为 Apple Silicon、macOS 27，具体 Xcode/Go/Rust 版本随每次源码快照记录；最低部署目标为 macOS 27。历史测试环境与当前发行验收应分别查看，不能把过去的真机记录视为当前版本全功能通过。

API 模型与端点已通过无硬件环境自动验证（`app/Tests/APIProbe`）；短信、eSIM 写入和蜂窝链路等完整功能仍需对应硬件与网络环境验证。

## 文档

- [`docs/MODULE_RESEARCH.md`](docs/MODULE_RESEARCH.md)：模块识别、USB ID 恢复、语音调查与 AT 指令
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)：架构、关键设计决策与开发记录
- [`docs/RUST_MIGRATION_V4.md`](docs/RUST_MIGRATION_V4.md)：Rust 渐进式重构边界、阶段与回滚策略
- [`docs/VOWIFI_PROXY_V015_CONTRACT.md`](docs/VOWIFI_PROXY_V015_CONTRACT.md)：冻结的 0.15 前置代理传输契约

## 许可证

OneWIFI for Mac 使用非商业用途许可证（继承底座工程许可文本，见 [`LICENSE`](LICENSE)）。必须保留的第三方版权与许可证文本见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)，随源码和安装包独立保存，不作为产品品牌或界面入口展示。

### 开发门禁

Rust/Go 迁移相关改动统一使用 `./scripts/test-migration-gates.sh` 执行格式检查、Rust workspace 测试、Go vet 与兼容测试；脚本不会执行真机操作，也不会生成 DMG。
