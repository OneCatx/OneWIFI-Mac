import Foundation

/// WEB 路径常量真源：对齐 Android/iOS `gateway_api.dart`。
/// Mac 远端主路径跟此表；禁止 MDD `/api/instances*`。
enum GatewayAPI {
    // Auth — WEB `server_auth.go`（Bearer session_token；纯 Bearer 写操作 CSRF 豁免）
    static let login = "/api/auth/login"
    static let logout = "/api/auth/logout"
    static let session = "/api/auth/session"

    // Devices / dashboard — P3 预备（本票仅鉴权接线，不造未承诺字段）
    static let dashboardHost = "/api/dashboard/host"
    static let dashboardDevices = "/api/dashboard/devices"
    static let devices = "/api/devices"
    static let devicesDiscovered = "/api/devices/discovered"
    static let devicesRescan = "/api/devices/actions/rescan"
    static func device(_ id: String) -> String { "/api/devices/\(id)" }
    static func deviceOverview(_ id: String) -> String { "/api/devices/\(id)/overview" }

    // SMS — P4 预备（全局路径，非 MDD）
    static let smsContacts = "/api/sms/contacts"
    static let smsThread = "/api/sms/thread"
    static let smsSend = "/api/sms/send"

    // Upstream proxies — P5（跟 WEB；≠ 本机 Mihomo）
    static let upstreamProxies = "/api/upstream-proxies"
    static func upstreamProxy(_ id: String) -> String {
        "/api/upstream-proxies/\(id)"
    }

    // HTTPS / certificates — P5 只读状态（跟 WEB `/api/settings/https`）
    static let settingsHttps = "/api/settings/https"

    // Calls control plane — RP3（跟 WEB call_api + gateway_api.dart deviceCalls*）
    // 不含 webrtc / media / events / push。
    static func deviceCalls(_ id: String) -> String { "/api/devices/\(id)/calls" }
    static func deviceCallsDial(_ id: String) -> String { "/api/devices/\(id)/calls/dial" }
    static func deviceCallsAnswer(_ id: String) -> String { "/api/devices/\(id)/calls/answer" }
    static func deviceCallsHangup(_ id: String) -> String { "/api/devices/\(id)/calls/hangup" }
    static func deviceCallsDtmf(_ id: String) -> String { "/api/devices/\(id)/calls/dtmf" }
    static func deviceCallsHistory(_ id: String) -> String { "/api/devices/\(id)/calls/history" }

    static let health = "/api/health"
}

/// HTTP 头名：对齐 `GatewayHeaders`；CSRF 仅 cookie 会话写路径需要。
enum GatewayHeaders {
    static let csrf = "X-CSRF-Token"
    static let contentType = "Content-Type"
    static let accept = "Accept"
    static let authorization = "Authorization"
}
