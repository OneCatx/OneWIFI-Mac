# 项目架构与开发笔记

> 【历史】下文保留 Switch／Proletarius 时代架构快照痕迹；**不是 OneWIFI 的当前指令或验收结论**。现行对外产物为 `OneWIFI.app`，数据目录为 `~/Library/Application Support/OneWIFI`。对内二进制名 `switchd`／`switchd-rs` 与工程名 `Switch.xcodeproj` 另记 D-INT 债，本文件不授权改名。

## 架构总览

```
OneWIFI.app                    # 对外产物（历史叙述曾写 Switch.app）
├── Contents/MacOS/OneWIFI     # SwiftUI 前端（进程宿主）
└── Contents/Resources/backend/
    ├── switchd-rs               # Rust 网关、原生状态/归档/编解码（对内名）
    ├── switchd                  # 尚未迁移的硬件、VoWiFi/IMS 兼容运行时（对内名）
    └── libusb-1.0.0.dylib            # 随包 USB 库（@loader_path 链接）
```

- **前端**：SwiftUI（`app/Sources/`）
- **通信**：Unix domain socket（`~/Library/Application Support/OneWIFI/switch.sock`；socket 文件名对内遗留），Swift 侧自定义 `URLProtocol` 以 `http+unix://` 协议走 URLSession
- **后端**：Rust 网关（`rust/`）是应用唯一公开 socket；其功能归属门禁仅将已完成迁移的路由留在 Rust，仍需硬件会话的请求转发到受监督的 Go 兼容进程（`backend/`）
- **构建**：`xcodebuild`（支持 macOS 27 SDK 的 Xcode），Go/Rust 后端由工程的 post-build 脚本随 Xcode 构建一并编译嵌入；具体工具链随源码快照记录，CI 见 `.github/workflows/release.yml`

## 关键源码文件

| 文件 | 职责 |
| --- | --- |
| `app/Sources/BackendProcess.swift` | 子进程管理（自动启动/停止、socket 路径） |
| `app/Sources/ProletariusApp.swift` | SwiftUI Window/MenuBarExtra 生命周期：原生静默启动、信号与实时流量 |
| `app/Sources/DeviceSnapshotStore.swift` | 带 revision 的设备聚合状态源；拒绝旧实例和旧请求响应 |
| `app/Sources/DashboardStore.swift` | 页面协调器；组合模块、读卡器、通话、音频与网络子状态源 |
| `app/Sources/UnixSocketURLProtocol.swift` | http+unix:// URLProtocol（POSIX socket + HTTP/1.1 解析，支持 Content-Length/chunked） |
| `app/Sources/APIClient.swift` | URLSession 封装；dateEncodingStrategy = .iso8601（Go RFC3339） |
| `app/Sources/AudioBridge.swift` | CoreAudio IOProc 音频桥（重采样、输入输出设备与 AirPods 路由） |
| `app/Sources/Views/HomeView.swift` | 按 scope 呈现独立模块页与读卡器页；复用状态源，但不混用设备卡片或 VoWiFi 操作 |
| `app/Sources/Views/SMSView.swift` | 短信页面协调、搜索、选择与删除状态 |
| `app/Sources/Views/SMSConversationViews.swift` | 会话列表、聊天气泡、详情、清理与发送界面 |
| `app/Sources/MultiStandbyStore.swift` | 多 Profile 状态、短信、幂等发送与通知游标；独立于原有读卡器短信页面 |
| `app/Sources/Views/MultiStandbyView.swift` | 莫比乌斯环 1 卡 N 待工作区 |
| `backend/cmd/proletarius-macos/multistandby_card.go` | FIFO 卡片许可、物理身份确认、切卡恢复和私有 Go AKA |
| `backend/cmd/proletarius-macos/multistandby_manager.go` | 多会话生命周期、重认证代次、恢复原 Profile 与预留释放 |
| `app/Sources/Views/ReaderCardView.swift` | 读卡器、eSIM Profile 与 VoWiFi 操作入口 |
| `app/Sources/Views/DiagnosticsView.swift` | 网络诊断（弹窗）+ AT 调试 |
| `backend/cmd/proletarius-macos/usbat_darwin.go` | libusb AT 桥（兼容 2ca3:4006 与 2c7c:0125） |
| `backend/cmd/proletarius-macos/main.go` | HTTP API + 短信归档 + 通话控制 |

## 关键设计决策

### 1. 短信存储模型（三层）
- **SIM 卡（SM）** 与 **模块（ME）** 存储：硬件源头，短信保留在这里（默认）
- **内存缓存**：后端进程内，最多 500 条
- **本机归档**（接管模式开启或发送记录）：`sms-archive.json`，最多 1000 条
- 短信列表 = 内存 + 归档合并去重（按 sender+content+timestamp 的 key）
- 单条删除按 存储+索引（SM/ME）或 sender+content+时间窗口（本机）
- 清空支持多选：SIM / 模块 / 本机（`POST /api/sms/clear`）

### 2. 短信接管模式（`smsAdopt`）
- 开启后：轮询读到的短信持久化到本机归档 → 清空 SM/ME 原始存储
- 状态持久化到 `sms-adopt.json`，**以后端为准**（app 启动拉取真实状态）
- 发送记录（direction=out）**总是**归档，不依赖开关

### 3. 网卡优先级
- `GET /api/network/services`：解析 `networksetup -listnetworkserviceorder`
- `PUT /api/network/services-order`：`osascript do shell script` 提权执行（base64 编码避免引号地狱）
- 模块网卡识别：模块 USB 产品名（Baiwang）匹配 Hardware Port 名
- 前端拖拽排序：onDrag/onDrop + DropDelegate（**不要用 List+onMove，ScrollView 内嵌 List 渲染不可靠**）

### 4. 通话与音频
- DJI 模块使用 `AT+CLCC` 轮询通话状态（stat: 0=active,2=dialing,3=alerting,4/5=incoming,6=disconnected）
- 读卡器通话经 IMS 安全通道，媒体由 Swift CoreAudio 桥接；运营商未提供 PCMA 时允许协商 AMR，避免以错误的强制编码破坏呼叫
- Rust `proletarius-sim` 提供只读 SIM 身份能力：标准 SELECT FILE / READ BINARY APDU 构造及 EF-ICCID 半字节解码。Go 聚合 USIM 字段时会先关闭自身 PC/SC 句柄，再通过认证的 native identity endpoint 校验并采用 Rust ICCID；Rust 不可用时保留已验证的 Go 回退值。
- Rust `proletarius-reader` 的 AKA 锁按读卡器名称隔离，并以弱引用回收拔插设备锁，避免热插拔造成全局锁表增长。
- 自 0.80 起，Rust AKA 请求可指定 USIM/ISIM，在同一次 PC/SC 事务内从 EF-DIR 查找完整 AID 并认证；Go 不再为该路径预先打开卡或枚举 AID。Rust 服务未配置时仍使用 Go 兼容路径，ISIM 缺失保持显式回退语义。
- **无号码的 active 条目视为固件残留**（ATH/CHUP 清不掉），忽略
- 来电横幅（顶部 overlay）+ 拨号/挂断；状态卡住时轮询失败标记 "unknown"

### 5. 服务生命周期
- app 启动自动拉起后端、退出自动停止（无手动开关）
- `BackendProcess.start()` 的 guard 写法注意：`state == .failed("")` 永远不匹配，失败后无法重启——用 `if case .running = state, process?.isRunning == true { return }`

## 踩坑记录

1. **日期解析**：Go time.Time 输出 RFC3339Nano（可能带小数秒），Swift 需自定义日期解析（ISO8601 带/不带小数秒都试）
2. **SwiftUI 编译**：ViewBuilder 里不能直接 for 循环（用 ForEach）；`onChange` 双参数版本需 macOS 14+（目标 13 用单参数）；超长表达式导致编译器 type-check 超时（抽子视图）
3. **@main 命令行程序**：顶层表达式 + semaphore 在 `-parse-as-library` 模式会卡死（测试工具用 main.swift 顶层代码模式）
4. **List 在 ScrollView 内**：macOS 渲染不可靠（空白/塌陷），用 VStack+ForEach 替代
5. **AudioBufferList**：Swift 中 `mBuffers` 不是数组，用 `UnsafeMutableAudioBufferListPointer` 遍历；IOProc 参数是非 Optional 指针（可判 nil 但不能 if let）
6. **语音开关状态**：`AT+QCFG="usbcfg"?` 响应含回显行（`AT+QCFG="usbcfg"?`），解析必须用 LastIndex 或正则；末尾 `\r\nOK` 会污染 Split 结果（正则提取最稳）
7. **模块 UAC 音频设备名**是 "AC/AS Interface" 而非 "Baiwang"

## 构建

```sh
mise run build               # 构建 Apple Silicon / macOS 27 的 .app 并输出到 dist/
mise run launch              # 启动已构建的 app
```

需要：Xcode（26 或更新，编译 `AppIcon.icon` 与 SwiftUI）、Go 1.26+、pkg-config、libusb（brew）。

## 测试

- `app/Tests/APIProbe`：无硬件环境 API 解码验证（服务连通、模型解析、错误路径）
- 真机测试：从 app bundle 启动 `switchd-rs -listen unix:<SWITCH_DATA_DIR>/test.sock`，经认证 Unix socket 验证读卡器、代理、IMS/SMS/通话链路

## Switch 隔离边界

直接打开应用时默认数据根为 `~/Library/Application Support/Switch`；开发脚本通过绝对路径 `SWITCH_DATA_DIR` 指向工作区 `build/runtime`。新多待原始短信、分片、回执与幂等发件记录由 Rust 持久化，与既有单卡短信模型分开。每个 Profile 以独立 SWu/netstack/IMS 代次保持网络会话；共享物理卡只在 FIFO 许可内完成身份检查、切卡和 AKA。该路径启用 SMSOnly，旧语音会话的默认协议行为保留。
