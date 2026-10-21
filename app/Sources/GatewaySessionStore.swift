import Foundation
import Combine

/// 网关 Session 生命周期：bootstrap / login / logout / 改 Origin。
/// Token 进 Keychain；HTTPS 仅经 `GatewayHTTPClient`，不碰本机 UDS `APIClient`。
@MainActor
final class GatewaySessionStore: ObservableObject {
    static let shared = GatewaySessionStore()

    @Published private(set) var phase: GatewaySessionPhase = .booting
    @Published private(set) var origin: URL = BaseUrlPolicy.defaultOrigin
    @Published var originText: String = BaseUrlPolicy.defaultOrigin.absoluteString
    @Published private(set) var isBusy = false
    @Published var statusMessage: String?

    private let originStore: GatewayOriginStoreing
    private let tokenStore: GatewayTokenStoreing
    private let client: GatewayHTTPClient
    /// T-M4: coalesce concurrent `.task { bootstrap() }` callers onto one GET session.
    private var bootstrapTask: Task<Void, Never>?

    init(
        originStore: GatewayOriginStoreing = CachedGatewayOriginStore(UserDefaultsGatewayOriginStore.shared),
        tokenStore: GatewayTokenStoreing = CachedGatewayTokenStore(GatewayKeychainTokenStore.shared),
        client: GatewayHTTPClient = GatewayHTTPClient()
    ) {
        self.originStore = originStore
        self.tokenStore = tokenStore
        self.client = client
    }

    var isSignedIn: Bool {
        if case .signedIn = phase { return true }
        return false
    }

    var signedInUsername: String? {
        if case .signedIn(let username, _) = phase { return username }
        return nil
    }

    /// Session restore for remote pages. Concurrent callers await the same in-flight
    /// work; once `.signedIn`, later page `.task` calls skip repeat `GET /api/auth/session`.
    /// Cold start / `.signedOut` / `.booting` still hit the network (not Keychain-only optimistic).
    func bootstrap() async {
        if let bootstrapTask {
            await bootstrapTask.value
            return
        }
        if case .signedIn = phase {
            return
        }
        let task = Task { @MainActor in
            await self.executeBootstrap()
        }
        bootstrapTask = task
        await task.value
        bootstrapTask = nil
    }

    private func executeBootstrap() async {
        guard !isBusy else { return }
        isBusy = true
        defer { isBusy = false }
        do {
            let loaded = try originStore.read()
            origin = loaded
            originText = loaded.absoluteString
            let token = try tokenStore.read(loaded)
            guard let token, !token.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                await client.setBearerToken(nil)
                phase = .signedOut
                return
            }
            await client.setBearerToken(token)
            let session = try await client.fetchSession(origin: loaded)
            // 若服务端轮转了 token（cookie 路径），Bearer 路径通常原样回传；仍按返回值写回。
            try tokenStore.write(loaded, token: session.sessionToken)
            await client.setBearerToken(session.sessionToken)
            phase = .signedIn(username: session.username, expiresAt: session.expiresAt)
            statusMessage = nil
        } catch {
            // 本地 token 失效：清 Keychain，回到未登录。
            try? tokenStore.clear(origin)
            await client.setBearerToken(nil)
            phase = .signedOut
            statusMessage = userFacingGatewayMessage(error)
        }
    }

    func saveOrigin(_ raw: String) async -> Bool {
        do {
            let normalized = try BaseUrlPolicy.parseAndValidate(raw)
            let previous = origin
            try originStore.write(normalized)
            origin = normalized
            originText = normalized.absoluteString
            if previous.absoluteString != normalized.absoluteString {
                try? tokenStore.clear(previous)
                await client.setBearerToken(nil)
                phase = .signedOut
            }
            statusMessage = nil
            if BaseUrlPolicy.isPublicCleartext(normalized) {
                statusMessage = "当前为公网 HTTP，明文传输不安全。建议改用 HTTPS 或局域网地址。"
            }
            return true
        } catch {
            statusMessage = userFacingGatewayMessage(error)
            return false
        }
    }

    func login(username: String, password: String) async {
        guard !isBusy else { return }
        let user = username.trimmingCharacters(in: .whitespacesAndNewlines)
        if user.isEmpty || password.isEmpty {
            statusMessage = "请输入用户名和密码"
            return
        }
        isBusy = true
        defer { isBusy = false }
        do {
            if originText.trimmingCharacters(in: .whitespacesAndNewlines) != origin.absoluteString {
                guard await saveOrigin(originText) else { return }
            }
            let session = try await client.login(origin: origin, username: user, password: password)
            try tokenStore.write(origin, token: session.sessionToken)
            // 禁止日志打印 session_token
            phase = .signedIn(username: session.username, expiresAt: session.expiresAt)
            statusMessage = nil
        } catch {
            phase = .signedOut
            statusMessage = userFacingGatewayMessage(error)
        }
    }

    func logout() async {
        guard !isBusy else { return }
        isBusy = true
        defer { isBusy = false }
        let currentOrigin = origin
        do {
            try await client.logout(origin: currentOrigin)
        } catch {
            // 本地清 token 权威；后端失败仍落已登出。
            statusMessage = userFacingGatewayMessage(error)
        }
        try? tokenStore.clear(currentOrigin)
        await client.setBearerToken(nil)
        phase = .signedOut
    }

    /// Authenticated GET for remote read paths (dashboard / devices / overview / SMS).
    /// Reuses the same `GatewayHTTPClient` + Bearer as login; never UDS `APIClient`.
    func getJSON(path: String) async throws -> Any {
        guard isSignedIn else {
            throw GatewayHTTPError.unauthorizedMissingToken
        }
        return try await client.sendJSON(
            origin: origin,
            method: "GET",
            path: path,
            authMode: .bearer
        )
    }

    /// Authenticated POST JSON for remote write paths (e.g. `/api/sms/send`).
    /// Reuses the same `GatewayHTTPClient` + Bearer; never UDS `APIClient`.
    func postJSON(path: String, body: [String: String]) async throws -> Any {
        guard isSignedIn else {
            throw GatewayHTTPError.unauthorizedMissingToken
        }
        return try await client.sendJSON(
            origin: origin,
            method: "POST",
            path: path,
            body: Dictionary(uniqueKeysWithValues: body.map { ($0.key, $0.value as Any) }),
            authMode: .bearer
        )
    }

    /// Authenticated PATCH JSON for remote partial updates (e.g. upstream `{enabled}`).
    /// Reuses the same `GatewayHTTPClient` + Bearer; never UDS `APIClient`.
    func patchJSON(path: String, body: [String: Any]) async throws -> Any {
        guard isSignedIn else {
            throw GatewayHTTPError.unauthorizedMissingToken
        }
        return try await client.sendJSON(
            origin: origin,
            method: "PATCH",
            path: path,
            body: body,
            authMode: .bearer
        )
    }
}

func userFacingGatewayMessage(_ error: any Error) -> String {
    if let policy = error as? BaseUrlPolicy.PolicyError {
        return policy.errorDescription ?? "服务地址无效"
    }
    if let gateway = error as? GatewayHTTPError {
        return gateway.errorDescription ?? "网关请求失败"
    }
    if let keychain = error as? GatewayKeychainError {
        return keychain.errorDescription ?? "无法保存凭证"
    }
    if let urlError = error as? URLError {
        switch urlError.code {
        case .timedOut:
            return "连接网关超时，请检查地址与网络"
        case .cannotConnectToHost, .networkConnectionLost, .notConnectedToInternet:
            return "无法连接网关，请确认地址与服务可用"
        case .cancelled:
            return "操作已取消"
        default:
            break
        }
    }
    return error.localizedDescription
}
