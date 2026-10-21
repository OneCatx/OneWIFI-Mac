import Combine
import Foundation

/// Remote HTTPS certificate status store (read-only).
/// Uses `GatewaySessionStore` authenticated GET (shared `GatewayHTTPClient` + Bearer).
/// Does not download PEM, upload, reissue, or delete in this ticket.
@MainActor
final class RemoteCertificateStore: ObservableObject {
    static let shared = RemoteCertificateStore()

    enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case empty
        case failed(String)
    }

    @Published private(set) var httpsState: RemoteHttpsState?
    @Published private(set) var state: LoadState = .idle

    private let session: GatewaySessionStore

    init(session: GatewaySessionStore = .shared) {
        self.session = session
    }

    func refresh() async {
        guard session.isSignedIn else {
            httpsState = nil
            state = .idle
            return
        }
        state = .loading
        do {
            let data = try await session.getJSON(path: GatewayAPI.settingsHttps)
            let parsed = try RemoteHttpsState.parse(data)
            httpsState = parsed
            state = parsed.hasCertificateMaterial || parsed.enabled ? .loaded : .empty
        } catch {
            httpsState = nil
            state = .failed(userFacingRemoteCertificateMessage(error))
        }
    }

    func resetForSignOut() {
        httpsState = nil
        state = .idle
    }
}

func userFacingRemoteCertificateMessage(_ error: any Error) -> String {
    if let gateway = error as? GatewayHTTPError {
        switch gateway.code {
        case "https_unavailable":
            return "远端 HTTPS 设置不可用"
        case "certificate_unavailable":
            return "远端证书不可用"
        case "https_update_failed":
            return "远端 HTTPS 更新失败"
        default:
            break
        }
    }
    return userFacingGatewayMessage(error)
}
