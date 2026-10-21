# Mac 正式交付树声明（MAC-P0 / T30.3）

> 角色：Lane-MacBase
> 日期：2026-10-21
> 阶段：MAC-P0 · 建树与盘点

---

## 正式改动边界

| 项 | 路径 | 状态 |
|---|---|---|
| **正式交付 / 主改树** | `E:\\GQ\\Ultra\\ONEWIFIX\\Mac` | **本阶段起唯一可写交付树** |
| **种子（只读底座）** | `E:\\GQ\\Ultra\\ONEWIFIX\\ventor\\Switch-2.67-source` | **只读**；禁止当交付树；禁止再往种子里落产品改动 |
| Android | `ONEWIFIX\\Android` | 本路禁止改动 |
| iOS | `ONEWIFIX\\IOS` | 本路禁止改动 |

## 硬约束

1. **正式改动只进 `Mac`**。换牌、远端通道、构建脚本、本机能力修复一律在本树进行。
2. **`ventor\\Switch-2.67-source` 自本阶段（MAC-P0）起只读**，仅作对照与回滚参照；**禁止**把种子充作交付或在种子内改业务。
3. **禁止**把远端 HTTP/网关调用塞进硬件进程（Rust `proletariusd` / Go `switchd` 硬件路径），除非另有设计票批准。
4. **禁止**本路改动 `Android` / `IOS`。
5. 无 Apple Silicon + Xcode 环境时，**编译 / 真机 / DMG / 公证**一律标 **NOT RUN**，不得宣称已验证。

## 种子锚点（复制来源）

| 字段 | 值 |
|---|---|
| VERSION | `2.67` |
| BUILD_NUMBER | `267` |
| 主工程 | `Switch.xcodeproj` + `project.yml` |
| 复制方式 | `cp -a ventor/Switch-2.67-source Mac`（文件数 3213 = 3213） |

## 相关文档

- 策划真源：`docs/执行计划-Mac客户端-Switch底座-OneWIFI-八路子代理-20261021.md`（§4 MacBase、§8 MAC-P0、§10 T30.3）
- 本路报告：`docs/客户端-Mac-MacBase-MAC-P0-20261021.md`
- 证据：`docs/_mac_p0_logs/` · `tasks/T30.3/`
