import Foundation
import Darwin

enum LocalBackendAuthentication {
    static let appToken = "isolated-unix-test"
    static let headerName = "X-Test-Token"
}
struct CheckFailure: Error { let message: String }
struct EmptyResponse: Decodable {}

private final class LocalTestServer: @unchecked Sendable {
    let path: String
    private let fd: Int32
    private let directory: String
    private let finished = DispatchSemaphore(value: 0)

    init(responseDelay: useconds_t) throws {
        directory = "/tmp/proletarius-unix-" + UUID().uuidString.prefix(8)
        try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: false,
                                               attributes: [.posixPermissions: 0o700])
        path = directory + "/test.sock"
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw CheckFailure(message: "socket failed") }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8) + [0]
        withUnsafeMutablePointer(to: &address.sun_path) {
            UnsafeMutableRawPointer($0).copyMemory(from: bytes, byteCount: bytes.count)
        }
        let bound = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard bound == 0, listen(fd, 1) == 0 else { throw CheckFailure(message: "bind/listen failed") }
        DispatchQueue.global().async { [self] in
            defer { finished.signal() }
            let client = accept(fd, nil, nil)
            guard client >= 0 else { return }
            defer { Darwin.close(client) }
            var noSignal: Int32 = 1
            setsockopt(client, SOL_SOCKET, SO_NOSIGPIPE, &noSignal, socklen_t(MemoryLayout<Int32>.size))
            var buffer = [UInt8](repeating: 0, count: 4096)
            guard recv(client, &buffer, buffer.count, 0) > 0 else { return }
            usleep(responseDelay)
            let response = Data("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}".utf8)
            _ = response.withUnsafeBytes { Darwin.send(client, $0.baseAddress!, $0.count, 0) }
        }
    }

    func close() {
        _ = finished.wait(timeout: .now() + 3)
        Darwin.close(fd)
        try? FileManager.default.removeItem(atPath: path)
        try? FileManager.default.removeItem(atPath: directory)
    }
}

@main enum UnixSocketRequestTests {
    static func main() async throws {
        let server = try LocalTestServer(responseDelay: 800_000)
        defer { server.close() }
        UnixSocketURLProtocol.socketPath = server.path
        let start = ProcessInfo.processInfo.systemUptime
        do {
            let _: EmptyResponse = try await APIClient(timeoutInterval: 0.2).send("api/sms/refresh")
            throw CheckFailure(message: "actual Unix request ignored the 0.2s deadline")
        } catch let error as URLError {
            guard error.code == .timedOut else { throw error }
            let elapsed = ProcessInfo.processInfo.systemUptime - start
            guard elapsed < 0.7 else { throw CheckFailure(message: "actual socket timeout was late: \(elapsed)") }
        }
        print("PASS: actual UnixSocketURLProtocol obeys the shortened request deadline")

        let longServer = try LocalTestServer(responseDelay: 450_000)
        defer { longServer.close() }
        UnixSocketURLProtocol.socketPath = longServer.path
        // The selected long-write policy must exceed the client's ordinary
        // short deadline; no real 190-second wait is needed to prove routing.
        let _: EmptyResponse = try await APIClient(timeoutInterval: 0.1).send("api/sms/send")
        print("PASS: long-write policy survives beyond ordinary timeout on the actual Unix bridge")
    }
}
