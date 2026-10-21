import Combine
import Foundation

/// Owns host audio routing for module and reader VoWiFi calls.
/// Signalling state is supplied by `CallRuntimeStore`; an operation revision
/// prevents route-change recovery or a late microphone grant from restarting
/// media after hangup.
@MainActor
final class CallAudioStore: ObservableObject {
    @Published var error: String?
    @Published var running = false
    @Published var readerPhase = "未连接"
    @Published var readerInputName = "系统默认输入"
    @Published var readerOutputName = "系统默认输出"
    @Published var readerProcessingName = "软件抗干扰与限幅"
    @Published private(set) var readerMicrophoneActive = false

    var currentStatus: (() -> CallStatus)?
    var currentEndpoint: (() -> String)?
    var currentReaderState: (() -> ReaderVoWiFiCallState?)?
    var moduleCallModeReady: (() -> Bool)?

    private let backend: BackendProcess
    private var activationTask: Task<Void, Never>?
    private var routeChangeTask: Task<Void, Never>?
    private var operationRevision: UInt64 = 0

    init(backend: BackendProcess) {
        self.backend = backend
        AudioBridge.shared.onDefaultDeviceChange = { [weak self] in
            self?.scheduleModuleRouteRecovery()
        }
    }

    var activationInFlight: Bool { activationTask != nil }

    func reset() {
        stop(stopModuleRoute: false)
        error = nil
        readerInputName = "系统默认输入"
        readerOutputName = "系统默认输出"
        readerProcessingName = "软件抗干扰与限幅"
    }

    func handleStatusChange(previous: CallStatus, status: CallStatus) {
        let endpoint = currentEndpoint?() ?? "module"
        let readerCall = currentReaderState?()
        if status.isActive {
            if endpoint == "module" {
                if !previous.isActive { startModuleAudio() }
            } else if let readerCall,
                      let port = readerCall.audioPort,
                      let token = readerCall.audioToken,
                      readerCall.mediaReady {
                refreshReaderRouteDescription()
                if running {
                    ReaderVoWiFiAudioBridge.shared.setLocalRingbackEnabled(false)
                    if !readerMicrophoneActive { enableReaderMicrophone() }
                } else {
                    startReaderAudio(
                        port: port,
                        token: token,
                        captureMicrophone: true,
                        localRingback: false)
                }
            }
        } else if isVoWiFiEndpoint(endpoint),
                  let readerCall,
                  readerCall.state == "early",
                  let port = readerCall.audioPort,
                  let token = readerCall.audioToken {
            if running {
                ReaderVoWiFiAudioBridge.shared.setLocalRingbackEnabled(!readerCall.earlyMedia)
                readerPhase = readerCall.earlyMedia
                    ? "正在播放远端早期媒体"
                    : "正在播放本地回铃音"
                refreshReaderRouteDescription()
            } else {
                startReaderAudio(
                    port: port,
                    token: token,
                    captureMicrophone: false,
                    localRingback: !readerCall.earlyMedia)
            }
        }
        if status.isIdle && !previous.isIdle { stop() }
    }

    func prepareForHangup(endpoint _: String) {
        stop(stopModuleRoute: false)
    }

    func startModuleAudio() {
        guard currentEndpoint?() == "module" else { return }
        startModuleAudio(activateBackendRoute: true)
    }

    private func startModuleAudio(activateBackendRoute: Bool) {
        guard activationTask == nil, !running else { return }
        guard moduleCallModeReady?() == true else {
            error = "通话已接通，但通话模式尚未就绪。请挂断后完成运行时准备。"
            return
        }
        operationRevision &+= 1
        let revision = operationRevision
        error = nil
        activationTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if revision == self.operationRevision { self.activationTask = nil }
            }
            do {
                guard await AudioBridge.shared.requestMicrophoneAccess() else {
                    guard revision == self.operationRevision else { return }
                    self.error = "麦克风权限未开启。请在系统设置“隐私与安全性 → 麦克风”中允许 OneWIFI。"
                    return
                }
                if activateBackendRoute {
                    let result: CallAudioStartResult = try await APIClient(timeoutInterval: 60)
                        .send("api/call/audio/start")
                    guard revision == self.operationRevision else { return }
                    guard result.started else {
                        self.error = "模块没有确认通话音频路由已启动。"
                        return
                    }
                }
                try Task.checkCancellation()
                guard revision == self.operationRevision,
                      self.currentStatus?().isActive == true,
                      self.currentEndpoint?() == "module" else {
                    if activateBackendRoute {
                        let _: CallAudioStopResult? = try? await APIClient(timeoutInterval: 20)
                            .send("api/call/audio/stop")
                    }
                    return
                }
                if let bridgeError = AudioBridge.shared.start() {
                    self.error = bridgeError
                    self.running = false
                    if activateBackendRoute {
                        let _: CallAudioStopResult? = try? await APIClient(timeoutInterval: 20)
                            .send("api/call/audio/stop")
                    }
                    return
                }
                guard revision == self.operationRevision else {
                    AudioBridge.shared.stop()
                    return
                }
                self.running = true
            } catch is CancellationError {
                // The stop/hangup path owns route cleanup.
            } catch {
                guard revision == self.operationRevision else { return }
                self.error = "通话音频启动失败：\(error.localizedDescription)"
                self.running = false
            }
        }
    }

    private func startReaderAudio(
        port: Int,
        token: String,
        captureMicrophone: Bool,
        localRingback: Bool
    ) {
        guard activationTask == nil, !running else { return }
        operationRevision &+= 1
        let revision = operationRevision
        error = nil
        readerPhase = localRingback
            ? "正在建立回铃音输出"
            : (captureMicrophone ? "正在连接双向音频" : "正在连接早期媒体")
        activationTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if revision == self.operationRevision { self.activationTask = nil }
            }
            self.refreshReaderRouteDescription()
            let bridgeError = await ReaderVoWiFiAudioBridge.shared.start(
                port: port,
                token: token,
                captureMicrophone: captureMicrophone,
                localRingback: localRingback)
            guard !Task.isCancelled, revision == self.operationRevision else { return }
            guard isVoWiFiEndpoint(self.currentEndpoint?() ?? ""),
                  self.currentStatus?().isIdle != true else {
                ReaderVoWiFiAudioBridge.shared.stop()
                return
            }
            self.error = bridgeError
            self.running = bridgeError == nil
            self.readerMicrophoneActive = bridgeError == nil && captureMicrophone
            self.readerProcessingName = ReaderVoWiFiAudioBridge.shared.audioProcessingDescription()
            if let bridgeError {
                self.readerPhase = bridgeError
            } else if localRingback {
                self.readerPhase = "正在播放本地回铃音"
            } else if captureMicrophone {
                self.readerPhase = "双向音频已连接"
            } else {
                self.readerPhase = "正在播放运营商早期媒体"
            }
        }
    }

    private func enableReaderMicrophone() {
        guard activationTask == nil else { return }
        operationRevision &+= 1
        let revision = operationRevision
        readerPhase = "正在连接麦克风"
        activationTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if revision == self.operationRevision { self.activationTask = nil }
            }
            let bridgeError = await ReaderVoWiFiAudioBridge.shared.enableMicrophone()
            guard !Task.isCancelled,
                  revision == self.operationRevision,
                  isVoWiFiEndpoint(self.currentEndpoint?() ?? ""),
                  self.currentStatus?().isActive == true else { return }
            self.error = bridgeError
            self.readerMicrophoneActive = bridgeError == nil
            self.readerProcessingName = ReaderVoWiFiAudioBridge.shared.audioProcessingDescription()
            self.readerPhase = bridgeError ?? "双向音频已连接"
        }
    }

    func stop(stopModuleRoute: Bool = true) {
        let shouldStopModuleRoute = stopModuleRoute
            && currentEndpoint?() == "module"
            && (running || activationTask != nil)
        operationRevision &+= 1
        let revision = operationRevision
        activationTask?.cancel()
        activationTask = nil
        routeChangeTask?.cancel()
        routeChangeTask = nil
        AudioBridge.shared.stop()
        ReaderVoWiFiAudioBridge.shared.stop()
        running = false
        readerMicrophoneActive = false
        readerPhase = "未连接"
        if shouldStopModuleRoute, case .running = backend.state {
            Task { [weak self] in
                do {
                    let _: CallAudioStopResult = try await APIClient(timeoutInterval: 20)
                        .send("api/call/audio/stop")
                } catch {
                    guard let self, revision == self.operationRevision else { return }
                    self.error = "通话音频清理失败：\(error.localizedDescription)"
                }
            }
        }
    }

    private func refreshReaderRouteDescription() {
        let route = ReaderVoWiFiAudioBridge.shared.audioRoute()
        readerInputName = route.input
        readerOutputName = route.output
        if let routeError = ReaderVoWiFiAudioBridge.shared.audioRouteError() {
            error = routeError
        } else if error?.hasPrefix("音频设备切换后") == true
                    || error?.hasPrefix("无法恢复当前音频") == true {
            error = nil
        }
    }

    private func scheduleModuleRouteRecovery() {
        routeChangeTask?.cancel()
        routeChangeTask = Task { [weak self] in
            do {
                try await Task.sleep(for: .milliseconds(350))
            } catch {
                return
            }
            guard let self, self.running, self.currentStatus?().isActive == true else { return }
            let endpoint = self.currentEndpoint?() ?? "module"
            if isVoWiFiEndpoint(endpoint) {
                guard let call = self.currentReaderState?(),
                      let port = call.audioPort,
                      let token = call.audioToken,
                      call.mediaReady else { return }
                self.operationRevision &+= 1
                self.activationTask?.cancel()
                self.activationTask = nil
                ReaderVoWiFiAudioBridge.shared.stop()
                self.running = false
                self.startReaderAudio(
                    port: port,
                    token: token,
                    captureMicrophone: true,
                    localRingback: false)
                return
            }
            guard endpoint == "module" else { return }
            self.operationRevision &+= 1
            self.activationTask?.cancel()
            self.activationTask = nil
            AudioBridge.shared.stop()
            self.running = false
            self.startModuleAudio(activateBackendRoute: false)
        }
    }
}

private func isVoWiFiEndpoint(_ endpoint: String) -> Bool {
    endpoint == "reader" || endpoint == "module-vowifi"
}
