import Combine
import Foundation

struct ModuleVoWiFiState: Equatable {
    var revision: UInt64
    var status: VoWiFiStatus?
    var loaded: Bool
    var error: String?
    var actionInFlight: Bool

    static let initial = ModuleVoWiFiState(
        revision: 0,
        status: nil,
        loaded: false,
        error: nil,
        actionInFlight: false)
}

/// DJI 模块 VoWiFi 的单一状态源。
///
/// 查询、启停及异步结果轮询均受 lifecycle generation 与 operation revision
/// 保护，后端重启或新操作开始后，旧任务不会再覆盖当前状态。
@MainActor
final class ModuleVoWiFiStore: ObservableObject {
    @Published private(set) var state = ModuleVoWiFiState.initial

    private var requestedEnabled: Bool?
    var toggleEnabled: Bool {
        if let requestedEnabled { return requestedEnabled }
        if state.error != nil, state.status?.vowifiActive != true { return false }
        if state.status?.state == "failed" || state.status?.state == "unavailable" { return false }
        return state.status?.imsWiFiCallingEnabled ?? false
    }

    /// Let the user stop an in-progress start request instead of trapping the
    /// switch in a disabled/loading state until the network timeout expires.
    var canCancelStart: Bool {
        state.actionInFlight && requestedEnabled == true
    }

    var moduleStatus: () -> DeviceStatus? = { nil }
    var onToast: (_ message: String, _ isSuccess: Bool, _ title: String?) -> Void = { _, _, _ in }

    private let backend: BackendProcess
    private var refreshTask: Task<Void, Never>?
    private var operationTask: Task<Void, Never>?
    private var lifecycleGeneration: UInt64 = 0
    private var refreshRevision: UInt64 = 0
    private var operationRevision: UInt64 = 0
    private var observedProfileICCID: String?
    private var consecutiveStatusReadFailures = 0
    private(set) var lastHealthRefreshAt: Date?
    private var cancellables = Set<AnyCancellable>()

    init(backend: BackendProcess) {
        self.backend = backend
        backend.$state
            .receive(on: RunLoop.main)
            .sink { [weak self] backendState in
                guard let self else { return }
                if case .running = backendState {
                    self.refresh()
                } else {
                    self.reset()
                }
            }
            .store(in: &cancellables)
    }

    deinit {
        refreshTask?.cancel()
        operationTask?.cancel()
    }

    func reset() {
        lifecycleGeneration &+= 1
        refreshRevision &+= 1
        operationRevision &+= 1
        refreshTask?.cancel()
        operationTask?.cancel()
        refreshTask = nil
        operationTask = nil
        requestedEnabled = nil
        observedProfileICCID = nil
        consecutiveStatusReadFailures = 0
        lastHealthRefreshAt = nil
        commit(status: nil, loaded: false, error: nil, actionInFlight: false)
    }

    /// Binds displayed VoWiFi state to the module's active SIM identity. A
    /// profile transition invalidates late responses and removes the previous
    /// SIM's status before querying the new profile.
    func observeActiveProfileICCID(_ iccid: String?) {
        guard let current = normalizedICCID(iccid), !current.isEmpty else { return }
        guard let previous = observedProfileICCID else {
            observedProfileICCID = current
            return
        }
        guard previous != current else { return }
        observedProfileICCID = current
        invalidateProfileScopedState()
        refresh()
    }

    /// The eSIM manager calls this after a profile switch completes (or the
    /// module reports that the switch may have completed). Do not keep the
    /// former profile's status visible while the dashboard catches up.
    func invalidateAfterProfileSwitch() {
        if let current = normalizedICCID(moduleStatus()?.iccid) {
            observedProfileICCID = current
        }
        invalidateProfileScopedState()
        refresh()
    }

    private func invalidateProfileScopedState() {
        refreshRevision &+= 1
        operationRevision &+= 1
        refreshTask?.cancel()
        operationTask?.cancel()
        refreshTask = nil
        operationTask = nil
        requestedEnabled = nil
        consecutiveStatusReadFailures = 0
        commit(status: nil, loaded: false, error: nil, actionInFlight: false)
    }

    func refresh() {
        guard case .running = backend.state else { return }
        if observedProfileICCID == nil {
            observedProfileICCID = normalizedICCID(moduleStatus()?.iccid)
        }
        if let hardwareStatus = moduleStatus()?.hardwareStatus, !hardwareStatus.isEmpty {
            refreshRevision &+= 1
            refreshTask?.cancel()
            refreshTask = nil
            requestedEnabled = nil
            commit(status: nil, loaded: true, error: nil, actionInFlight: state.actionInFlight)
            return
        }
        guard refreshTask == nil else { return }

        refreshRevision &+= 1
        let expectedRefresh = refreshRevision
        let expectedGeneration = lifecycleGeneration
        lastHealthRefreshAt = Date()
        refreshTask = Task { [weak self] in
            do {
                let latest: VoWiFiStatus = try await APIClient(timeoutInterval: 12)
                    .get("api/vowifi")
                guard let self,
                      self.isCurrentRefresh(expectedRefresh, generation: expectedGeneration) else { return }
                self.refreshTask = nil
                self.consecutiveStatusReadFailures = 0
                if !self.state.actionInFlight {
                    self.requestedEnabled = nil
                }
                let profileBoundStatus = self.statusBelongsToActiveProfile(latest) ? latest : nil
                self.commit(
                    status: profileBoundStatus,
                    loaded: true,
                    error: nil,
                    actionInFlight: self.state.actionInFlight)
            } catch {
                guard let self,
                      self.isCurrentRefresh(expectedRefresh, generation: expectedGeneration) else { return }
                self.refreshTask = nil
                let message = error.localizedDescription
                if isModuleDisconnectedError(message) {
                    self.requestedEnabled = nil
                    self.consecutiveStatusReadFailures = 0
                } else {
                    self.consecutiveStatusReadFailures += 1
                }
                self.commit(
                    status: isModuleDisconnectedError(message) || self.consecutiveStatusReadFailures >= 2
                        ? nil : self.state.status,
                    loaded: true,
                    error: isModuleDisconnectedError(message) ? nil : "读取 VoWiFi 状态失败：\(message)",
                    actionInFlight: self.state.actionInFlight)
            }
        }
    }

    func enable() {
        setEnabled(true)
    }

    func disable() {
        setEnabled(false)
    }

    private func setEnabled(_ enabled: Bool) {
        guard case .running = backend.state else { return }
        if state.actionInFlight {
            // A pending start is cancellable; duplicate starts and attempts to
            // reverse an already-running stop remain ignored.
            guard !enabled, requestedEnabled == true else { return }
        }
        // An earlier status read describes the pre-operation state. Invalidate it
        // before sending the mutation so it cannot overwrite the final result.
        refreshRevision &+= 1
        refreshTask?.cancel()
        refreshTask = nil
        operationRevision &+= 1
        let expectedOperation = operationRevision
        let expectedGeneration = lifecycleGeneration
        requestedEnabled = enabled
        let request = VoWiFiRequest(
            enable: enabled,
            wifiCallingMode: enabled ? 1 : nil)

        operationTask?.cancel()
        commit(status: state.status, loaded: state.loaded, error: nil, actionInFlight: true)
        operationTask = Task { [weak self] in
            guard let self else { return }
            do {
                let _: EmptyResponse = try await APIClient(timeoutInterval: 10)
                    .send("api/vowifi", body: request)
                try await self.pollResult(
                    operation: expectedOperation,
                    generation: expectedGeneration,
                    enabling: enabled)
            } catch is CancellationError {
                return
            } catch {
                guard self.isCurrentOperation(expectedOperation, generation: expectedGeneration) else { return }
                self.operationTask = nil
                self.requestedEnabled = nil
                let message = error.localizedDescription
                let displayMessage = isModuleDisconnectedError(message)
                    ? moduleDisconnectedFriendlyMessage
                    : "\(enabled ? "启用" : "关闭") VoWiFi 失败：\(message)"
                self.commit(
                    status: self.state.status,
                    loaded: self.state.loaded,
                    error: displayMessage,
                    actionInFlight: false)
                self.onToast(displayMessage, false, "Wi-Fi Calling")
            }
        }
    }

    private func pollResult(operation: UInt64, generation: UInt64, enabling: Bool) async throws {
        try await Task.sleep(for: .milliseconds(500))
        var operationFinished = false
        for _ in 0..<75 {
            try Task.checkCancellation()
            guard isCurrentOperation(operation, generation: generation) else {
                throw CancellationError()
            }

            // The POST only confirms that the session was launched. Track live
            // status until IMS registers so the switch cannot look successful
            // while ePDG, tunnel or IMS setup is still in progress.
            do {
                let result: VoWiFiResult = try await APIClient(timeoutInterval: 10)
                    .get("api/vowifi/result")
                guard isCurrentOperation(operation, generation: generation) else {
                    throw CancellationError()
                }
                operationFinished = result.done
                if result.done, let error = result.error, !error.isEmpty {
                    finishOperationFailure(error, status: result.status ?? state.status)
                    return
                }
            } catch is CancellationError {
                throw CancellationError()
            } catch {
                // A transient result read is retried; current status remains visible.
            }

            do {
                let status: VoWiFiStatus = try await APIClient(timeoutInterval: 10)
                    .get("api/vowifi")
                guard isCurrentOperation(operation, generation: generation) else {
                    throw CancellationError()
                }
                guard statusBelongsToActiveProfile(status) else {
                    commit(status: nil, loaded: true, error: nil, actionInFlight: true)
                    try await Task.sleep(for: .milliseconds(1_500))
                    continue
                }
                commit(status: status, loaded: true, error: nil, actionInFlight: true)

                if enabling {
                    if status.state == "failed" || status.state == "unavailable" {
                        let detail = status.detail?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                        finishOperationFailure(detail.isEmpty ? status.summary : detail, status: status)
                        return
                    }
                    if status.vowifiActive && status.imsRegistered {
                        let message: String
                        if status.smsReady == true && status.callReady == true {
                            message = "模块 Wi-Fi Calling 已开启，IMS、短信与通话通道就绪"
                        } else if status.callReady == true {
                            message = "模块 Wi-Fi Calling 已开启，IMS 与通话就绪；短信通道尚未就绪"
                        } else {
                            message = "模块 Wi-Fi Calling 已开启并完成 IMS 注册"
                        }
                        finishOperationSuccess(message, status: status)
                        return
                    }
                } else if operationFinished,
                          status.state == "disabled",
                          !status.imsWiFiCallingEnabled {
                    finishOperationSuccess("模块 Wi-Fi Calling 已关闭，原无线状态已恢复", status: status)
                    return
                }
            } catch is CancellationError {
                throw CancellationError()
            } catch {
                // Keep the last confirmed phase visible while status is retried.
            }
            try await Task.sleep(for: .milliseconds(1_500))
        }

        guard isCurrentOperation(operation, generation: generation) else {
            throw CancellationError()
        }
        operationTask = nil
        requestedEnabled = nil
        commit(
            status: state.status,
            loaded: state.loaded,
            error: enabling
                ? "模块 VoWiFi 尚未完成 IMS 注册，请检查当前阶段与错误详情"
                : "关闭模块 VoWiFi 超时；请刷新并核对无线状态",
            actionInFlight: false)
        refresh()
    }

    private func finishOperationSuccess(_ message: String, status: VoWiFiStatus) {
        operationTask = nil
        requestedEnabled = nil
        commit(status: status, loaded: true, error: nil, actionInFlight: false)
        onToast(message, true, "Wi-Fi Calling")
        refresh()
    }

    private func finishOperationFailure(_ message: String, status: VoWiFiStatus?) {
        operationTask = nil
        requestedEnabled = nil
        let detail = message.trimmingCharacters(in: .whitespacesAndNewlines)
        let displayMessage = detail.isEmpty ? "模块 Wi-Fi Calling 连接失败" : detail
        let currentStatus = status?.progressStep == 0 ? state.status : status
        commit(status: currentStatus, loaded: true, error: displayMessage, actionInFlight: false)
        onToast(displayMessage, false, "Wi-Fi Calling")
    }

    private func isCurrentRefresh(_ revision: UInt64, generation: UInt64) -> Bool {
        !Task.isCancelled &&
            lifecycleGeneration == generation &&
            refreshRevision == revision
    }

    private func isCurrentOperation(_ revision: UInt64, generation: UInt64) -> Bool {
        !Task.isCancelled &&
            lifecycleGeneration == generation &&
            operationRevision == revision
    }

    private func statusBelongsToActiveProfile(_ status: VoWiFiStatus) -> Bool {
        guard let active = normalizedICCID(moduleStatus()?.iccid),
              let bound = normalizedICCID(status.simICCID) else { return true }
        return active == bound
    }

    private func normalizedICCID(_ value: String?) -> String? {
        guard let value else { return nil }
        let digits = value.filter(\.isNumber)
        return digits.isEmpty ? nil : digits
    }

    private func commit(
        status: VoWiFiStatus?,
        loaded: Bool,
        error: String?,
        actionInFlight: Bool
    ) {
        state = ModuleVoWiFiState(
            revision: state.revision &+ 1,
            status: status,
            loaded: loaded,
            error: error,
            actionInFlight: actionInFlight)
    }

    private struct EmptyResponse: Decodable {}
}
