# CallsContract（MAC-RP3 / T43.6）

离线远端通话**控制面**契约夹具与断言（**list / dial / answer / hangup / history** 成功+失败）。**不连** live WEB、Keychain、本机 UDS；**不含** WebRTC / SSE / media-ticket。

## 真源

- `docs/客户端-Mac-WEB-Calls回收索引-MAC-RP0-20261021.md`
- `WEB后端/internal/server/call_api.go`（`handleCalls` / `handleCallAction` / `handleCallHistory`）
- `WEB后端/internal/vowifi/types.go` `type Call struct`
- WebRTC 排除：`docs/_mac_rp0_logs/T40.2-no-webrtc.md`

## 夹具

| 文件 | 覆盖 |
|---|---|
| `Fixtures/calls_list_success.json` | GET `…/calls` → `{calls:[Call]}` |
| `Fixtures/calls_list_empty.json` | 空列表 |
| `Fixtures/calls_dial_success.json` | POST `…/calls/dial` **202** + Call |
| `Fixtures/calls_answer_success.json` | POST `…/calls/answer` + Call `active` |
| `Fixtures/calls_hangup_success.json` | POST `…/calls/hangup` → `{ended,id}` |
| `Fixtures/calls_history_success.json` | GET `…/calls/history` → `{calls:[historyItem]}` |
| `Fixtures/calls_history_empty.json` | 空历史 |
| `Fixtures/calls_dial_invalid_number.json` | 400 `invalid_dial_number` |
| `Fixtures/calls_answer_invalid_call_id.json` | 400 `invalid_call_id` |
| `Fixtures/calls_hangup_call_not_found.json` | 404 `call_not_found` |
| `Fixtures/calls_answer_call_state.json` | 409 `call_state` |
| `Fixtures/calls_list_unauthorized.json` | 401 `unauthorized` |
| `Fixtures/calls_list_device_not_found.json` | 404 `device_not_found` |
| `Fixtures/calls_dial_vowifi_unavailable.json` | 503 `vowifi_runtime_unavailable` |
| `Fixtures/calls_history_unavailable.json` | 503 `call_history_unavailable` |
| `Fixtures/golden_path_sequence.json` | list→dial→answer→hangup→history |
| `Fixtures/mdd_calls_forbidden.json` | 禁 MDD `/api/instances*/call*` |
| `Fixtures/no_webrtc_forbidden.json` | 禁 WebRTC / SSE / media |

## 可复跑命令

### Windows（离线夹具，无 Xcode）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File docs\_mac_rp3_logs\T43.6-run-calls-contract-fixtures.ps1
```

期望：全部 `PASS …`；末行 `CallsContract fixtures: ALL PASS (offline)`；退出码 `0`。

### macOS（Swift 全量）

```sh
cd Mac
sh scripts/test-calls-contract.sh
```

期望：stdout 含 `PASS calls_fixtures` 与 `CallsContract: all assertions passed`；退出码 `0`。

## 铁律

- 不为绿放宽断言；失败即 FAIL。
- 零产品业务码；本目录为测试夹具 / harness。
- 主路径仅 `/api/devices/{id}/calls*` 控制面；禁止 MDD、禁止 WebRTC/SSE/media。
- 禁止把 HTTPS 网关请求塞进本机 UDS `APIClient`。
