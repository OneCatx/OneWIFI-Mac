import Darwin
import Foundation

/// Private, authenticated Unix-socket bridge from the backend's card services
/// to the app-owned CoreBluetooth central. One connection handles one request,
/// and the accept loop serializes APDUs across eSIM, AKA, and SMS operations.
final class ReaderBLEBridge {
    static let shared = ReaderBLEBridge()
    private var listener: Int32 = -1
    private let maxRequestBytes = 160_000
    private let standardRequestTimeout: TimeInterval = 35
    // BluetoothReaderManager invalidates a stalled APDU after 120 seconds.
    // Give it time to deliver that failure and clear pendingReply first.
    private let cardOperationTimeout: TimeInterval = 130

    private init() {}

    func start(socketPath: String) {
        guard listener < 0 else { return }
        guard socketPath.utf8.count < MemoryLayout<sockaddr_un>.size - 4 else {
            NSLog("BLE bridge socket path too long")
            return
        }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &address.sun_path) { bytes in
            let path = Array(socketPath.utf8)
            bytes.copyBytes(from: path)
            bytes[path.count] = 0
        }
        let addressLength = socklen_t(MemoryLayout<sockaddr_un>.size)
        let bindAddress: (Int32) -> Int32 = { fd in
            withUnsafePointer(to: &address) { pointer in
                pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    Darwin.bind(fd, $0, addressLength)
                }
            }
        }
        if FileManager.default.fileExists(atPath: socketPath) {
            guard let attributes = try? FileManager.default.attributesOfItem(atPath: socketPath),
                  attributes[.type] as? FileAttributeType == .typeSocket else {
                NSLog("BLE bridge path is not a socket; refusing to overwrite")
                return
            }
            let probe = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
            let reachable = withUnsafePointer(to: &address) { pointer in
                pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    Darwin.connect(probe, $0, addressLength) == 0
                }
            }
            Darwin.close(probe)
            guard !reachable else {
                NSLog("BLE bridge already owned by a running app")
                return
            }
            guard Darwin.unlink(socketPath) == 0 else { return }
        }
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return }
        guard bindAddress(fd) == 0, Darwin.chmod(socketPath, 0o600) == 0,
              Darwin.listen(fd, 4) == 0 else {
            Darwin.close(fd)
            _ = Darwin.unlink(socketPath)
            NSLog("BLE bridge could not bind private socket")
            return
        }
        listener = fd
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            self?.acceptLoop(fd)
        }
    }

    private func acceptLoop(_ fd: Int32) {
        while true {
            let client = Darwin.accept(fd, nil, nil)
            if client < 0 { break }
            handle(client)
            Darwin.close(client)
        }
    }

    private func handle(_ fd: Int32) {
        var timeout = timeval(tv_sec: 5, tv_usec: 0)
        withUnsafePointer(to: &timeout) {
            _ = Darwin.setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, $0, socklen_t(MemoryLayout<timeval>.size))
        }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while data.count <= maxRequestBytes {
            let capacity = buffer.count
            let count = buffer.withUnsafeMutableBytes { Darwin.read(fd, $0.baseAddress, capacity) }
            if count <= 0 { return }
            data.append(contentsOf: buffer.prefix(count))
            if let newline = data.firstIndex(of: 10) {
                data = data.prefix(upTo: newline)
                break
            }
        }
        guard data.count <= maxRequestBytes,
              let request = try? JSONDecoder().decode(Request.self, from: data),
              constantTimeEquals(request.token, LocalBackendAuthentication.appToken) else {
            send(Response(ok: false, errorCode: "ble_unauthorized"), fd: fd)
            return
        }
        let semaphore = DispatchSemaphore(value: 0)
        let answerLock = NSLock()
        var answer = Response(ok: false, errorCode: "ble_timeout")
        let complete: (Response) -> Void = { value in
            answerLock.lock()
            answer = value
            answerLock.unlock()
            semaphore.signal()
        }
        DispatchQueue.main.async {
            let manager = BluetoothReaderManager.shared
            switch request.action {
            case "status":
                let devices = manager.devices.map { Device(id: $0.key.uuidString, name: $0.value) }
                let state = String(describing: manager.state)
                if let id = manager.connectedID, manager.state == .connected {
                    complete(Response(ok: true, reader: "BLE:" + id.uuidString,
                                      devices: devices, state: state))
                } else {
                    complete(Response(ok: true, devices: devices, state: state,
                                      errorCode: manager.errorCode?.rawValue))
                }
            case "scan":
                manager.scan()
                complete(Response(ok: true))
            case "connect":
                guard let reader = request.reader, let id = UUID(uuidString: reader),
                      manager.devices[id] != nil else {
                    complete(Response(ok: false, errorCode: "ble_reader_not_found"))
                    break
                }
                manager.connect(id)
                if manager.state == .connecting {
                    complete(Response(ok: true, state: "connecting"))
                } else {
                    complete(Response(ok: false,
                                      errorCode: manager.errorCode?.rawValue ?? "ble_connect_rejected"))
                }
            case "disconnect":
                manager.disconnect()
                complete(Response(ok: true))
            case "apdu", "reset":
                let finish: (Result<Data, Error>) -> Void = { result in
                    switch result {
                    case .success(let bytes): complete(Response(ok: true, hex: bytes.hexString))
                    case .failure(let error):
                        let code = (error as? BluetoothReaderTransportError)?.code.rawValue ?? "ble_exchange_failed"
                        complete(Response(ok: false, errorCode: code))
                    }
                }
                if request.action == "reset" {
                    manager.powerCycle(completion: finish)
                } else if let payload = Data(hexString: request.hex ?? "") {
                    manager.exchangeAPDU(payload, completion: finish)
                } else {
                    complete(Response(ok: false, errorCode: "ble_invalid_apdu"))
                }
            default:
                complete(Response(ok: false, errorCode: "ble_unknown_action"))
            }
        }
        let waitSeconds = (request.action == "apdu" || request.action == "reset")
            ? cardOperationTimeout : standardRequestTimeout
        if semaphore.wait(timeout: .now() + waitSeconds) == .timedOut {
            answerLock.lock()
            answer = Response(ok: false, errorCode: "ble_timeout")
            answerLock.unlock()
        }
        answerLock.lock()
        let finalAnswer = answer
        answerLock.unlock()
        send(finalAnswer, fd: fd)
    }

    private func send(_ response: Response, fd: Int32) {
        guard var bytes = try? JSONEncoder().encode(response) else { return }
        bytes.append(10)
        bytes.withUnsafeBytes { raw in
            guard let base = raw.baseAddress else { return }
            var sent = 0
            while sent < raw.count {
                let count = Darwin.write(fd, base.advanced(by: sent), raw.count - sent)
                if count <= 0 { return }
                sent += count
            }
        }
    }

    private func constantTimeEquals(_ first: String, _ second: String) -> Bool {
        let a = Array(first.utf8), b = Array(second.utf8)
        guard a.count == b.count else { return false }
        var difference: UInt8 = 0
        for index in a.indices { difference |= a[index] ^ b[index] }
        return difference == 0
    }

    private struct Request: Decodable {
        let token: String
        let action: String
        let hex: String?
        let reader: String?
    }
    private struct Device: Encodable { let id: String; let name: String }
    private struct Response: Encodable {
        let ok: Bool
        var reader: String? = nil
        var devices: [Device]? = nil
        var state: String? = nil
        var hex: String? = nil
        var errorCode: String? = nil
    }
}

private extension Data {
    init?(hexString: String) {
        guard hexString.count % 2 == 0, hexString.count <= 131_070 else { return nil }
        self.init(capacity: hexString.count / 2)
        var index = hexString.startIndex
        while index < hexString.endIndex {
            let next = hexString.index(index, offsetBy: 2)
            guard let byte = UInt8(hexString[index..<next], radix: 16) else { return nil }
            append(byte)
            index = next
        }
    }
    var hexString: String { map { String(format: "%02x", $0) }.joined() }
}
