import Foundation

/// 独立 HTTPS 网关客户端。**禁止**与本机 UDS `APIClient` / `UnixSocketURLProtocol` 混用。
/// 鉴权：`Authorization: Bearer <session_token>`；纯 Bearer 写操作不附 CSRF（对齐移动端）。
actor GatewayHTTPClient {
    enum AuthMode {
        case none
        case bearer
    }

    private let session: URLSession
    private let trustDelegate: GatewayURLSessionTrustDelegate
    private var bearerToken: String?

    init(timeoutInterval: TimeInterval = 30) {
        let delegate = GatewayURLSessionTrustDelegate()
        self.trustDelegate = delegate
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = timeoutInterval
        config.timeoutIntervalForResource = max(60, timeoutInterval)
        config.httpCookieAcceptPolicy = .never
        config.httpShouldSetCookies = false
        // 不挂 UnixSocketURLProtocol：保持系统默认 HTTP(S) 栈，与本机 UDS APIClient 隔离。
        self.session = URLSession(configuration: config, delegate: delegate, delegateQueue: nil)
    }

    func setBearerToken(_ token: String?) {
        bearerToken = token
    }

    func bearerTokenValue() -> String? { bearerToken }

    func sendJSON(
        origin: URL,
        method: String,
        path: String,
        body: [String: Any]? = nil,
        authMode: AuthMode = .bearer
    ) async throws -> Any {
        let url = try Self.makeURL(origin: origin, path: path)
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: GatewayHeaders.accept)
        if let body {
            request.setValue("application/json; charset=utf-8", forHTTPHeaderField: GatewayHeaders.contentType)
            request.httpBody = try JSONSerialization.data(withJSONObject: body, options: [])
        }
        if authMode == .bearer {
            guard let token = bearerToken, !token.isEmpty else {
                throw GatewayHTTPError.unauthorizedMissingToken
            }
            request.setValue("Bearer \(token)", forHTTPHeaderField: GatewayHeaders.authorization)
        }

        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw GatewayHTTPError.invalidResponse
        }
        if http.statusCode >= 400 {
            throw Self.decodeError(status: http.statusCode, data: data, retryAfter: http.value(forHTTPHeaderField: "Retry-After"))
        }
        return try Self.decodeSuccessData(data)
    }

    func login(origin: URL, username: String, password: String) async throws -> GatewayAuthSession {
        let data = try await sendJSON(
            origin: origin,
            method: "POST",
            path: GatewayAPI.login,
            body: ["username": username as Any, "password": password as Any],
            authMode: .none
        )
        let session = try GatewayAuthSession.parseLogin(data)
        bearerToken = session.sessionToken
        return session
    }

    func fetchSession(origin: URL) async throws -> GatewayAuthSession {
        let data = try await sendJSON(
            origin: origin,
            method: "GET",
            path: GatewayAPI.session,
            authMode: .bearer
        )
        return try GatewayAuthSession.parseSession(data)
    }

    func logout(origin: URL) async throws {
        let data = try await sendJSON(
            origin: origin,
            method: "POST",
            path: GatewayAPI.logout,
            authMode: .bearer
        )
        try GatewayLogoutResult.parse(data)
        bearerToken = nil
    }

    private static func makeURL(origin: URL, path: String) throws -> URL {
        let normalizedOrigin = try BaseUrlPolicy.parseAndValidate(origin.absoluteString)
        guard var components = URLComponents(url: normalizedOrigin, resolvingAgainstBaseURL: false) else {
            throw GatewayHTTPError.invalidURL
        }
        let parts = path.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false)
        components.path = String(parts[0])
        if parts.count == 2 {
            components.percentEncodedQuery = String(parts[1])
        } else {
            components.query = nil
        }
        guard let url = components.url else { throw GatewayHTTPError.invalidURL }
        return url
    }

    private static func decodeSuccessData(_ data: Data) throws -> Any {
        if data.isEmpty { return [String: Any]() }
        let root = try JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])
        guard let map = root as? [String: Any] else { return root }
        if let nested = map["data"] { return nested }
        return map
    }

    private static func decodeError(status: Int, data: Data, retryAfter: String?) -> GatewayHTTPError {
        if let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            if let errorObj = root["error"] as? [String: Any] {
                let code = errorObj["code"] as? String
                let message = (errorObj["message"] as? String) ?? "请求失败"
                return .server(status: status, code: code, message: message, retryAfter: retryAfter)
            }
            if let message = root["error"] as? String {
                return .server(status: status, code: root["code"] as? String, message: message, retryAfter: retryAfter)
            }
        }
        return .httpStatus(status)
    }
}

enum GatewayHTTPError: LocalizedError {
    case invalidURL
    case invalidResponse
    case unauthorizedMissingToken
    case httpStatus(Int)
    case server(status: Int, code: String?, message: String, retryAfter: String?)
    case dtoShape

    var code: String? {
        switch self {
        case .server(_, let code, _, _): return code
        case .unauthorizedMissingToken: return "unauthorized"
        default: return nil
        }
    }

    var statusCode: Int? {
        switch self {
        case .httpStatus(let s), .server(let s, _, _, _): return s
        case .unauthorizedMissingToken: return 401
        default: return nil
        }
    }

    var errorDescription: String? {
        switch self {
        case .invalidURL: return "网关地址无效"
        case .invalidResponse: return "网关响应无效"
        case .unauthorizedMissingToken: return "尚未登录网关"
        case .httpStatus(let s): return "网关返回错误（HTTP \(s)）"
        case .server(_, let code, let message, let retryAfter):
            return Self.userMessage(code: code, fallback: message, retryAfter: retryAfter)
        case .dtoShape: return "网关响应字段不符合约定"
        }
    }

    private static func userMessage(code: String?, fallback: String, retryAfter: String?) -> String {
        switch code {
        case "invalid_credentials":
            return "用户名或密码错误"
        case "too_many_attempts":
            if let retryAfter, let seconds = Int(retryAfter), seconds > 0 {
                return "尝试过多，请 \(seconds) 秒后重试"
            }
            return "尝试过多，请稍后再试"
        case "https_required":
            return "此网关要求使用 HTTPS"
        case "unauthorized", "session_expired":
            return "会话已失效，请重新登录"
        case "invalid_csrf":
            return "安全校验失败，请重新登录"
        case "invalid_request":
            return "请求无效，请检查用户名与密码"
        default:
            return fallback.isEmpty ? "请求失败，请稍后重试" : fallback
        }
    }
}

/// 局域网自签：对 loopback / 私网主机放宽服务端信任（对齐移动端本地网关调试口径）。
/// 公网仍走系统校验。完整指纹钉扎属 G03，可后续加强。
final class GatewayURLSessionTrustDelegate: NSObject, URLSessionDelegate {
    func urlSession(
        _ session: URLSession,
        didReceive challenge: URLAuthenticationChallenge,
        completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void
    ) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust else {
            completionHandler(.performDefaultHandling, nil)
            return
        }
        let host = challenge.protectionSpace.host
        // 私网放宽受设置「证书信任」开关约束；公网始终系统校验。指纹钉扎属 G03 后续加强。
        if GatewayTrustPolicy.shouldRelaxTrust(forHost: host) {
            completionHandler(.useCredential, URLCredential(trust: trust))
            return
        }
        completionHandler(.performDefaultHandling, nil)
    }
}
