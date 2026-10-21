# ProxyCertContract（MAC-P5 / T35.6）

离线远端 **PROXY（upstream）+ CERT（HTTPS）** 契约夹具与断言（成功 + 失败）。**不连** live WEB、Keychain、本机 UDS。

## 真源

- `docs/客户端-Mac-WEB-远端能力优先级-MAC-P0-20261021.md` §2.5–§2.6
- `WEB后端/internal/server/proxy_api*.go` · `https_settings.go`
- 票：`TICKET-MAC-P5-PROXY` · `TICKET-MAC-P5-CERT`（禁扩 eSIM/Calls）

## 夹具

| 文件 | 覆盖 |
|---|---|
| `Fixtures/upstream_proxies_list_success.json` | GET `/api/upstream-proxies` 列表 |
| `Fixtures/upstream_proxies_list_empty.json` | 空列表 |
| `Fixtures/upstream_proxy_patch_enable_success.json` | PATCH `/{id}` `{enabled}` |
| `Fixtures/upstream_proxy_probe_success.json` | POST `…/actions/probe` |
| `Fixtures/https_settings_success.json` | GET `/api/settings/https` State |
| `Fixtures/https_certificate_pem_success.json` | GET certificate PEM 附件 meta |
| `Fixtures/upstream_proxies_invalid_proxy_id.json` | 400 `invalid_proxy_id` |
| `Fixtures/upstream_proxy_probe_invalid_addr.json` | 400 `invalid_proxy_addr` |
| `Fixtures/upstream_proxy_immutable_id.json` | 409 `immutable_proxy_id` |
| `Fixtures/upstream_proxies_unauthorized.json` | 401 |
| `Fixtures/upstream_proxies_method_not_allowed.json` | 405 |
| `Fixtures/https_unavailable.json` | 503 `https_unavailable` |
| `Fixtures/certificate_unavailable.json` | 500 `certificate_unavailable` |
| `Fixtures/https_unauthorized.json` | 401 |
| `Fixtures/invalid_certificate.json` | 400 `invalid_certificate` |
| `Fixtures/golden_path_sequence.json` | list→patch→probe + https→pem |
| `Fixtures/mdd_proxy_cert_forbidden.json` | 禁 MDD / local-proxy |
| `Fixtures/local_mihomo_not_upstream.json` | Keep C1 语义隔离 |

## 可复跑命令

### Windows（离线夹具，无 Xcode）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File docs\_mac_p5_logs\T35.6-run-proxy-cert-fixtures.ps1
```

期望：全部 `PASS …`；末行 `ProxyCertContract fixtures: ALL PASS (offline)`；退出码 `0`；`passes=20 failures=0`。

### macOS（Swift 全量）

```sh
cd Mac
sh scripts/test-proxy-cert-contract.sh
```

期望：stdout 含 `PASS proxy_cert_fixtures` 与 `ProxyCertContract: all assertions passed`；退出码 `0`。本 Windows 宿主 **NOT RUN**。

## 铁律

- 不为绿放宽断言；失败即 FAIL。
- 零产品业务码；本目录为测试夹具 / harness。
- 主路径仅 `/api/upstream-proxies*` + `/api/settings/https*`；禁止 MDD `/api/instances*`；禁止把 upstream 混成本机 Mihomo Tab。
- 禁止把 HTTPS 网关请求塞进本机 UDS `APIClient`。
