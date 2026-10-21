import Combine
import Foundation

/// Owns the call-control state shared by the module and reader VoWiFi paths.
/// Media routing stays in `DashboardStore`; this store serializes signalling,
/// endpoint selection and DTMF so late replies cannot revive an ended call.
@MainActor
final class CallRuntimeStore: ObservableObject {
    @Published var status = CallStatus(state: "idle", number: nil, incoming: false, active: false)
    @Published var answerInFlight = false
    @Published var endpoint = "module"
    @Published var readerState: ReaderVoWiFiCallState?
    @Published var dialNumber = ""
    @Published var error: String?

    var readerAvailable: (() -> Bool)?
    var readerIMSRegistered: (() -> Bool)?
    var moduleVoWiFiActive: (() -> Bool)?
    var moduleCallModeReady: (() -> Bool)?
    var onStatusChange: ((CallStatus, CallStatus) -> Void)?
    var onPrepareHangup: ((String) -> Void)?
    var onModuleHangupWarning: ((String) -> Void)?

    private var resolvedEndpoint: String {
        moduleVoWiFiActive?() == true ? "module-vowifi" : endpoint
    }

    private let backend: BackendProcess
    private var readerGeneration: UInt64 = 0
    private var readerStatusFailures = 0
    private var pollRevision: UInt64 = 0
    private var actionRevision: UInt64 = 0
    private var statusRefreshInFlight = false
    private var dtmfTask: Task<Void, Never>?
    private var pendingDTMF: [(digit: String, endpoint: String, generation: UInt64)] = []

    init(backend: BackendProcess) {
        self.backend = backend
    }

    func reset() {
        pollRevision &+= 1
        actionRevision &+= 1
        statusRefreshInFlight = false
        answerInFlight = false
        readerStatusFailures = 0
        readerGeneration = 0
        assignReaderState(nil)
        error = nil
        cancelDTMF()
        publish(CallStatus(state: "idle", number: nil, incoming: false, active: false))
    }

    func acceptModuleStatus(_ newStatus: CallStatus) {
        guard endpoint == "module" else { return }
        publish(newStatus)
    }

    func poll() {
        guard case .running = backend.state, !statusRefreshInFlight else { return }
        if moduleVoWiFiActive?() == true {
            assignEndpoint("module-vowifi")
        } else if endpoint == "module-vowifi" {
            assignEndpoint("module")
        }
        pollRevision &+= 1
        let revision = pollRevision
        statusRefreshInFlight = true
        Task {
            defer {
                if revision == pollRevision { statusRefreshInFlight = false }
            }
            var readerResponse: ReaderVoWiFiCallResponse?
            if readerAvailable?() == true || readerIMSRegistered?() == true
                || moduleVoWiFiActive?() == true || readerState != nil {
                readerResponse = try? await APIClient().get("api/vowifi/calls/status")
                guard revision == pollRevision else { return }
                if let readerResponse {
                    readerStatusFailures = 0
                    assignReaderState(readerResponse.call)
                    readerGeneration = readerResponse.generation
                    if readerResponse.source == "module" {
                        assignEndpoint("module-vowifi")
                    } else if Self.activeReaderStates.contains(readerResponse.call.state) {
                        assignEndpoint("reader")
                    }
                } else {
                    readerStatusFailures += 1
                    if readerStatusFailures >= 3,
                       readerAvailable?() != true,
                       moduleVoWiFiActive?() != true {
                        assignReaderState(nil)
                        readerGeneration = 0
                        if endpoint == "reader" || endpoint == "module-vowifi" {
                            assignEndpoint("module")
                        }
                    }
                }
            } else {
                guard revision == pollRevision else { return }
                readerStatusFailures = 0
                assignReaderState(nil)
                readerGeneration = 0
            }

            if Self.isVoWiFiEndpoint(endpoint) {
                publishReaderStatus(readerResponse?.call ?? readerState)
                return
            }
            do {
                let response: CallStatus = try await APIClient().get("api/call/status")
                guard revision == pollRevision else { return }
                publish(response)
            } catch {
                guard revision == pollRevision else { return }
                publish(CallStatus(state: "unknown", number: status.number, incoming: false, active: false))
            }
        }
    }

    func dial(_ number: String) {
        let trimmed = number.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        let selectedEndpoint = resolvedEndpoint
        assignEndpoint(selectedEndpoint)
        let generation = readerGeneration
        let revision = beginAction()
        error = nil
        Task {
            do {
                if Self.isVoWiFiEndpoint(selectedEndpoint) {
                    let response: ReaderVoWiFiCallResponse = try await APIClient().send(
                        "api/vowifi/calls/dial",
                        body: ReaderVoWiFiCallRequest(peer: trimmed, generation: generation))
                    guard revision == actionRevision else { return }
                    assignReaderState(response.call)
                    readerGeneration = response.generation
                } else {
                    let _: CallActionResult = try await APIClient().send(
                        "api/call/dial",
                        body: CallDialRequest(number: trimmed))
                    guard revision == actionRevision else { return }
                }
                poll()
            } catch {
                guard revision == actionRevision else { return }
                self.error = "拨号失败：\(error.localizedDescription)"
            }
        }
    }

    func answer() {
        guard !answerInFlight else { return }
        let selectedEndpoint = resolvedEndpoint
        guard status.isIncoming else {
            error = "当前没有可接听的来电。"
            return
        }
        if Self.isVoWiFiEndpoint(selectedEndpoint), !ReaderCallReadiness.canAnswer(readerState) {
            error = "VoWiFi 来电信令尚未就绪，暂时不能接听。"
            return
        }
        guard Self.isVoWiFiEndpoint(selectedEndpoint) || moduleCallModeReady?() == true else {
            error = "通话模式尚未就绪，暂时不能接听。"
            return
        }
        assignEndpoint(selectedEndpoint)
        let generation = readerGeneration
        let revision = beginAction()
        error = nil
        answerInFlight = true
        Task {
            defer {
                if revision == actionRevision { answerInFlight = false }
            }
            do {
                if Self.isVoWiFiEndpoint(selectedEndpoint) {
                    let response: ReaderVoWiFiCallResponse = try await APIClient().send(
                        "api/vowifi/calls/answer",
                        body: ReaderVoWiFiCallRequest(peer: nil, generation: generation))
                    guard revision == actionRevision else { return }
                    assignReaderState(response.call)
                    readerGeneration = response.generation
                } else {
                    let _: CallActionResult = try await APIClient().send("api/call/answer")
                    guard revision == actionRevision else { return }
                }
                poll()
            } catch {
                guard revision == actionRevision else { return }
                self.error = "接听失败：\(error.localizedDescription)"
            }
        }
    }

    func hangup() {
        let selectedEndpoint = resolvedEndpoint
        assignEndpoint(selectedEndpoint)
        let generation = readerGeneration
        let revision = beginAction()
        error = nil
        answerInFlight = false
        cancelDTMF()
        onPrepareHangup?(selectedEndpoint)
        Task {
            do {
                if Self.isVoWiFiEndpoint(selectedEndpoint) {
                    let response: ReaderVoWiFiCallResponse = try await APIClient().send(
                        "api/vowifi/calls/hangup",
                        body: ReaderVoWiFiCallRequest(peer: nil, generation: generation))
                    guard revision == actionRevision else { return }
                    assignReaderState(response.call)
                    readerGeneration = response.generation
                } else {
                    let response: CallActionResult = try await APIClient().send("api/call/hangup")
                    guard revision == actionRevision else { return }
                    if let warning = response.warning, !warning.isEmpty {
                        onModuleHangupWarning?(warning)
                    }
                }
                poll()
            } catch {
                guard revision == actionRevision else { return }
                self.error = "挂断失败：\(error.localizedDescription)"
                poll()
            }
        }
    }

    /// Rejects a ringing call without treating the not-yet-established dialog as
    /// an active call. Reader VoWiFi must answer the pending INVITE with a final
    /// SIP response; sending BYE here is invalid and leaves the call ringing.
    func reject() {
        guard status.isIncoming else {
            error = "当前没有可拒接的来电。"
            return
        }
        let selectedEndpoint = resolvedEndpoint
        assignEndpoint(selectedEndpoint)
        let generation = readerGeneration
        let revision = beginAction()
        error = nil
        answerInFlight = false
        cancelDTMF()
        Task {
            do {
                if Self.isVoWiFiEndpoint(selectedEndpoint) {
                    let response: ReaderVoWiFiCallResponse = try await APIClient().send(
                        "api/vowifi/calls/reject",
                        body: ReaderVoWiFiCallRequest(peer: nil, generation: generation))
                    guard revision == actionRevision else { return }
                    assignReaderState(response.call)
                    readerGeneration = response.generation
                } else {
                    // The module backend exposes one termination action for both
                    // ringing and established calls.
                    let _: CallActionResult = try await APIClient().send("api/call/hangup")
                    guard revision == actionRevision else { return }
                }
                poll()
            } catch {
                guard revision == actionRevision else { return }
                self.error = "拒接失败：\(error.localizedDescription)"
                poll()
            }
        }
    }

    func sendDTMF(_ digit: String) {
        guard digit.count == 1, "0123456789*#".contains(digit) else { return }
        guard status.isActive else {
            error = "只有通话接通后才能发送按键。"
            return
        }
        let selectedEndpoint = resolvedEndpoint
        assignEndpoint(selectedEndpoint)
        pendingDTMF.append((digit, selectedEndpoint, readerGeneration))
        guard dtmfTask == nil else { return }
        let revision = actionRevision
        dtmfTask = Task { [weak self] in
            guard let self else { return }
            defer { self.dtmfTask = nil }
            while !Task.isCancelled,
                  revision == self.actionRevision,
                  self.status.isActive,
                  !self.pendingDTMF.isEmpty {
                let next = self.pendingDTMF.removeFirst()
                do {
                    if Self.isVoWiFiEndpoint(next.endpoint) {
                        let response: ReaderVoWiFiCallResponse = try await APIClient(timeoutInterval: 5).send(
                            "api/vowifi/calls/dtmf",
                            body: ReaderVoWiFiDTMFRequest(digit: next.digit, generation: next.generation))
                        guard revision == self.actionRevision else { return }
                        self.assignReaderState(response.call)
                        self.readerGeneration = response.generation
                    } else {
                        let _: CallActionResult = try await APIClient(timeoutInterval: 5).send(
                            "api/call/dtmf",
                            body: CallDTMFRequest(digit: next.digit))
                        guard revision == self.actionRevision else { return }
                    }
                } catch {
                    guard !Task.isCancelled, revision == self.actionRevision else { return }
                    self.error = "发送按键 \(next.digit) 失败：\(error.localizedDescription)"
                }
            }
            if !self.status.isActive { self.pendingDTMF.removeAll(keepingCapacity: true) }
        }
    }

    func cancelDTMF() {
        dtmfTask?.cancel()
        dtmfTask = nil
        pendingDTMF.removeAll(keepingCapacity: true)
    }

    private func beginAction() -> UInt64 {
        // An action changes the expected signalling state. Any poll that
        // started before it must not publish its now-stale response.
        actionRevision &+= 1
        pollRevision &+= 1
        statusRefreshInFlight = false
        return actionRevision
    }

    private func publishReaderStatus(_ state: ReaderVoWiFiCallState?) {
        let rawState = state?.state ?? "idle"
        let displayState: String
        switch rawState {
        case "outgoing", "connecting": displayState = "dialing"
        case "early": displayState = "alerting"
        case "ended": displayState = "idle"
        case "failed":
            displayState = "idle"
            if let detail = state?.lastError, !detail.isEmpty { error = "呼叫失败：\(detail)" }
        default: displayState = rawState
        }
        publish(CallStatus(
            state: displayState,
            number: state?.peer,
            incoming: rawState == "incoming",
            active: rawState == "active"))
    }

    /// Skip no-op `@Published` writes so idle polls do not fan out
    /// `objectWillChange` through `DashboardStore`.
    private func publish(_ newStatus: CallStatus) {
        let previous = status
        guard newStatus != previous else { return }
        status = newStatus
        if newStatus.isIdle && !previous.isIdle { cancelDTMF() }
        onStatusChange?(previous, newStatus)
    }

    private func assignReaderState(_ newState: ReaderVoWiFiCallState?) {
        guard readerState != newState else { return }
        readerState = newState
    }

    private func assignEndpoint(_ newEndpoint: String) {
        guard endpoint != newEndpoint else { return }
        endpoint = newEndpoint
    }

    private static let activeReaderStates: Set<String> = [
        "incoming", "outgoing", "early", "connecting", "active",
    ]

    private static func isVoWiFiEndpoint(_ endpoint: String) -> Bool {
        endpoint == "reader" || endpoint == "module-vowifi"
    }
}
