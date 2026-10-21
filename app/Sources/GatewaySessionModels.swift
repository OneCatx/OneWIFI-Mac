import Foundation

/// 鉴权 DTO：字段仅取自 WEB `server_auth.go` / Android `auth_dto.dart`，禁造键。
struct GatewayAuthSession: Equatable {
    let username: String
    let sessionToken: String
    let expiresAt: Date

    /// Login：要求 `csrf_token` + `status == "ok"` + `authenticated` + `session_token` + `user.username` + `expires_at`。
    static func parseLogin(_ value: Any) throws -> GatewayAuthSession {
        let data = try asMap(value)
        _ = try requireNonEmptyString(data["csrf_token"])
        guard (data["status"] as? String) == "ok" else { throw GatewayHTTPError.dtoShape }
        return try parseSessionFields(data)
    }

    /// Session（纯 Bearer）：**不要求** `csrf_token`。
    static func parseSession(_ value: Any) throws -> GatewayAuthSession {
        try parseSessionFields(try asMap(value))
    }

    private static func parseSessionFields(_ data: [String: Any]) throws -> GatewayAuthSession {
        let user = try asMap(data["user"])
        let username = try requireNonEmptyString(user["username"])
        let sessionToken = try requireNonEmptyString(data["session_token"])
        guard data["authenticated"] as? Bool == true else { throw GatewayHTTPError.dtoShape }
        let expiresAt = try parseRFC3339(data["expires_at"])
        return GatewayAuthSession(username: username, sessionToken: sessionToken, expiresAt: expiresAt)
    }
}

struct GatewayLogoutResult {
    static func parse(_ value: Any) throws {
        let data = try asMap(value)
        guard data["logged_out"] as? Bool == true else { throw GatewayHTTPError.dtoShape }
    }
}

enum GatewaySessionPhase: Equatable {
    case booting
    case signedOut
    case signedIn(username: String, expiresAt: Date)
    case failed(message: String)
}

private func asMap(_ value: Any?) throws -> [String: Any] {
    guard let map = value as? [String: Any] else { throw GatewayHTTPError.dtoShape }
    return map
}

private func requireNonEmptyString(_ value: Any?) throws -> String {
    guard let text = value as? String else { throw GatewayHTTPError.dtoShape }
    let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
    if trimmed.isEmpty { throw GatewayHTTPError.dtoShape }
    return trimmed
}

private func parseRFC3339(_ value: Any?) throws -> Date {
    let text = try requireNonEmptyString(value)
    let withFraction = ISO8601DateFormatter()
    withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let plain = ISO8601DateFormatter()
    plain.formatOptions = [.withInternetDateTime]
    if let date = withFraction.date(from: text) { return date }
    if let date = plain.date(from: text) { return date }
    throw GatewayHTTPError.dtoShape
}
