import Foundation
import Combine

/// The app and both child runtimes share one isolated data root. The override
/// also lets offline checks keep every generated file inside the OneWIFI tree.
///
/// Internal legacy type name (`Switch*`); on-disk data directory is **OneWIFI**.
/// Type rename deferred (D-INT debt; MAC-RP2 does not rename this symbol).
enum SwitchRuntimePaths {
    static func dataDirectory(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        fileManager: FileManager = .default
    ) -> URL {
        if let path = environment["SWITCH_DATA_DIR"], !path.isEmpty {
            precondition(path.hasPrefix("/"), "SWITCH_DATA_DIR must be an absolute path")
            return URL(fileURLWithPath: path, isDirectory: true).standardizedFileURL
        }
        let applicationSupport = fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? fileManager.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support", isDirectory: true)
        return applicationSupport.appendingPathComponent("OneWIFI", isDirectory: true)
    }
}

/// 管理后端子进程的生命周期，并对外暴露 Unix socket 路径。
///
/// 正式路径使用 Rust gateway；尚未迁移的硬件仍仅由其 Go 子进程接管。
/// 隐藏的 `SwitchBackendMode=go-compatibility` 仅用于紧急回退。
@MainActor
final class BackendProcess: ObservableObject {
    enum State: Equatable {
        case stopped
        case starting
        case running
        case failed(String)
    }

    static let shared = BackendProcess()

    @Published private(set) var state: State = .stopped
    @Published private(set) var childPID: pid_t = 0
    private(set) var usesRustGateway = false

    private var process: Process?
    private var startupTask: Task<Void, Never>?
    private var launchGeneration: UInt64 = 0
    private let executableURLOverride: URL?
    private var stdoutTask: Task<Void, Never>?
    private var stderrTask: Task<Void, Never>?
    private let logWriteQueue = DispatchQueue(label: "switch.backend.log-writer")
    private let dataDirectory: URL

    let socketPath: String
    let bleSocketPath: String
    let logURL: URL

    init(supportDirectory: URL? = nil, executableURL: URL? = nil) {
        executableURLOverride = executableURL
        let fm = FileManager.default
        let support = supportDirectory ?? SwitchRuntimePaths.dataDirectory(fileManager: fm)
        dataDirectory = support
        try? fm.createDirectory(at: support, withIntermediateDirectories: true)
        try? fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: support.path)
        socketPath = support.appendingPathComponent("switch.sock").path
        bleSocketPath = support.appendingPathComponent("estk-ble.sock").path
        logURL = support.appendingPathComponent("switch.log")
        if !fm.fileExists(atPath: logURL.path) {
            _ = fm.createFile(
                atPath: logURL.path,
                contents: nil,
                attributes: [.posixPermissions: 0o600])
        }
        try? fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: logURL.path)

        UnixSocketURLProtocol.socketPath = socketPath
    }

    private var binaryResourceName: String {
        let mode = UserDefaults.standard.string(forKey: "SwitchBackendMode")
        if mode == "go-compatibility" {
            return "switchd"
        }
        return "switchd-rs"
    }

    var binaryURL: URL? {
        executableURLOverride ?? Bundle.main.url(
            forResource: binaryResourceName,
            withExtension: nil,
            subdirectory: "backend")
    }

    func start() {
        // A slow/unresponsive child may still own hardware after startup times
        // out. Never launch a second child or kill the first one to retry.
        if let process, process.isRunning {
            if case .failed = state { beginReadinessCheck(for: process) }
            return
        }
        startupTask?.cancel()
        startupTask = nil
        launchGeneration &+= 1
        usesRustGateway = binaryResourceName == "switchd-rs"
        guard let binary = binaryURL else {
            state = .failed("缺少内置后端程序")
            return
        }

        do {
            try prepareSocketPathForLaunch()
        } catch {
            state = .failed("后端 socket 路径不安全：\(error.localizedDescription)")
            return
        }

        let proc = Process()
        proc.executableURL = binary
        proc.arguments = ["-listen", "unix:\(socketPath)"]
        var environment = ProcessInfo.processInfo.environment
        environment["PROLETARIUS_APP_TOKEN"] = LocalBackendAuthentication.appToken
        environment["PROLETARIUS_BLE_SOCKET"] = bleSocketPath
        environment["SWITCH_DATA_DIR"] = dataDirectory.path
        proc.environment = environment

        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardOutput = outPipe
        proc.standardError = errPipe
        proc.terminationHandler = { [weak self] p in
            Task { @MainActor in
                guard let self, self.process === p else { return }
                self.launchGeneration &+= 1
                self.startupTask?.cancel()
                self.startupTask = nil
                self.process = nil
                self.childPID = 0
                if self.state == .starting {
                    self.state = .failed("后端进程提前退出，请查看日志")
                } else if self.state == .running {
                    self.state = .stopped
                }
            }
        }

        do {
            try proc.run()
        } catch {
            state = .failed("无法启动后端：\(error.localizedDescription)")
            return
        }

        let logURL = self.logURL
        let logQueue = self.logWriteQueue
        stdoutTask = Task.detached(priority: .utility) {
            Self.drain(
                outPipe.fileHandleForReading,
                to: .standardOutput,
                logURL: logURL,
                logWriteQueue: logQueue)
        }
        stderrTask = Task.detached(priority: .utility) {
            Self.drain(
                errPipe.fileHandleForReading,
                to: .standardError,
                logURL: logURL,
                logWriteQueue: logQueue)
        }

        process = proc
        childPID = proc.processIdentifier
        beginReadinessCheck(for: proc)
    }

    private func beginReadinessCheck(for proc: Process) {
        startupTask?.cancel()
        launchGeneration &+= 1
        let generation = launchGeneration
        state = .starting

        startupTask = Task { @MainActor [weak self] in
            guard let self else { return }
            defer {
                if self.launchGeneration == generation, self.process === proc {
                    self.startupTask = nil
                }
            }
            // One absolute deadline includes every health request and delay.
            let deadline = ProcessInfo.processInfo.systemUptime + 5
            while ProcessInfo.processInfo.systemUptime < deadline {
                guard !Task.isCancelled, self.launchGeneration == generation,
                      self.process === proc else { return }
                if !proc.isRunning {
                    self.state = .failed("后端进程提前退出，请查看日志")
                    return
                }
                let remaining = deadline - ProcessInfo.processInfo.systemUptime
                let ready = await BackendReadiness.check(timeout: min(1, remaining))
                guard !Task.isCancelled, self.launchGeneration == generation,
                      self.process === proc else { return }
                guard proc.isRunning else {
                    self.state = .failed("后端进程提前退出，请查看日志")
                    return
                }
                if ready {
                    self.state = .running
                    return
                }
                let pause = min(0.1, deadline - ProcessInfo.processInfo.systemUptime)
                if pause > 0 {
                    do { try await Task.sleep(nanoseconds: UInt64(pause * 1_000_000_000)) }
                    catch { return }
                }
            }
            self.state = .failed("后端启动超时")
        }
    }

    func stop() {
        launchGeneration &+= 1
        startupTask?.cancel()
        startupTask = nil
        state = .stopped
        if let proc = process, proc.isRunning {
            proc.terminate()
        }
        stdoutTask?.cancel()
        stderrTask?.cancel()
    }

    /// Only a stale Unix socket may be removed. A regular file or symlink at
    /// this path is treated as a launch failure instead of being destroyed.
    private func prepareSocketPathForLaunch() throws {
        let fileManager = FileManager.default
        if (try? fileManager.destinationOfSymbolicLink(atPath: socketPath)) != nil {
            throw NSError(
                domain: "Proletarius.BackendProcess",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "拒绝覆盖符号链接"])
        }
        guard fileManager.fileExists(atPath: socketPath) else { return }
        let attributes = try fileManager.attributesOfItem(atPath: socketPath)
        guard attributes[.type] as? FileAttributeType == .typeSocket else {
            throw NSError(
                domain: "Proletarius.BackendProcess",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "拒绝覆盖非 socket 文件"])
        }
        guard !UnixSocketURLProtocol.isSocketReachable(socketPath) else {
            throw NSError(
                domain: "Proletarius.BackendProcess",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "已有 OneWIFI 实例正在运行；为保护现有 IMS、短信和通话会话，拒绝覆盖其本地后端。请先退出旧实例后再启动此版本"])
        }
        try fileManager.removeItem(atPath: socketPath)
    }

    nonisolated private static func drain(
        _ handle: FileHandle,
        to dest: FileHandle,
        logURL: URL,
        logWriteQueue: DispatchQueue
    ) {
        while true {
            let data = handle.availableData
            if data.isEmpty { break }
            try? dest.write(contentsOf: data)
            // stdout 与 stderr 分别在后台任务中读取，统一串行追加，避免日志互相覆盖。
            logWriteQueue.sync {
                guard let handle = try? FileHandle(forWritingTo: logURL) else { return }
                defer { try? handle.close() }
                handle.seekToEndOfFile()
                handle.write(data)
            }
        }
        try? handle.close()
    }
}

/// Startup health checks have their own deadline: APIClient's normal resource
/// budget is intentionally much longer, and a custom URLProtocol can be stuck
/// waiting for a peer that accepted a socket but never sent HTTP.
enum BackendReadiness {
    static func check(timeout: TimeInterval) async -> Bool {
        guard timeout > 0, !Task.isCancelled else { return false }
        let attempt = Attempt()
        return await withTaskCancellationHandler {
            await withCheckedContinuation { continuation in
                attempt.start(timeout: timeout, continuation: continuation)
            }
        } onCancel: {
            attempt.finish(false)
        }
    }

    private struct Health: Decodable { let ok: Bool }

    private final class Attempt: @unchecked Sendable {
        private let lock = NSLock()
        private var finished = false
        private var continuation: CheckedContinuation<Bool, Never>?
        private var task: URLSessionDataTask?
        private var session: URLSession?
        private var deadline: DispatchWorkItem?

        func start(timeout: TimeInterval, continuation: CheckedContinuation<Bool, Never>) {
            let configuration = URLSessionConfiguration.ephemeral
            configuration.protocolClasses = [UnixSocketURLProtocol.self]
            configuration.timeoutIntervalForRequest = timeout
            configuration.timeoutIntervalForResource = timeout
            let session = URLSession(configuration: configuration)
            var request = URLRequest(url: URL(string: "http+unix://proletarius/api/health")!)
            request.setValue(LocalBackendAuthentication.appToken,
                             forHTTPHeaderField: LocalBackendAuthentication.headerName)
            let task = session.dataTask(with: request) { [weak self] data, response, error in
                let status = (response as? HTTPURLResponse)?.statusCode ?? 0
                let healthy = error == nil && (200..<300).contains(status)
                    && data.flatMap { try? JSONDecoder().decode(Health.self, from: $0) }?.ok == true
                self?.finish(healthy)
            }
            let deadline = DispatchWorkItem { [weak self] in self?.finish(false) }
            lock.lock()
            guard !finished else {
                lock.unlock()
                task.cancel()
                session.invalidateAndCancel()
                continuation.resume(returning: false)
                return
            }
            self.continuation = continuation
            self.task = task
            self.session = session
            self.deadline = deadline
            lock.unlock()
            DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + timeout, execute: deadline)
            task.resume()
        }

        func finish(_ healthy: Bool) {
            lock.lock()
            guard !finished else { lock.unlock(); return }
            finished = true
            let continuation = self.continuation
            let task = self.task
            let session = self.session
            let deadline = self.deadline
            self.continuation = nil
            self.task = nil
            self.session = nil
            self.deadline = nil
            lock.unlock()
            deadline?.cancel()
            task?.cancel()
            session?.invalidateAndCancel()
            continuation?.resume(returning: healthy)
        }
    }
}
