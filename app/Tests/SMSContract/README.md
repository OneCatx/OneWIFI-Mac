# SMSContract（MAC-P4 / T34.6）

离线远端 SMS 契约夹具与断言（**contacts 列表 / thread 详情 / send 成功+失败**）。**不连** live WEB、Keychain、本机 UDS。

## 真源

- `docs/客户端-Mac-WEB-远端能力优先级-MAC-P0-20261021.md` §2.3
- `WEB后端/internal/server/sms_api*.go`（`handleSMSContacts` / `handleSMSThread` / `handleSMSSend` / `storedSMSResponse`）
- 路径常量：`Mac/app/Sources/GatewayAPI.swift`（`smsContacts` / `smsThread` / `smsSend`）

## 夹具

| 文件 | 覆盖 |
|---|---|
| `Fixtures/sms_contacts_success.json` | GET `/api/sms/contacts` 联系人数组 |
| `Fixtures/sms_contacts_empty.json` | 空列表 |
| `Fixtures/sms_thread_success.json` | GET `/api/sms/thread` 消息数组（详情） |
| `Fixtures/sms_thread_invalid_peer.json` | 400 `invalid_peer` |
| `Fixtures/sms_send_success.json` | POST `/api/sms/send` **202** + `outcome` |
| `Fixtures/sms_send_device_required.json` | 400 `device_required` |
| `Fixtures/sms_send_rate_limited.json` | 429 `sms_rate_limited` + Retry-After |
| `Fixtures/sms_send_device_feature_unsupported.json` | 501 `device_feature_unsupported` |
| `Fixtures/sms_send_submission_failed.json` | 502 `sms_submission_failed` + `retry_safe=false` |
| `Fixtures/sms_contacts_unauthorized.json` | 401 `unauthorized` |
| `Fixtures/sms_send_physical_missing.json` | 503 `physical_device_missing` |
| `Fixtures/sms_send_manager_unavailable.json` | 503 `device_manager_unavailable` |
| `Fixtures/golden_path_sequence.json` | contacts→thread→send 同 device/peer |
| `Fixtures/mdd_sms_forbidden.json` | 禁 MDD `/api/instances*/sms*` |

## 可复跑命令

### Windows（离线夹具，无 Xcode）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File docs\_mac_p4_logs\T34.6-run-sms-contract-fixtures.ps1
```

期望：全部 `PASS …`；末行 `SMSContract fixtures: ALL PASS (offline)`；退出码 `0`。

### macOS（Swift 全量）

```sh
cd Mac
sh scripts/test-sms-contract.sh
```

期望：stdout 含 `PASS sms_fixtures` 与 `SMSContract: all assertions passed`；退出码 `0`。

## 铁律

- 不为绿放宽断言；失败即 FAIL。
- 零产品业务码；本目录为测试夹具 / harness。
- 主路径仅全局 `/api/sms/*`；禁止 MDD `/api/instances/{id}/sms*`。
- 禁止把 HTTPS 网关请求塞进本机 UDS `APIClient`。
