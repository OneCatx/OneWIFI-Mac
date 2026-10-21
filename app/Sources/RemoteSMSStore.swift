import Combine
import Foundation

/// Remote SMS store: contacts list, thread detail, send.
/// Uses `GatewaySessionStore` authenticated GET/POST (shared `GatewayHTTPClient` + Bearer).
/// Does not touch local UDS `APIClient` / `SMSStore`.
@MainActor
final class RemoteSMSStore: ObservableObject {
    static let shared = RemoteSMSStore()

    enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case empty
        case failed(String)
    }

    @Published private(set) var contacts: [RemoteSMSContact] = []
    @Published private(set) var listState: LoadState = .idle
    @Published var deviceFilter: String = "all"
    @Published private(set) var selectedKey: RemoteSMSThreadKey?
    @Published private(set) var messages: [RemoteSMSMessage] = []
    @Published private(set) var threadState: LoadState = .idle
    @Published private(set) var sendBusy = false
    @Published var sendError: String?
    @Published var selectedSendDeviceId: String = ""

    private let session: GatewaySessionStore

    init(session: GatewaySessionStore = .shared) {
        self.session = session
    }

    func refreshContacts() async {
        guard session.isSignedIn else {
            contacts = []
            listState = .idle
            clearThread()
            return
        }
        listState = .loading
        do {
            let path = RemoteSMSPathBuilder.contacts(deviceId: deviceFilter)
            let data = try await session.getJSON(path: path)
            let parsed = try RemoteSMSContact.parseList(data)
            contacts = parsed.sorted { lhs, rhs in
                (lhs.lastTimestamp ?? .distantPast) > (rhs.lastTimestamp ?? .distantPast)
            }
            listState = parsed.isEmpty ? .empty : .loaded
            if let selectedKey,
               !parsed.contains(where: { RemoteSMSThreadKey(contact: $0) == selectedKey }) {
                clearThread()
            }
        } catch {
            contacts = []
            listState = .failed(userFacingGatewayMessage(error))
            if shouldClearAfter(error) {
                clearThread()
            }
        }
    }

    func selectContact(_ contact: RemoteSMSContact) async {
        let key = RemoteSMSThreadKey(contact: contact)
        selectedKey = key
        if selectedSendDeviceId.isEmpty, !contact.deviceId.isEmpty {
            selectedSendDeviceId = contact.deviceId
        }
        await loadThread(key)
    }

    func reloadThread() async {
        guard let key = selectedKey else { return }
        await loadThread(key)
    }

    func clearThread() {
        selectedKey = nil
        messages = []
        threadState = .idle
        sendError = nil
    }

    func resetForSignOut() {
        contacts = []
        listState = .idle
        clearThread()
        sendBusy = false
        sendError = nil
    }

    @discardableResult
    func send(phone: String, message: String, deviceId: String) async -> Bool {
        let peer = phone.trimmingCharacters(in: .whitespacesAndNewlines)
        let body = message.trimmingCharacters(in: .whitespacesAndNewlines)
        let device = deviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        guard session.isSignedIn else {
            sendError = "尚未登录网关"
            return false
        }
        guard !peer.isEmpty else {
            sendError = "请填写收件号码"
            return false
        }
        guard !body.isEmpty else {
            sendError = "请填写短信内容"
            return false
        }
        guard !device.isEmpty else {
            sendError = "请选择发送设备"
            return false
        }
        sendBusy = true
        sendError = nil
        defer { sendBusy = false }
        do {
            let data = try await session.postJSON(
                path: GatewayAPI.smsSend,
                body: [
                    "device_id": device,
                    "phone": peer,
                    "message": body
                ]
            )
            _ = try RemoteSMSSendResult.parse(data)
            selectedSendDeviceId = device
            await refreshContacts()
            if let selectedKey {
                await loadThread(selectedKey)
            } else {
                let key = RemoteSMSThreadKey(peer: peer, deviceId: device)
                selectedKey = key
                await loadThread(key)
            }
            return true
        } catch {
            sendError = userFacingRemoteSMSMessage(error)
            return false
        }
    }

    private func loadThread(_ key: RemoteSMSThreadKey) async {
        guard session.isSignedIn else {
            threadState = .idle
            messages = []
            return
        }
        threadState = .loading
        do {
            let path = RemoteSMSPathBuilder.thread(key)
            let data = try await session.getJSON(path: path)
            let parsed = try RemoteSMSMessage.parseList(data)
            messages = parsed.sorted { $0.timestamp < $1.timestamp }
            threadState = parsed.isEmpty ? .empty : .loaded
        } catch {
            messages = []
            threadState = .failed(userFacingGatewayMessage(error))
        }
    }

    private func shouldClearAfter(_ error: any Error) -> Bool {
        if let gateway = error as? GatewayHTTPError {
            return gateway.statusCode == 401
                || gateway.code == "unauthorized"
                || gateway.code == "session_expired"
        }
        return false
    }
}

func userFacingRemoteSMSMessage(_ error: any Error) -> String {
    if let gateway = error as? GatewayHTTPError {
        switch gateway.code {
        case "sms_rate_limited":
            return "远端短信发送过于频繁，请稍后再试"
        case "device_required":
            return "请选择发送设备"
        case "blocked_destination":
            return gateway.errorDescription ?? "收件号码不可用"
        case "device_feature_unsupported":
            return "该远端设备不支持短信"
        case "sms_submission_failed", "sms_submission_unconfirmed",
             "ims_sms_submission_failed", "ims_sms_submission_unconfirmed":
            return gateway.errorDescription ?? "远端短信提交未确认"
        default:
            break
        }
    }
    return userFacingGatewayMessage(error)
}
