import Foundation

/// Hardware-free API fixture. Every response is explicitly released by a test;
/// it never opens a socket or starts the installed application.
enum FixtureError: Error { case failed }

@MainActor
final class ControlledAPI {
    struct Request {
        let method: String
        let path: String
        let continuation: CheckedContinuation<Data, Error>
    }

    static let shared = ControlledAPI()
    private(set) var requests: [Request] = []
    private var completed: Set<Int> = []
    var readerListJSON = #"{"readers":["Fixture Reader"],"selected":"Fixture Reader"}"#

    func request(method: String, path: String) async throws -> Data {
        // These independent reads are deliberately not part of the failure
        // fixture. They keep the aggregate fallback focused on VoWiFi.
        if path == "api/reader/list" {
            return Data(readerListJSON.utf8)
        }
        if path == "api/network/traffic" { return Data("{}".utf8) }
        return try await withCheckedThrowingContinuation { continuation in
            requests.append(Request(method: method, path: path, continuation: continuation))
        }
    }

    func complete(_ index: Int, json: String) {
        precondition(completed.insert(index).inserted)
        requests[index].continuation.resume(returning: Data(json.utf8))
    }

    func fail(_ index: Int) {
        precondition(completed.insert(index).inserted)
        requests[index].continuation.resume(throwing: FixtureError.failed)
    }

    var pendingCount: Int { requests.count - completed.count }
}

@MainActor
final class BackendProcess {
    enum State { case running }
    let state: State = .running
    let usesRustGateway = true
}

struct EmptyResponse: Decodable {}
struct TrafficSnapshot: Decodable {}

func flagEmoji(countryCode: String) -> String { "" }

struct APIClient {
    init(timeoutInterval: TimeInterval = 15) {}

    func get<T: Decodable>(_ path: String) async throws -> T {
        try await decode(method: "GET", path: path)
    }

    func send<T: Decodable, B: Encodable>(
        _ path: String,
        method: String = "POST",
        body: B,
        as: T.Type = T.self
    ) async throws -> T {
        try await decode(method: method, path: path)
    }

    func send<T: Decodable>(
        _ path: String,
        method: String = "POST",
        as: T.Type = T.self
    ) async throws -> T {
        try await decode(method: method, path: path)
    }

    private func decode<T: Decodable>(method: String, path: String) async throws -> T {
        let data = try await ControlledAPI.shared.request(method: method, path: path)
        return try JSONDecoder().decode(T.self, from: data)
    }
}
