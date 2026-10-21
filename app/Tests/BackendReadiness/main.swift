import Darwin
import Foundation

// This executable is also its own fake backend. All sockets, logs and control
// files live in its unique temporary directory; it never runs the app/backend.
private func require(_ condition: @autoclosure () -> Bool, _ message: String) {
    if !condition() { fatalError(message) }
}

private func address(_ path: String) -> sockaddr_un {
    var value = sockaddr_un()
    value.sun_family = sa_family_t(AF_UNIX)
    require(path.utf8.count < MemoryLayout.size(ofValue: value.sun_path), "socket path too long")
    withUnsafeMutablePointer(to: &value.sun_path) { pointer in
        for (index, byte) in path.utf8.enumerated() {
            UnsafeMutableRawPointer(pointer).storeBytes(of: byte, toByteOffset: index, as: UInt8.self)
        }
    }
    return value
}

private func fakeBackend(_ path: String) {
    signal(SIGPIPE, SIG_IGN)
    let originalParent = getppid()
    DispatchQueue.global().async {
        while true {
            usleep(50_000)
            if getppid() != originalParent { exit(0) }
        }
    }
    let modePath = URL(fileURLWithPath: path).deletingLastPathComponent().appendingPathComponent("mode")
    require(ProcessInfo.processInfo.environment["SWITCH_DATA_DIR"] == modePath.deletingLastPathComponent().path,
            "child backend did not inherit its owner's isolated data directory")
    let mode = (try? String(contentsOf: modePath, encoding: .utf8)) ?? "healthy"
    if mode == "exit" { return }
    if mode == "late" { signal(SIGTERM, SIG_IGN) }
    let listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
    require(listener >= 0, "fixture socket failed")
    defer { Darwin.close(listener) }
    var socketAddress = address(path)
    let bound = withUnsafePointer(to: &socketAddress) { pointer in
        pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    require(bound == 0, "fixture bind failed")
    if mode == "bound" {
        while true { pause() }
    }
    require(Darwin.listen(listener, 16) == 0, "fixture listen failed")
    while true {
        let client = Darwin.accept(listener, nil, nil)
        if client < 0 { return }
        var bytes = [UInt8](repeating: 0, count: 4096)
        let count = Darwin.read(client, &bytes, bytes.count)
        let request = count > 0 ? String(decoding: bytes.prefix(count), as: UTF8.self) : ""
        let requestMode = (try? String(contentsOf: modePath, encoding: .utf8)) ?? mode
        let token = ProcessInfo.processInfo.environment["PROLETARIUS_APP_TOKEN"] ?? "missing"
        let authorized = request.lowercased().contains("x-proletarius-app-token: " + token.lowercased())
        require(request.hasPrefix("GET /api/health "), "probe must only request health")
        // Keep accepted peers open without sending or closing. This catches
        // cancellation that returns to the UI but leaves the protocol reader blocked.
        if requestMode == "silent" {
            try! "accepted".write(to: modePath.deletingLastPathComponent().appendingPathComponent("accepted"),
                                  atomically: true, encoding: .utf8)
            continue
        }
        if requestMode == "late" { usleep(400_000) }
        if requestMode == "exit-after-request" { Darwin.close(client); return }
        let status = !authorized || requestMode == "unauthorized" ? 401 : 200
        let body = requestMode == "malformed" ? "not-json" : (requestMode == "false" ? "{\"ok\":false}" : "{\"ok\":true}")
        let response = "HTTP/1.1 \(status) Fixture\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)"
        response.withCString { pointer in _ = Darwin.write(client, pointer, response.utf8.count) }
        Darwin.close(client)
    }
}

@MainActor
private final class Fixture {
    let directory: URL
    let backend: BackendProcess
    private var ownedPIDs: [pid_t] = []

    init(root: URL, name: String, mode: String) throws {
        directory = root.appendingPathComponent(name)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        try mode.write(to: directory.appendingPathComponent("mode"), atomically: true, encoding: .utf8)
        backend = BackendProcess(supportDirectory: directory,
                                 executableURL: URL(fileURLWithPath: CommandLine.arguments[0]))
    }

    func start() { backend.start(); if backend.childPID > 0 { ownedPIDs.append(backend.childPID) } }

    func cleanup() async {
        backend.stop()
        // Only PIDs launched by this fixture may be force-terminated. The late
        // response fixture intentionally ignores SIGTERM to exercise fencing.
        for pid in Set(ownedPIDs) where Darwin.kill(pid, 0) == 0 { _ = Darwin.kill(pid, SIGKILL) }
        _ = await waitUntil { self.backend.childPID == 0 }
    }
}

@MainActor
private func waitUntil(seconds: TimeInterval = 2, _ condition: () -> Bool) async -> Bool {
    let deadline = ProcessInfo.processInfo.systemUptime + seconds
    while !condition(), ProcessInfo.processInfo.systemUptime < deadline {
        try? await Task.sleep(nanoseconds: 10_000_000)
    }
    return condition()
}

private func threadCount() -> Int {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/bin/ps")
    process.arguments = ["-M", "-p", String(getpid())]
    let output = Pipe()
    process.standardOutput = output
    try! process.run()
    let data = output.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    return String(decoding: data, as: UTF8.self).split(separator: "\n").count - 1
}

private func fileDescriptorCount() -> Int {
    (try! FileManager.default.contentsOfDirectory(atPath: "/dev/fd")).count
}

@main
private struct BackendReadinessTests {
    @MainActor
    static func main() async throws {
        if CommandLine.arguments.count == 3, CommandLine.arguments[1] == "-listen" {
            fakeBackend(String(CommandLine.arguments[2].dropFirst("unix:".count)))
            return
        }
        let root = URL(fileURLWithPath: CommandLine.arguments[1])
        let isolated = SwitchRuntimePaths.dataDirectory(environment: ["SWITCH_DATA_DIR": root.path])
        require(isolated.path == root.standardizedFileURL.path, "data-root override was not honored")
        let defaultRoot = SwitchRuntimePaths.dataDirectory(environment: [:])
        require(defaultRoot.lastPathComponent == "OneWIFI", "default data root must use the new app identity")
        let healthy = try Fixture(root: root, name: "healthy", mode: "healthy")
        require(healthy.backend.socketPath == healthy.directory.appendingPathComponent("switch.sock").path,
                "backend socket escaped its Switch fixture")
        require(healthy.backend.logURL == healthy.directory.appendingPathComponent("switch.log"),
                "backend log escaped its Switch fixture")
        healthy.start()
        let firstPID = healthy.backend.childPID
        healthy.start()
        require(healthy.backend.childPID == firstPID, "duplicate startup spawned another child")
        let becameHealthy = await waitUntil { healthy.backend.state == .running }
        require(becameHealthy, "authenticated healthy backend was not ready")
        healthy.start()
        require(healthy.backend.childPID == firstPID, "running backend spawned another child")
        await healthy.cleanup()
        print("PASS authenticated health and duplicate starts")

        for mode in ["bound", "unauthorized", "malformed", "false", "exit-after-request"] {
            let fixture = try Fixture(root: root, name: mode, mode: mode)
            fixture.start()
            try? await Task.sleep(nanoseconds: 200_000_000)
            require(fixture.backend.state != .running, "\(mode) incorrectly reported running")
            await fixture.cleanup()
        }
        print("PASS bound-only socket, unauthorized, malformed, ok=false, exit during health")

        let silent = try Fixture(root: root, name: "silent", mode: "silent")
        silent.start()
        let silentPID = silent.backend.childPID
        let began = ProcessInfo.processInfo.systemUptime
        let failed = await waitUntil(seconds: 6) {
            if case .failed = silent.backend.state { return true }
            return false
        }
        let elapsed = ProcessInfo.processInfo.systemUptime - began
        require(failed && elapsed < 5.7, "silent HTTP ignored the five-second startup deadline: \(elapsed)")
        require(Darwin.kill(silentPID, 0) == 0, "startup failure killed a live child")
        let threadsBeforeRepeatedTimeout = threadCount()
        let descriptorsBeforeRepeatedTimeout = fileDescriptorCount()
        for _ in 0..<12 { _ = await BackendReadiness.check(timeout: 0.05) }
        let threadsAfterRepeatedTimeout = threadCount()
        let descriptorsAfterRepeatedTimeout = fileDescriptorCount()
        require(threadsAfterRepeatedTimeout <= threadsBeforeRepeatedTimeout + 3, "timeout readers accumulated threads")
        require(descriptorsAfterRepeatedTimeout <= descriptorsBeforeRepeatedTimeout + 2, "timeout readers accumulated descriptors")
        print("RESOURCE 12 further timeouts: threads \(threadsBeforeRepeatedTimeout)→\(threadsAfterRepeatedTimeout), FDs \(descriptorsBeforeRepeatedTimeout)→\(descriptorsAfterRepeatedTimeout)")
        try "healthy".write(to: silent.directory.appendingPathComponent("mode"), atomically: true, encoding: .utf8)
        silent.start()
        require(silent.backend.childPID == silentPID, "failed readiness spawned a second live child")
        let retriedHealthy = await waitUntil { silent.backend.state == .running }
        require(retriedHealthy && silent.backend.childPID == silentPID, "explicit retry did not recover the existing child")
        let threadsBeforeCancellation = threadCount()
        let descriptorsBeforeCancellation = fileDescriptorCount()
        let accepted = silent.directory.appendingPathComponent("accepted")
        for _ in 0..<100 {
            try? FileManager.default.removeItem(at: accepted)
            try "silent".write(to: silent.directory.appendingPathComponent("mode"), atomically: true, encoding: .utf8)
            let pending = Task { await BackendReadiness.check(timeout: 1) }
            let received = await waitUntil { FileManager.default.fileExists(atPath: accepted.path) }
            require(received, "fixture did not receive cancellation probe")
            pending.cancel()
            let canceledResult = await pending.value
            require(!canceledResult, "canceled probe succeeded")
            try "healthy".write(to: silent.directory.appendingPathComponent("mode"), atomically: true, encoding: .utf8)
            let nextHealthy = await BackendReadiness.check(timeout: 1)
            require(nextHealthy, "canceled request interfered with the next healthy request")
        }
        let threadsAfterCancellation = threadCount()
        let descriptorsAfterCancellation = fileDescriptorCount()
        require(threadsAfterCancellation <= threadsBeforeCancellation + 3, "canceled readers accumulated threads")
        require(descriptorsAfterCancellation <= descriptorsBeforeCancellation + 2, "canceled readers accumulated descriptors")
        print("RESOURCE 100 cancellation/health pairs: threads \(threadsBeforeCancellation)→\(threadsAfterCancellation), FDs \(descriptorsBeforeCancellation)→\(descriptorsAfterCancellation)")
        await silent.cleanup()
        print("PASS silent listener deadline \(String(format: "%.3f", elapsed))s; explicit retry recovered original PID")

        let late = try Fixture(root: root, name: "late", mode: "late")
        late.start()
        try? await Task.sleep(nanoseconds: 150_000_000)
        late.backend.stop()
        require(late.backend.state == .stopped, "stop must immediately cancel startup")
        try? await Task.sleep(nanoseconds: 500_000_000)
        require(late.backend.state == .stopped, "late successful health revived a stopped startup")
        await late.cleanup()
        print("PASS cancellation fences delayed successful health")

        let replacement = try Fixture(root: root, name: "replacement", mode: "exit")
        replacement.start()
        let retiredPID = replacement.backend.childPID
        // Keep the main actor occupied until the first child's termination is
        // queued, then replace it before either old callback can update state.
        usleep(250_000)
        try "healthy".write(to: replacement.directory.appendingPathComponent("mode"), atomically: true, encoding: .utf8)
        replacement.start()
        let replacementPID = replacement.backend.childPID
        require(retiredPID != replacementPID, "fixture failed to launch replacement")
        let replacementReady = await waitUntil { replacement.backend.state == .running }
        require(replacementReady && replacement.backend.childPID == replacementPID,
                "old startup/termination overwrote replacement state")
        await replacement.cleanup()
        print("PASS old startup/termination cannot overwrite replacement process")
    }
}
