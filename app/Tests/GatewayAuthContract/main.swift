import Foundation

// Offline MAC-P2 / T32.6 Gateway auth contract fixtures.
// Does not touch the running backend, Keychain, or product UDS APIClient.
// When MacRemote lands product Gateway DTO / BaseUrlPolicy types, prefer linking
// those Sources instead of the test-local mirrors below — assertions stay strict.

private enum ContractFailure: Error {
    case message(String)
}

private func require(_ condition: @autoclosure () -> Bool, _ message: String) throws {
    if !condition() { throw ContractFailure.message(message) }
}

private func fixturesDirectory() -> URL {
    let here = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
    return here.appendingPathComponent("Fixtures", isDirectory: true)
}

private func loadJSON(_ name: String) throws -> [String: Any] {
    let url = fixturesDirectory().appendingPathComponent(name)
    let data = try Data(contentsOf: url)
    let object = try JSONSerialization.jsonObject(with: data)
    guard let dict = object as? [String: Any] else {
        throw ContractFailure.message("\(name): root must be object")
    }
    return dict
}

// MARK: - Test-local DTO mirrors (mobile AuthSessionDto / LogoutDto)

private struct AuthSessionFixture {
    let username: String
    let sessionToken: String
    let expiresAt: Date

    static func fromLoginData(_ value: Any?) throws -> AuthSessionFixture {
        let data = try asObject(value, label: "login.data")
        _ = try nonEmptyString(data["csrf_token"], label: "login.csrf_token")
        try require(data["status"] as? String == "ok", "login.status must be ok")
        return try fromSessionData(data, requireCSRF: false)
    }

    static func fromSessionData(_ value: Any?, requireCSRFAbsent: Bool = false) throws -> AuthSessionFixture {
        let data = try asObject(value, label: "session.data")
        if requireCSRFAbsent {
            try require(data["csrf_token"] == nil, "Bearer session must not include csrf_token")
        }
        return try fromSessionData(data, requireCSRF: false)
    }

    private static func fromSessionData(_ data: [String: Any], requireCSRF: Bool) throws -> AuthSessionFixture {
        if requireCSRF {
            _ = try nonEmptyString(data["csrf_token"], label: "csrf_token")
        }
        let user = try asObject(data["user"], label: "user")
        let username = try nonEmptyString(user["username"], label: "user.username")
        let sessionToken = try nonEmptyString(data["session_token"], label: "session_token")
        try require(data["authenticated"] as? Bool == true, "authenticated must be true")
        let expiresAt = try parseRFC3339(data["expires_at"], label: "expires_at")
        return AuthSessionFixture(username: username, sessionToken: sessionToken, expiresAt: expiresAt)
    }
}

private func requireLogout(_ value: Any?) throws {
    let data = try asObject(value, label: "logout.data")
    try require(data["logged_out"] as? Bool == true, "logged_out must be true")
}

private func asObject(_ value: Any?, label: String) throws -> [String: Any] {
    guard let dict = value as? [String: Any] else {
        throw ContractFailure.message("\(label) must be object")
    }
    return dict
}

private func nonEmptyString(_ value: Any?, label: String) throws -> String {
    guard let text = value as? String, !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
        throw ContractFailure.message("\(label) must be non-empty string")
    }
    return text
}

private func parseRFC3339(_ value: Any?, label: String) throws -> Date {
    let text = try nonEmptyString(value, label: label)
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: text) { return date }
    formatter.formatOptions = [.withInternetDateTime]
    guard let date = formatter.date(from: text) else {
        throw ContractFailure.message("\(label) must be RFC3339")
    }
    return date
}

// MARK: - Test-local BaseUrlPolicy mirror (mobile base_url_policy.dart)

private enum BaseUrlPolicyFixture {
    static func parseAndValidate(_ input: String) throws -> URL {
        let candidate = input.trimmingCharacters(in: .whitespacesAndNewlines)
        try require(!candidate.isEmpty, "服务地址不能为空。")
        guard var components = URLComponents(string: candidate) else {
            throw ContractFailure.message("服务地址无效。")
        }
        let scheme = (components.scheme ?? "").lowercased()
        try require(scheme == "http" || scheme == "https", "服务地址必须是 http:// 或 https://。")
        try require(!(components.host ?? "").isEmpty, "服务地址必须是 http:// 或 https://。")
        try require(components.user == nil && components.password == nil, "服务地址不能包含用户名或密码。")
        let path = components.path
        try require(path.isEmpty || path == "/", "服务地址不能包含路径。")
        try require(components.query == nil, "服务地址不能包含查询参数或片段。")
        try require(components.fragment == nil, "服务地址不能包含查询参数或片段。")
        components.scheme = scheme
        components.host = components.host?.lowercased()
        components.path = ""
        components.query = nil
        components.fragment = nil
        components.user = nil
        components.password = nil
        let defaultPort = scheme == "https" ? 443 : 80
        if let port = components.port, port == defaultPort {
            components.port = nil
        }
        guard let url = components.url else {
            throw ContractFailure.message("服务地址无效。")
        }
        return url
    }

    static func isPublicCleartext(_ origin: URL) -> Bool {
        guard (origin.scheme ?? "").lowercased() == "http" else { return false }
        return !isLoopbackOrPrivate(origin.host ?? "")
    }

    private static func isLoopbackOrPrivate(_ host: String) -> Bool {
        let normalized = host.lowercased()
        if normalized == "localhost" || normalized.hasSuffix(".localhost") { return true }
        let parts = normalized.split(separator: ".").compactMap { Int($0) }
        if parts.count == 4 {
            let first = parts[0], second = parts[1]
            return first == 127
                || first == 10
                || (first == 172 && (16...31).contains(second))
                || (first == 192 && second == 168)
                || (first == 169 && second == 254)
        }
        return false
    }
}

// MARK: - Cases

private func assertErrorFixture(file: String, expectedCode: String, expectedStatus: Int) throws {
    let root = try loadJSON(file)
    let meta = try asObject(root["_meta"], label: "_meta")
    try require(meta["http_status"] as? Int == expectedStatus, "\(file): http_status")
    try require(meta["expected_code"] as? String == expectedCode, "\(file): expected_code meta")
    let response = try asObject(root["response"], label: "response")
    let error = try asObject(response["error"], label: "error")
    try require(error["code"] as? String == expectedCode, "\(file): error.code")
    _ = try nonEmptyString(error["message"], label: "\(file): error.message")
}

private func runAuthFixtures() throws {
    let login = try loadJSON("login_success.json")
    let loginResponse = try asObject(login["response"], label: "login.response")
    let loginSession = try AuthSessionFixture.fromLoginData(loginResponse["data"])
    try require(loginSession.username == "admin", "login username")
    try require(loginSession.sessionToken == "sess-fixture-token-do-not-log", "login session_token")

    let session = try loadJSON("session_bearer_success.json")
    let sessionResponse = try asObject(session["response"], label: "session.response")
    let sessionData = try asObject(sessionResponse["data"], label: "session.data")
    try require(sessionData["csrf_token"] == nil, "Bearer session fixture must omit csrf_token")
    let bearerSession = try AuthSessionFixture.fromSessionData(sessionResponse["data"], requireCSRFAbsent: true)
    try require(bearerSession.sessionToken == loginSession.sessionToken, "session token continuity")

    let logout = try loadJSON("logout_success.json")
    let logoutResponse = try asObject(logout["response"], label: "logout.response")
    try requireLogout(logoutResponse["data"])

    try assertErrorFixture(file: "login_invalid_credentials.json", expectedCode: "invalid_credentials", expectedStatus: 401)
    try assertErrorFixture(file: "login_too_many_attempts.json", expectedCode: "too_many_attempts", expectedStatus: 429)
    try assertErrorFixture(file: "login_https_required.json", expectedCode: "https_required", expectedStatus: 403)
    try assertErrorFixture(file: "login_invalid_request.json", expectedCode: "invalid_request", expectedStatus: 400)
    try assertErrorFixture(file: "session_unauthorized.json", expectedCode: "unauthorized", expectedStatus: 401)
    try assertErrorFixture(file: "session_expired.json", expectedCode: "session_expired", expectedStatus: 401)
    try assertErrorFixture(file: "logout_invalid_csrf.json", expectedCode: "invalid_csrf", expectedStatus: 403)

    let tooMany = try loadJSON("login_too_many_attempts.json")
    let tooManyMeta = try asObject(tooMany["_meta"], label: "too_many._meta")
    let headers = try asObject(tooManyMeta["response_headers"], label: "Retry-After headers")
    try require(headers["Retry-After"] as? String == "60", "Retry-After must be present for 429")
}

private func runBaseUrlFixtures() throws {
    let root = try loadJSON("base_url_policy_cases.json")
    let accept = root["accept"] as? [[String: Any]] ?? []
    let reject = root["reject"] as? [[String: Any]] ?? []
    let cleartext = root["public_cleartext"] as? [[String: Any]] ?? []

    for caseDict in accept {
        let id = caseDict["id"] as? String ?? "?"
        let input = caseDict["origin"] as? String ?? ""
        let expected = caseDict["normalized"] as? String ?? ""
        let url = try BaseUrlPolicyFixture.parseAndValidate(input)
        try require(url.absoluteString == expected, "\(id): got \(url.absoluteString) want \(expected)")
    }
    for caseDict in reject {
        let id = caseDict["id"] as? String ?? "?"
        let input = caseDict["origin"] as? String ?? ""
        var rejected = false
        do {
            _ = try BaseUrlPolicyFixture.parseAndValidate(input)
        } catch {
            rejected = true
        }
        try require(rejected, "\(id): expected reject for \(input)")
    }
    for caseDict in cleartext {
        let id = caseDict["id"] as? String ?? "?"
        let input = caseDict["origin"] as? String ?? ""
        let expected = caseDict["is_public_cleartext"] as? Bool ?? false
        let url = try BaseUrlPolicyFixture.parseAndValidate(input)
        try require(BaseUrlPolicyFixture.isPublicCleartext(url) == expected, "\(id): cleartext flag")
    }
}

private func runIsolationAssertions() throws {
    // Contract: Gateway HTTPS must never be routed through local UDS APIClient.
    let loginMeta = try asObject(try loadJSON("login_success.json")["_meta"], label: "login._meta")
    try require(loginMeta["path"] as? String == "/api/auth/login", "login path")
    try require(loginMeta["method"] as? String == "POST", "login method")
    let sessionMeta = try asObject(try loadJSON("session_bearer_success.json")["_meta"], label: "session._meta")
    let headers = try asObject(sessionMeta["request_headers"], label: "session headers")
    let auth = try nonEmptyString(headers["Authorization"], label: "Authorization")
    try require(auth.hasPrefix("Bearer "), "Authorization must be Bearer")
    try require(!auth.lowercased().contains("x-proletarius-app-token"), "must not use local app token header")
}

@main
enum GatewayAuthContractTests {
    static func main() throws {
        var failures: [String] = []
        func run(_ name: String, _ body: () throws -> Void) {
            do {
                try body()
                print("PASS \(name)")
            } catch ContractFailure.message(let message) {
                failures.append("\(name): \(message)")
                print("FAIL \(name): \(message)")
            } catch {
                failures.append("\(name): \(error)")
                print("FAIL \(name): \(error)")
            }
        }
        run("auth_fixtures", runAuthFixtures)
        run("base_url_policy", runBaseUrlFixtures)
        run("isolation_headers", runIsolationAssertions)
        if !failures.isEmpty {
            fputs("GatewayAuthContract failures:\n\(failures.joined(separator: "\n"))\n", stderr)
            exit(1)
        }
        print("GatewayAuthContract: all assertions passed")
    }
}
