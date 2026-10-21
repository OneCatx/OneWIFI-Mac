import Combine
import Foundation

/// Remote Calls control-plane store: live list, history, dial/answer/hangup (+ optional DTMF).
/// Uses `GatewaySessionStore` authenticated GET/POST (shared `GatewayHTTPClient` + Bearer).
/// Does not touch local UDS `APIClient` / `CallView` / WebRTC media.
@MainActor
final class RemoteCallsStore: ObservableObject {
    static let shared = RemoteCallsStore()

    enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case empty
        case failed(String)
    }

    @Published var selectedDeviceId: String = ""
    @Published private(set) var liveCalls: [RemoteCall] = []
    @Published private(set) var liveState: LoadState = .idle
    @Published private(set) var history: [RemoteCallHistoryItem] = []
    @Published private(set) var historyState: LoadState = .idle
    @Published private(set) var actionBusy = false
    @Published var actionError: String?
    @Published var dialNumber: String = ""
    @Published var dtmfDigits: String = ""

    private let session: GatewaySessionStore

    init(session: GatewaySessionStore = .shared) {
        self.session = session
    }

    func refreshAll() async {
        await refreshLive()
        await refreshHistory()
    }

    func refreshLive() async {
        let deviceId = selectedDeviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            liveCalls = []
            liveState = .idle
            return
        }
        guard !deviceId.isEmpty else {
            liveCalls = []
            liveState = .idle
            return
        }
        liveState = .loading
        do {
            let data = try await session.getJSON(path: GatewayAPI.deviceCalls(deviceId))
            let parsed = try RemoteCall.parseListEnvelope(data)
            liveCalls = parsed.sorted { $0.startedAt > $1.startedAt }
            liveState = parsed.isEmpty ? .empty : .loaded
        } catch {
            liveCalls = []
            liveState = .failed(userFacingRemoteCallsMessage(error))
        }
    }

    func refreshHistory() async {
        let deviceId = selectedDeviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            history = []
            historyState = .idle
            return
        }
        guard !deviceId.isEmpty else {
            history = []
            historyState = .idle
            return
        }
        historyState = .loading
        do {
            let data = try await session.getJSON(path: GatewayAPI.deviceCallsHistory(deviceId))
            let parsed = try RemoteCallHistoryItem.parseListEnvelope(data)
            history = parsed.sorted { $0.startedAt > $1.startedAt }
            historyState = parsed.isEmpty ? .empty : .loaded
        } catch {
            history = []
            historyState = .failed(userFacingRemoteCallsMessage(error))
        }
    }

    @discardableResult
    func dial() async -> Bool {
        let deviceId = selectedDeviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        let number = dialNumber.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            actionError = "尚未登录网关"
            return false
        }
        guard !deviceId.isEmpty else {
            actionError = "请选择远端设备"
            return false
        }
        guard !number.isEmpty else {
            actionError = "请填写拨号号码"
            return false
        }
        return await runAction {
            let data = try await session.postJSON(
                path: GatewayAPI.deviceCallsDial(deviceId),
                body: ["number": number]
            )
            _ = try RemoteCall.parse(data)
            await refreshLive()
            return true
        }
    }

    @discardableResult
    func answer(callId: String) async -> Bool {
        let deviceId = selectedDeviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        let id = callId.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            actionError = "尚未登录网关"
            return false
        }
        guard !deviceId.isEmpty else {
            actionError = "请选择远端设备"
            return false
        }
        guard !id.isEmpty else {
            actionError = "通话 ID 无效"
            return false
        }
        return await runAction {
            let data = try await session.postJSON(
                path: GatewayAPI.deviceCallsAnswer(deviceId),
                body: ["id": id]
            )
            _ = try RemoteCall.parse(data)
            await refreshLive()
            return true
        }
    }

    @discardableResult
    func hangup(callId: String) async -> Bool {
        let deviceId = selectedDeviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        let id = callId.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            actionError = "尚未登录网关"
            return false
        }
        guard !deviceId.isEmpty else {
            actionError = "请选择远端设备"
            return false
        }
        guard !id.isEmpty else {
            actionError = "通话 ID 无效"
            return false
        }
        return await runAction {
            let data = try await session.postJSON(
                path: GatewayAPI.deviceCallsHangup(deviceId),
                body: ["id": id]
            )
            _ = try RemoteCallHangupResult.parse(data)
            await refreshAll()
            return true
        }
    }

    @discardableResult
    func sendDTMF(callId: String) async -> Bool {
        let deviceId = selectedDeviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        let id = callId.trimmingCharacters(in: .whitespacesAndNewlines)
        let digits = dtmfDigits.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            actionError = "尚未登录网关"
            return false
        }
        guard !deviceId.isEmpty else {
            actionError = "请选择远端设备"
            return false
        }
        guard !id.isEmpty else {
            actionError = "通话 ID 无效"
            return false
        }
        guard !digits.isEmpty else {
            actionError = "请填写 DTMF 数字"
            return false
        }
        return await runAction {
            let data = try await session.postJSON(
                path: GatewayAPI.deviceCallsDtmf(deviceId),
                body: ["id": id, "digits": digits]
            )
            _ = try RemoteCallDTMFResult.parse(data)
            dtmfDigits = ""
            return true
        }
    }

    func resetForSignOut() {
        selectedDeviceId = ""
        liveCalls = []
        liveState = .idle
        history = []
        historyState = .idle
        actionBusy = false
        actionError = nil
        dialNumber = ""
        dtmfDigits = ""
    }

    func ensureDeviceSelection(from devices: [RemoteDeviceOverview]) {
        if devices.isEmpty {
            selectedDeviceId = ""
            return
        }
        if devices.contains(where: { $0.id == selectedDeviceId }) {
            return
        }
        selectedDeviceId = devices[0].id
    }

    private func runAction(_ work: () async throws -> Bool) async -> Bool {
        guard !actionBusy else { return false }
        actionBusy = true
        actionError = nil
        defer { actionBusy = false }
        do {
            return try await work()
        } catch {
            actionError = userFacingRemoteCallsMessage(error)
            return false
        }
    }
}

func userFacingRemoteCallsMessage(_ error: any Error) -> String {
    if let gateway = error as? GatewayHTTPError {
        switch gateway.code {
        case "invalid_dial_number":
            return "拨号号码无效"
        case "invalid_call_id":
            return "通话 ID 无效"
        case "invalid_dtmf":
            return "DTMF 无效（需 0-9 * # A-D）"
        case "call_not_found", "not_found":
            return "通话不存在"
        case "device_not_found":
            return "远端设备不存在"
        case "call_state":
            return gateway.errorDescription ?? "当前通话状态不允许此操作"
        case "dtmf_unavailable":
            return "当前通话不支持 DTMF"
        case "vowifi_not_running", "vowifi_runtime_unavailable", "vowifi_runtime_stopped":
            return "远端 VoWiFi 运行时不可用"
        case "vowifi_busy":
            return "远端设备忙"
        case "vowifi_device_unavailable":
            return "远端设备不可用"
        case "cellular_voice_unavailable":
            return "蜂窝语音不可用"
        case "cellular_no_carrier":
            return "无蜂窝运营商"
        case "cellular_dial_refused":
            return "蜂窝拨号被拒绝"
        case "vowifi_error":
            return gateway.errorDescription ?? "远端通话失败"
        case "call_history_unavailable", "call_history_failed":
            return "远端通话记录不可用"
        default:
            break
        }
    }
    return userFacingGatewayMessage(error)
}
