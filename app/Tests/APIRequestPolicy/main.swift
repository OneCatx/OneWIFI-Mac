import Foundation

enum LocalBackendAuthentication {
    static let appToken = "isolated-test-token"
    static let headerName = "X-Test-Token"
}
final class UnixSocketURLProtocol: URLProtocol, @unchecked Sendable {
    private static let lock = NSLock()
    private static var recorded: [URLRequest] = []
    private static var failure: URLError.Code?
    private static var httpStatus = 200
    private static var responseBody = "{}"
    static func configure(error: URLError.Code? = nil, status: Int = 200, body: String = "{}") {
        lock.lock()
        defer { lock.unlock() }
        recorded = []
        failure = error
        httpStatus = status
        responseBody = body
    }
    static var requests: [URLRequest] {
        lock.lock()
        defer { lock.unlock() }
        return recorded
    }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.lock.lock()
        Self.recorded.append(request)
        let failure = Self.failure
        let status = Self.httpStatus
        let body = Self.responseBody
        Self.lock.unlock()
        if let failure {
            client?.urlProtocol(self, didFailWithError: URLError(failure))
        } else {
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: nil)!
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: Data(body.utf8))
            client?.urlProtocolDidFinishLoading(self)
        }
    }
    override func stopLoading() {}
}

struct CheckFailure: Error, CustomStringConvertible { let description: String }
func expect(_ condition: @autoclosure () -> Bool, _ message: String) throws {
    if !condition() { throw CheckFailure(description: message) }
}
struct EmptyResponse: Decodable {}

@main enum APIRequestPolicyTests {
    static func main() async throws {
        for path in ["api/sms/send", "/api/esim/switch", "api/esim/download", "api/reader/esim/switch",
                     "api/reader/esim/download", "api/vowifi", "api/reader/vowifi"] {
            try expect(APIClient.mutationTimeout(path: path, method: "POST", defaultTimeout: 30) == 190,
                       "long write budget not applied: \(path)")
            try expect(APIClient.mutationTimeout(path: path, method: "GET", defaultTimeout: 30) == 30,
                       "read budget was extended: \(path)")
        }
        for path in ["api/esim/profile", "api/reader/esim/profile"] {
            for method in ["DELETE", "PATCH"] {
                try expect(APIClient.mutationTimeout(path: path, method: method, defaultTimeout: 130) == 190,
                           "profile budget not applied")
            }
        }
        for path in ["", "api/reader/vowifi/cancel", "api/sms/send-extra", "api/sms/refresh"] {
            try expect(APIClient.mutationTimeout(path: path, method: "POST", defaultTimeout: 30) == 30,
                       "unrelated request changed: \(path)")
        }
        try expect(APIClient.mutationTimeout(path: "api/esim/switch?x=y", method: "post", defaultTimeout: 240) == 240,
                   "explicit longer budget was reduced")
        print("PASS: exact mutation whitelist; GET/ordinary actions unchanged")

        for (path, configured, expected) in [("api/sms/send", 30.0, 190.0), ("api/reader/esim/switch", 130.0, 190.0),
                                            ("api/esim/download", 240.0, 240.0), ("api/sms/refresh", 30.0, 30.0)] {
            UnixSocketURLProtocol.configure()
            let _: EmptyResponse = try await APIClient(timeoutInterval: configured).send(path)
            try expect(UnixSocketURLProtocol.requests.count == 1, "successful write repeated")
            try expect(UnixSocketURLProtocol.requests.first?.timeoutInterval == expected, "actual request timeout mismatch")
            try expect(UnixSocketURLProtocol.requests.first?.value(forHTTPHeaderField: "X-Test-Token") == "isolated-test-token",
                       "extended session dropped authentication")
        }
        print("PASS: production URLSession receives policy and authentication exactly once")

        for code in [URLError.Code.timedOut, .networkConnectionLost, .badServerResponse] {
            UnixSocketURLProtocol.configure(error: code)
            do {
                try await APIClient().send("api/sms/send")
                throw CheckFailure(description: "transport failure succeeded")
            } catch let error as APIOperationOutcomeUnknownError {
                try expect(operationOutcomeIsUnknown(error) && smsSendErrorMessage(error).contains("不要重复提交"),
                           "ambiguous write did not warn against duplicate submission")
                try expect(!smsSendErrorMessage(error).hasPrefix("发送失败"), "unknown result labelled definite failure")
            }
            try expect(UnixSocketURLProtocol.requests.count == 1, "failed write automatically retried")
        }
        print("PASS: ambiguous transport failures are pending confirmation; zero automatic retries")

        UnixSocketURLProtocol.configure(body: "unparseable 2xx response")
        do {
            let _: EmptyResponse = try await APIClient().send("api/sms/send")
            throw CheckFailure(description: "malformed write result succeeded")
        } catch let error as APIOperationOutcomeUnknownError {
            try expect(error.localizedDescription.contains("无法解析操作结果"), "2xx decode error encouraged resubmission")
        }
        try expect(UnixSocketURLProtocol.requests.count == 1, "malformed 2xx write automatically retried")
        print("PASS: successful HTTP write with malformed result remains pending confirmation")

        UnixSocketURLProtocol.configure(error: .cannotConnectToHost)
        do {
            try await APIClient().send("api/sms/send")
            throw CheckFailure(description: "disconnected service succeeded")
        } catch let error as URLError {
            try expect(!operationOutcomeIsUnknown(error), "failed connect falsely reported submitted")
        }
        UnixSocketURLProtocol.configure(error: .timedOut)
        do {
            let _: EmptyResponse = try await APIClient().get("api/sms/status")
            throw CheckFailure(description: "GET timeout succeeded")
        } catch let error as URLError {
            try expect(error.code == .timedOut, "GET timeout semantics changed")
        }
        UnixSocketURLProtocol.configure(status: 504, body: "{\"error\":\"短信结果待确认，已确认 1/2 段，请勿重发\",\"code\":\"sms_send_outcome_unknown\",\"recoverable\":false}")
        do {
            try await APIClient().send("api/sms/send")
            throw CheckFailure(description: "unknown remote outcome succeeded")
        } catch let error as APIError {
            try expect(operationOutcomeIsUnknown(error) && smsSendErrorMessage(error).contains("1/2"),
                       "backend partial-segment evidence lost")
            try expect(!error.isRecoverable && !smsSendErrorMessage(error).hasPrefix("发送失败"),
                       "backend uncertain outcome was retryable or marked failed")
        }
        print("PASS: ordinary failures preserved; server uncertainty and confirmed segments retained")
        print("API request policy: in-memory phase passed (no sockets/credentials/hardware)")
    }
}
