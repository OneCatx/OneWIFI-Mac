# DeviceDomainContract（MAC-P3 / T33.6）

离线远端设备域契约夹具与断言（**dashboard / devices / overview** 只读黄金路径 + 失败路径）。**不连** live WEB、Keychain、本机 UDS。

## 真源

- `docs/客户端-Mac-WEB-远端能力优先级-MAC-P0-20261021.md` §2.2
- `WEB后端/internal/server/device_api*.go` · `device_api_view.go`
- 移动端 `Android/lib/features/devices/data/device_dto.dart`（`DeviceFleetDto` / `DeviceDetailDto`）
- 路径常量：`Mac/app/Sources/GatewayAPI.swift` · `gateway_api.dart`

## 夹具

| 文件 | 覆盖 |
|---|---|
| `Fixtures/dashboard_devices_success.json` | GET `/api/dashboard/devices` 摘要数组 |
| `Fixtures/dashboard_devices_empty.json` | 空仪表盘 |
| `Fixtures/devices_list_success.json` | GET `/api/devices` `{device_limit,devices[]}` |
| `Fixtures/devices_list_empty.json` | 空舰队 |
| `Fixtures/device_overview_success.json` | GET `/api/devices/{id}/overview` 单设备 + traffic |
| `Fixtures/device_overview_not_found.json` | 404 `device_not_found` |
| `Fixtures/device_overview_physical_missing.json` | 503 `physical_device_missing` |
| `Fixtures/devices_unauthorized.json` | 401 `unauthorized` |
| `Fixtures/devices_manager_unavailable.json` | 503 `device_manager_unavailable` |
| `Fixtures/device_bare_get_method_not_allowed.json` | 405 bare `GET /api/devices/{id}` |
| `Fixtures/golden_path_sequence.json` | dashboard→devices→overview 同 id |

## 可复跑命令

### Windows（离线夹具，无 Xcode）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File docs\_mac_p3_logs\T33.6-run-device-domain-fixtures.ps1
```

期望：全部 `PASS …`；末行 `DeviceDomainContract fixtures: ALL PASS (offline)`；退出码 `0`。

### macOS（Swift 全量）

```sh
cd Mac
sh scripts/test-device-domain-contract.sh
```

期望：stdout 含 `PASS device_fixtures` 与 `DeviceDomainContract: all assertions passed`；退出码 `0`。

## 铁律

- 不为绿放宽断言；失败即 FAIL。
- 零产品业务码；本目录为测试夹具 / harness。
- 禁止 `GET /api/devices/{id}`；详情只走 `/overview`。
- 禁止把 HTTPS 网关请求塞进本机 UDS `APIClient`；禁止 MDD `/api/instances*`。
