import Combine
import Foundation

/// Remote upstream-proxy store: list + enable toggle.
/// Uses `GatewaySessionStore` authenticated GET/PATCH (shared `GatewayHTTPClient` + Bearer).
/// Does not touch local Mihomo / UDS `APIClient`.
@MainActor
final class RemoteUpstreamStore: ObservableObject {
    static let shared = RemoteUpstreamStore()

    enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case empty
        case failed(String)
    }

    @Published private(set) var proxies: [RemoteUpstreamProxy] = []
    @Published private(set) var state: LoadState = .idle
    @Published private(set) var toggleBusyId: String?
    @Published var actionError: String?

    private let session: GatewaySessionStore

    init(session: GatewaySessionStore = .shared) {
        self.session = session
    }

    func refresh() async {
        guard session.isSignedIn else {
            proxies = []
            state = .idle
            actionError = nil
            return
        }
        state = .loading
        actionError = nil
        do {
            let data = try await session.getJSON(path: GatewayAPI.upstreamProxies)
            let parsed = try RemoteUpstreamProxy.parseList(data)
            proxies = parsed.sorted { lhs, rhs in
                if lhs.enabled != rhs.enabled { return lhs.enabled && !rhs.enabled }
                return lhs.name.localizedCaseInsensitiveCompare(rhs.name) == .orderedAscending
            }
            state = parsed.isEmpty ? .empty : .loaded
        } catch {
            proxies = []
            state = .failed(userFacingRemoteUpstreamMessage(error))
        }
    }

    func setEnabled(id: String, enabled: Bool) async {
        let proxyId = id.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            actionError = "尚未登录网关"
            return
        }
        guard !proxyId.isEmpty else { return }
        guard toggleBusyId == nil else { return }
        toggleBusyId = proxyId
        actionError = nil
        defer { toggleBusyId = nil }
        // Optimistic local flip; roll back on failure.
        let previous = proxies
        if let index = proxies.firstIndex(where: { $0.id == proxyId }) {
            let item = proxies[index]
            proxies[index] = RemoteUpstreamProxy(
                id: item.id,
                name: item.name,
                addr: item.addr,
                enabled: enabled
            )
        }
        do {
            _ = try await session.patchJSON(
                path: GatewayAPI.upstreamProxy(proxyId),
                body: ["enabled": enabled]
            )
            await refresh()
        } catch {
            proxies = previous
            actionError = userFacingRemoteUpstreamMessage(error)
        }
    }

    func resetForSignOut() {
        proxies = []
        state = .idle
        toggleBusyId = nil
        actionError = nil
    }
}

func userFacingRemoteUpstreamMessage(_ error: any Error) -> String {
    if let gateway = error as? GatewayHTTPError {
        switch gateway.code {
        case "invalid_proxy_id":
            return "代理 ID 无效"
        case "invalid_proxy_addr":
            return "代理地址无效"
        case "immutable_proxy_id":
            return "该代理 ID 不可修改"
        case "upstream_proxy_disabled":
            return "上游代理已禁用"
        default:
            break
        }
    }
    return userFacingGatewayMessage(error)
}
