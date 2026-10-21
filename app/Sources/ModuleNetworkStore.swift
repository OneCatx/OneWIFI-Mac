import Combine
import Foundation

private enum NetworkRecoveryTrigger: Equatable {
    case wake
    case manual

    var delayNanoseconds: UInt64 {
        switch self {
        case .wake: return 12_000_000_000
        case .manual: return 0
        }
    }

    var waitingMessage: String {
        switch self {
        case .wake: return "正在等待 4G 网卡恢复…"
        case .manual: return "正在检查 4G 网卡…"
        }
    }

    var successMessage: String {
        switch self {
        case .wake: return "4G 网卡已自动恢复"
        case .manual: return "4G 网卡已恢复"
        }
    }
}

/// DJI 模块网络控制的单一状态源。
///
/// 流量、USB 网卡模式、诊断和重连均在此处串行化。DashboardStore 只负责
/// 把模块与通话状态回写到各自领域，避免多个页面各自启动重复网络任务。
@MainActor
final class ModuleNetworkStore: ObservableObject {
    @Published private(set) var traffic: TrafficSnapshot?
    @Published var usbnetEnabled = false
    @Published private(set) var usbnetLoaded = false
    @Published private(set) var busy = false
    @Published private(set) var recovering = false
    @Published private(set) var recoveryMessage: String?
    @Published private(set) var check4G: CheckResult?
    @Published private(set) var flightModeStatus: ModuleFlightModeStatus?
    @Published private(set) var flightModeLoaded = false
    @Published private(set) var flightModeBusy = false
    @Published private(set) var flightModeError: String?

    var isCallModeBusy: () -> Bool = { false }
    var onModuleStatus: (DeviceStatus) -> Void = { _ in }
    var onCallStatus: (CallStatus) -> Void = { _ in }
    var onServicesReload: () -> Void = {}
    var onDashboardRefresh: () -> Void = {}
    var onToast: (_ message: String, _ isSuccess: Bool, _ title: String?) -> Void = { _, _, _ in }

    private let backend: BackendProcess
    private var loadTask: Task<Void, Never>?
    private var actionTask: Task<Void, Never>?
    private var recoveryTask: Task<Void, Never>?
    private var flightModeTask: Task<Void, Never>?
    private var lifecycleGeneration: UInt64 = 0
    private var loadRevision: UInt64 = 0
    private var flightModeRevision: UInt64 = 0
    private var lastAutomaticRecoveryAt: Date?
    private var cancellables = Set<AnyCancellable>()

    private static let disconnectedError = "4G 模块网卡未连接"
    private static let automaticRecoveryCooldown: TimeInterval = 10 * 60

    init(backend: BackendProcess) {
        self.backend = backend
        backend.$state
            .receive(on: RunLoop.main)
            .sink { [weak self] state in
                guard let self else { return }
                if case .running = state {
                    self.loadUSBStatus()
                    self.refreshFlightMode()
                } else {
                    self.reset()
                }
            }
            .store(in: &cancellables)
    }

    deinit {
        loadTask?.cancel()
        actionTask?.cancel()
        recoveryTask?.cancel()
        flightModeTask?.cancel()
    }

    func updateTraffic(_ snapshot: TrafficSnapshot) {
        traffic = snapshot
    }

    func loadUSBStatus() {
        guard case .running = backend.state else { return }
        loadRevision &+= 1
        let expectedRevision = loadRevision
        let expectedGeneration = lifecycleGeneration
        loadTask?.cancel()
        loadTask = Task { [weak self] in
            do {
                let diagnostic: NetworkDiagnostic = try await APIClient().get("api/network")
                guard let self,
                      !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration,
                      self.loadRevision == expectedRevision else { return }
                if let mode = Int(diagnostic.usbnetMode ?? "") {
                    self.usbnetEnabled = mode == 1
                }
                self.usbnetLoaded = true
                self.loadTask = nil
            } catch {
                guard let self,
                      !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration,
                      self.loadRevision == expectedRevision else { return }
                // 已完成读取尝试；保留最近一次成功的开关值。
                self.usbnetLoaded = true
                self.loadTask = nil
            }
        }
    }

    func refreshFlightMode() {
        guard case .running = backend.state, !flightModeBusy else { return }
        flightModeRevision &+= 1
        let expectedRevision = flightModeRevision
        let expectedGeneration = lifecycleGeneration
        flightModeTask?.cancel()
        flightModeTask = Task { [weak self] in
            do {
                let status: ModuleFlightModeStatus = try await APIClient(timeoutInterval: 12)
                    .get("api/module/flight-mode")
                guard let self,
                      !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration,
                      self.flightModeRevision == expectedRevision else { return }
                self.flightModeStatus = status
                self.flightModeLoaded = true
                self.flightModeError = nil
                self.flightModeTask = nil
            } catch {
                guard let self,
                      !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration,
                      self.flightModeRevision == expectedRevision else { return }
                self.flightModeLoaded = true
                self.flightModeError = "读取模块飞行模式失败：\(error.localizedDescription)"
                self.flightModeTask = nil
            }
        }
    }

    func setFlightMode(enabled: Bool) {
        guard case .running = backend.state, !flightModeBusy else { return }
        guard flightModeStatus?.available == true else {
            onToast("模块未连接，无法切换飞行模式", false, "模块飞行模式")
            refreshFlightMode()
            return
        }
        guard flightModeStatus?.canToggle == true else {
            onToast(flightModeStatus?.detail ?? "模块当前正被其他业务占用", false, "暂时无法切换")
            refreshFlightMode()
            return
        }
        flightModeRevision &+= 1
        let expectedRevision = flightModeRevision
        let expectedGeneration = lifecycleGeneration
        flightModeTask?.cancel()
        flightModeBusy = true
        flightModeError = nil
        flightModeTask = Task { [weak self] in
            guard let self else { return }
            do {
                let status: ModuleFlightModeStatus = try await APIClient(timeoutInterval: 30)
                    .send("api/module/flight-mode", body: ModuleFlightModeRequest(enabled: enabled, userInitiated: true))
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration,
                      self.flightModeRevision == expectedRevision else { return }
                self.flightModeBusy = false
                self.flightModeTask = nil
                self.flightModeStatus = status
                self.flightModeLoaded = true
                self.flightModeError = nil
                let detail = status.detail?.trimmingCharacters(in: .whitespacesAndNewlines)
                self.onToast(
                    detail?.isEmpty == false ? detail! : status.summary,
                    status.enabled == enabled,
                    "模块飞行模式")
            } catch {
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration,
                      self.flightModeRevision == expectedRevision else { return }
                self.flightModeBusy = false
                self.flightModeTask = nil
                self.flightModeError = error.localizedDescription
                self.onToast("\(enabled ? "开启" : "关闭")飞行模式失败：\(error.localizedDescription)", false, "模块飞行模式")
                self.refreshFlightMode()
            }
        }
    }

    func switchMode() {
        guard case .running = backend.state, !busy, !recovering else { return }
        beginAction()
        let targetMode = usbnetEnabled ? 1 : 0
        let expectedGeneration = lifecycleGeneration
        actionTask = Task { [weak self] in
            guard let self else { return }
            do {
                let _: USBNetResult = try await APIClient().send(
                    "api/network/usbnet",
                    body: USBNetRequest(mode: targetMode))
                try await Task.sleep(nanoseconds: 3_000_000_000)
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration else { return }
                self.finishAction()
                self.loadUSBStatus()
                self.onServicesReload()
            } catch {
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration else { return }
                self.finishAction()
                self.loadUSBStatus()
            }
        }
    }

    func reboot() {
        guard case .running = backend.state, !busy, !recovering else { return }
        guard flightModeStatus?.enabled != true else {
            onToast("请先关闭飞行模式，再重启模块", false, "无法重启")
            return
        }
        beginAction()
        let expectedGeneration = lifecycleGeneration
        actionTask = Task { [weak self] in
            guard let self else { return }
            do {
                let _: RebootResult = try await APIClient().send("api/network/reboot-module")
                try await Task.sleep(nanoseconds: 5_000_000_000)
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration else { return }
                self.finishAction()
                self.loadUSBStatus()
            } catch {
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration else { return }
                self.finishAction()
            }
        }
    }

    var canRetryConnection: Bool {
        guard let traffic else { return false }
        return isRecoverableFailure(traffic) && usbnetEnabled
    }

    func recoverAfterWake() {
        guard usbnetEnabled else { return }
        if let lastAutomaticRecoveryAt,
           Date().timeIntervalSince(lastAutomaticRecoveryAt) < Self.automaticRecoveryCooldown {
            return
        }
        scheduleRecovery(.wake)
    }

    func retryConnection() {
        scheduleRecovery(.manual)
    }

    func runCheck4G() {
        guard case .running = backend.state, !busy, !recovering else { return }
        beginAction()
        let expectedGeneration = lifecycleGeneration
        actionTask = Task { [weak self] in
            guard let self else { return }
            do {
                let result: CheckResult = try await APIClient().send("api/network/check-4g")
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration else { return }
                self.check4G = result
                self.finishAction()
                self.onToast(
                    [result.summary, result.detail].compactMap { $0 }.joined(separator: "\n"),
                    result.ok,
                    nil)
            } catch {
                guard !Task.isCancelled,
                      self.lifecycleGeneration == expectedGeneration else { return }
                self.finishAction()
                self.onToast("检查失败：\(error.localizedDescription)", false, nil)
            }
        }
    }

    func reset() {
        lifecycleGeneration &+= 1
        loadRevision &+= 1
        loadTask?.cancel()
        actionTask?.cancel()
        recoveryTask?.cancel()
        flightModeTask?.cancel()
        loadTask = nil
        actionTask = nil
        recoveryTask = nil
        flightModeTask = nil
        traffic = nil
        usbnetEnabled = false
        usbnetLoaded = false
        busy = false
        recovering = false
        recoveryMessage = nil
        check4G = nil
        flightModeStatus = nil
        flightModeLoaded = false
        flightModeBusy = false
        flightModeError = nil
    }

    private func beginAction() {
        actionTask?.cancel()
        actionTask = nil
        busy = true
    }

    private func finishAction() {
        busy = false
        actionTask = nil
    }

    private func scheduleRecovery(_ trigger: NetworkRecoveryTrigger) {
        guard case .running = backend.state else {
            if trigger == .manual {
                onToast("后台服务尚未运行", false, "无法重新连接")
            }
            return
        }
        guard recoveryTask == nil else { return }

        recovering = true
        recoveryMessage = trigger.waitingMessage
        let expectedGeneration = lifecycleGeneration
        recoveryTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if self.lifecycleGeneration == expectedGeneration {
                    self.recoveryTask = nil
                    self.recovering = false
                    self.recoveryMessage = nil
                }
            }
            if trigger.delayNanoseconds > 0 {
                do {
                    try await Task.sleep(nanoseconds: trigger.delayNanoseconds)
                } catch {
                    return
                }
            }
            guard self.lifecycleGeneration == expectedGeneration else { return }
            await self.performRecovery(trigger, generation: expectedGeneration)
        }
    }

    private func performRecovery(
        _ trigger: NetworkRecoveryTrigger,
        generation expectedGeneration: UInt64
    ) async {
        guard !busy, !isCallModeBusy() else {
            if trigger == .manual {
                onToast("模块正在执行其他操作，请稍后重试", false, "无法重新连接")
            }
            return
        }

        let client = APIClient(timeoutInterval: 10)
        do {
            let current: TrafficSnapshot = try await client.get("api/network/traffic")
            guard lifecycleGeneration == expectedGeneration else { return }
            traffic = current
            if current.available {
                if trigger == .manual {
                    onToast("4G 网卡当前已连接", true, "无需重试")
                }
                return
            }
            guard isRecoverableFailure(current) else {
                if trigger == .manual {
                    onToast(current.error ?? "当前故障无法通过模块重启恢复", false, "无法重新连接")
                }
                return
            }
            guard usbnetEnabled else {
                if trigger == .manual {
                    onToast("请先开启 USB 网卡", false, "无法重新连接")
                }
                return
            }

            busy = true
            defer {
                if lifecycleGeneration == expectedGeneration {
                    busy = false
                }
            }

            let deviceStatus: DeviceStatus = try await client.get("api/status")
            guard lifecycleGeneration == expectedGeneration else { return }
            onModuleStatus(deviceStatus)
            if let usbnetMode = deviceStatus.usbnetMode {
                usbnetEnabled = usbnetMode == 1
            }
            guard deviceStatus.usbnetMode == 1 else {
                if trigger == .manual {
                    onToast("请先开启 USB 网卡", false, "无法重新连接")
                }
                return
            }

            let callStatus: CallStatus = try await client.get("api/call/status")
            guard lifecycleGeneration == expectedGeneration else { return }
            onCallStatus(callStatus)
            guard callStatus.isIdle else {
                if trigger == .manual {
                    onToast("通话期间不能重启模块", false, "无法重新连接")
                }
                return
            }

            recoveryMessage = "正在重启 4G 模块…"
            if trigger == .wake {
                lastAutomaticRecoveryAt = Date()
            }
            let _: RebootResult = try await client.send("api/network/reboot-module")
            recoveryMessage = "正在等待 4G 网卡重新连接…"

            var latestTraffic = current
            for _ in 0..<30 {
                try await Task.sleep(nanoseconds: 2_000_000_000)
                try Task.checkCancellation()
                guard lifecycleGeneration == expectedGeneration else { return }
                do {
                    let sample: TrafficSnapshot = try await client.get("api/network/traffic")
                    latestTraffic = sample
                    traffic = sample
                    if sample.available {
                        loadUSBStatus()
                        onServicesReload()
                        onDashboardRefresh()
                        onToast(trigger.successMessage, true, "4G 网络")
                        return
                    }
                } catch {
                    // 模块重启期间硬件会短暂消失，继续等待重新枚举。
                }
            }

            traffic = latestTraffic
            onToast("等待 60 秒后仍未恢复，请重试或拔插模块", false, "4G 网卡恢复失败")
        } catch {
            guard !Task.isCancelled,
                  lifecycleGeneration == expectedGeneration else { return }
            onToast("重新连接失败：\(error.localizedDescription)", false, "4G 网卡恢复失败")
        }
    }

    private func isRecoverableFailure(_ snapshot: TrafficSnapshot) -> Bool {
        !snapshot.available
            && snapshot.interface?.isEmpty == false
            && snapshot.error == Self.disconnectedError
    }
}
