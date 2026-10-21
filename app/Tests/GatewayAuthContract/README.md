# GatewayAuthContract（MAC-P2 / T32.6）

离线远端登录契约夹具与断言。**不连** live WEB、Keychain、本机 UDS。

## 真源

- `docs/客户端-Mac-WEB-远端能力优先级-MAC-P0-20261021.md` §2.1
- `docs/客户端-鉴权对齐差距-P2-20261020.md`
- `WEB后端/internal/server/server_auth.go`
- 移动端 `AuthSessionDto` / `BaseUrlPolicy`

## 夹具

| 文件 | 覆盖 |
|---|---|
| `Fixtures/login_success.json` | login 成功 DTO |
| `Fixtures/session_bearer_success.json` | Bearer session（**无** `csrf_token`） |
| `Fixtures/logout_success.json` | logout `logged_out:true` |
| `Fixtures/login_invalid_credentials.json` | 401 `invalid_credentials` |
| `Fixtures/login_too_many_attempts.json` | 429 + `Retry-After` |
| `Fixtures/login_https_required.json` | 403 `https_required` |
| `Fixtures/login_invalid_request.json` | 400 `invalid_request` |
| `Fixtures/session_unauthorized.json` | 401 `unauthorized` |
| `Fixtures/session_expired.json` | 401 `session_expired` |
| `Fixtures/logout_invalid_csrf.json` | 403 `invalid_csrf`（cookie 误路径文档） |
| `Fixtures/base_url_policy_cases.json` | BaseUrl 接受/拒绝/明文公网 |

## 可复跑命令

工作目录：`Mac/`（或仓库根对 PowerShell 脚本）。

### macOS（Swift 全量）

```sh
cd Mac
sh scripts/test-gateway-auth-contract.sh
```

期望：stdout 含 `PASS auth_fixtures` / `PASS base_url_policy` / `PASS isolation_headers` 与 `GatewayAuthContract: all assertions passed`；退出码 `0`。

### Windows（离线夹具，无 Xcode）

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File docs\_mac_p2_logs\T32.6-run-gateway-auth-fixtures.ps1
```

期望：全部 `PASS …`；末行 `GatewayAuthContract fixtures: ALL PASS (offline)`；退出码 `0`。

## 铁律

- 不为绿放宽断言；失败即 FAIL。
- 零产品业务码；本目录为测试夹具/ harness。
- 禁止把 HTTPS 网关请求塞进本机 UDS `APIClient`。
