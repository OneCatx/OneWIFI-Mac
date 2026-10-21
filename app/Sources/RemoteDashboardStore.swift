import Combine
import Foundation

/// Remote dashboard + device overview store.
/// Uses `GatewaySessionStore` authenticated GET (shared `GatewayHTTPClient` + Bearer).
/// Does not touch local UDS `APIClient`.
@MainActor
final class RemoteDashboardStore: ObservableObject {
    static let shared = RemoteDashboardStore()

    enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case empty
        case failed(String)
    }

    @Published private(set) var host: RemoteHostOverview?
    @Published private(set) var devices: [RemoteDeviceOverview] = []
    @Published private(set) var state: LoadState = .idle
    @Published private(set) var selectedDetail: RemoteDeviceOverviewDetail?
    @Published private(set) var detailState: LoadState = .idle
    @Published private(set) var selectedDeviceId: String?

    private let session: GatewaySessionStore

    init(session: GatewaySessionStore = .shared) {
        self.session = session
    }

    func refresh() async {
        guard session.isSignedIn else {
            host = nil
            devices = []
            selectedDetail = nil
            selectedDeviceId = nil
            state = .idle
            detailState = .idle
            return
        }
        state = .loading
        do {
            async let hostData = session.getJSON(path: GatewayAPI.dashboardHost)
            async let devicesData = session.getJSON(path: GatewayAPI.dashboardDevices)
            let parsedHost = try RemoteHostOverview.parse(await hostData)
            let parsedDevices = try RemoteDeviceOverview.parseList(await devicesData)
            host = parsedHost
            devices = parsedDevices
            state = parsedDevices.isEmpty ? .empty : .loaded
            if let selectedDeviceId,
               !parsedDevices.contains(where: { $0.id == selectedDeviceId }) {
                clearSelection()
            }
        } catch {
            host = nil
            devices = []
            state = .failed(userFacingGatewayMessage(error))
            if sessionShouldClearSelection(after: error) {
                clearSelection()
            }
        }
    }

    func selectDevice(_ id: String) async {
        selectedDeviceId = id
        await loadOverview(deviceId: id)
    }

    func reloadSelectedOverview() async {
        guard let id = selectedDeviceId else { return }
        await loadOverview(deviceId: id)
    }

    func clearSelection() {
        selectedDeviceId = nil
        selectedDetail = nil
        detailState = .idle
    }

    private func loadOverview(deviceId: String) async {
        guard session.isSignedIn else {
            detailState = .idle
            selectedDetail = nil
            return
        }
        detailState = .loading
        do {
            let data = try await session.getJSON(path: GatewayAPI.deviceOverview(deviceId))
            selectedDetail = try RemoteDeviceOverviewDetail.parse(data, expectedDeviceId: deviceId)
            detailState = .loaded
        } catch {
            selectedDetail = nil
            detailState = .failed(userFacingGatewayMessage(error))
        }
    }

    private func sessionShouldClearSelection(after error: any Error) -> Bool {
        if let gateway = error as? GatewayHTTPError {
            return gateway.statusCode == 401 || gateway.code == "unauthorized" || gateway.code == "session_expired"
        }
        return false
    }
}
