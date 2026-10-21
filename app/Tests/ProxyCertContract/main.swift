import Foundation

// Offline MAC-P5 / T35.6 PROXY+CERT contract fixtures.
// Does not touch live WEB, Keychain, or product UDS APIClient.

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

private func asObject(_ value: Any?, label: String) throws -> [String: Any] {
    guard let dict = value as? [String: Any] else {
        throw ContractFailure.message("\(label) must be object")
    }
    return dict
}

private func asArray(_ value: Any?, label: String) throws -> [Any] {
    guard let array = value as? [Any] else {
        throw ContractFailure.message("\(label) must be array")
    }
    return array
}

private func stringField(_ data: [String: Any], _ key: String) throws -> String {
    guard let text = data[key] as? String else {
        throw ContractFailure.message("\(key) must be string")
    }
    return text
}

private func nonEmptyString(_ data: [String: Any], _ key: String) throws -> String {
    let text = try stringField(data, key)
    try require(!text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, "\(key) must be non-empty")
    return text
}

private func boolField(_ data: [String: Any], _ key: String) throws -> Bool {
    guard let value = data[key] as? Bool else {
        throw ContractFailure.message("\(key) must be bool")
    }
    return value
}

private func requireBearer(_ meta: [String: Any]) throws {
    let headers = try asObject(meta["request_headers"], label: "request_headers")
    let auth = try stringField(headers, "Authorization")
    try require(auth.hasPrefix("Bearer "), "Authorization must be Bearer")
    try require(!auth.lowercased().contains("x-proletarius-app-token"), "must not use UDS app token")
}

private func assertUpstreamProxy(_ proxy: [String: Any]) throws {
    for key in ["id", "name", "addr", "username", "password"] {
        _ = try nonEmptyString(proxy, key)
    }
    _ = try boolField(proxy, "enabled")
    try require(try stringField(proxy, "password") == "********", "password must be redacted")
}

private func assertProbe(_ probe: [String: Any]) throws {
    for key in ["reachable", "handshake_ok", "udp_associate_ok", "udp_exchange_ok"] {
        _ = try boolField(probe, key)
    }
}

private func assertHttpsState(_ data: [String: Any]) throws {
    _ = try boolField(data, "enabled")
    _ = try boolField(data, "covers_host")
    for key in ["http_url", "https_url", "fingerprint", "source", "host"] {
        _ = try nonEmptyString(data, key)
    }
    let source = try stringField(data, "source")
    try require(source == "self_signed" || source == "custom", "bad source")
    let names = try asArray(data["names"], label: "names")
    try require(!names.isEmpty, "names empty")
    try require(data["not_after"] != nil, "not_after missing")
}

private func testErrorFixture(_ file: String, code: String, status: Int) throws {
    let doc = try loadJSON(file)
    let meta = try asObject(doc["_meta"], label: "_meta")
    try require((meta["http_status"] as? Int) == status, "http_status")
    try require(try stringField(meta, "expected_code") == code, "expected_code")
    let response = try asObject(doc["response"], label: "response")
    let error = try asObject(response["error"], label: "error")
    try require(try stringField(error, "code") == code, "error.code")
    _ = try nonEmptyString(error, "message")
}

private func runAll() throws {
    // PROXY success
    do {
        let doc = try loadJSON("upstream_proxies_list_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require((meta["http_status"] as? Int) == 200, "list status")
        try require(try stringField(meta, "path") == "/api/upstream-proxies", "list path")
        try requireBearer(meta)
        let data = try asArray(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(!data.isEmpty, "list non-empty")
        try assertUpstreamProxy(try asObject(data[0], label: "proxy0"))
        print("PASS FX-PROXY-LIST-OK")
    }

    do {
        let doc = try loadJSON("upstream_proxies_list_empty.json")
        let data = try asArray(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(data.isEmpty, "empty")
        print("PASS FX-PROXY-LIST-EMPTY")
    }

    do {
        let doc = try loadJSON("upstream_proxy_patch_enable_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(try stringField(meta, "method") == "PATCH", "patch method")
        try requireBearer(meta)
        let request = try asObject(doc["request"], label: "request")
        try require(try boolField(request, "enabled") == true, "request.enabled")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        try assertUpstreamProxy(data)
        _ = try boolField(data, "reconnect_requested")
        print("PASS FX-PROXY-PATCH-OK")
    }

    do {
        let doc = try loadJSON("upstream_proxy_probe_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try requireBearer(meta)
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(try stringField(data, "status") == "probed", "status")
        try assertProbe(try asObject(data["probe"], label: "probe"))
        _ = try nonEmptyString(data, "message")
        print("PASS FX-PROXY-PROBE-OK")
    }

    // CERT success
    do {
        let doc = try loadJSON("https_settings_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try requireBearer(meta)
        try assertHttpsState(try asObject(try asObject(doc["response"], label: "response")["data"], label: "data"))
        print("PASS FX-HTTPS-SETTINGS-OK")
    }

    do {
        let doc = try loadJSON("https_certificate_pem_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try requireBearer(meta)
        try require(try stringField(meta, "content_type") == "application/x-pem-file", "content_type")
        let response = try asObject(doc["response"], label: "response")
        let pem = try stringField(response, "body")
        try require(pem.contains("BEGIN CERTIFICATE") && pem.contains("END CERTIFICATE"), "PEM")
        print("PASS FX-HTTPS-CERT-PEM-OK")
    }

    try testErrorFixture("upstream_proxies_invalid_proxy_id.json", code: "invalid_proxy_id", status: 400)
    print("PASS FX-PROXY-INVALID-ID")
    try testErrorFixture("upstream_proxy_probe_invalid_addr.json", code: "invalid_proxy_addr", status: 400)
    print("PASS FX-PROXY-INVALID-ADDR")
    try testErrorFixture("upstream_proxy_immutable_id.json", code: "immutable_proxy_id", status: 409)
    print("PASS FX-PROXY-IMMUTABLE-ID")
    try testErrorFixture("upstream_proxies_unauthorized.json", code: "unauthorized", status: 401)
    print("PASS FX-PROXY-UNAUTHORIZED")
    try testErrorFixture("upstream_proxies_method_not_allowed.json", code: "method_not_allowed", status: 405)
    print("PASS FX-PROXY-METHOD-NOT-ALLOWED")
    try testErrorFixture("https_unavailable.json", code: "https_unavailable", status: 503)
    print("PASS FX-HTTPS-UNAVAILABLE")
    try testErrorFixture("certificate_unavailable.json", code: "certificate_unavailable", status: 500)
    print("PASS FX-CERT-UNAVAILABLE")
    try testErrorFixture("https_unauthorized.json", code: "unauthorized", status: 401)
    print("PASS FX-HTTPS-UNAUTHORIZED")
    try testErrorFixture("invalid_certificate.json", code: "invalid_certificate", status: 400)
    print("PASS FX-INVALID-CERTIFICATE")

    // Golden + isolation
    do {
        let seq = try loadJSON("golden_path_sequence.json")
        let steps = try asArray(seq["steps"], label: "steps")
        try require(steps.count == 5, "want 5 steps")
        var proxyId: String?
        for stepAny in steps {
            let step = try asObject(stepAny, label: "step")
            let fx = try loadJSON(try stringField(step, "fixture"))
            let meta = try asObject(fx["_meta"], label: "_meta")
            try require(try stringField(meta, "path") == try stringField(step, "path"), "path match")
            let want = try stringField(step, "expect_proxy_id")
            if !want.isEmpty {
                if proxyId == nil { proxyId = want }
                try require(want == proxyId, "proxy id drift")
            }
        }
        let forbidden = try asArray(seq["forbidden"], label: "forbidden").compactMap { ($0 as? [String: Any])?["path"] as? String }
        try require(forbidden.contains("/api/instances*"), "instances forbidden")
        try require(forbidden.contains("/api/local-proxy*"), "local-proxy forbidden")
        print("PASS FX-GOLDEN-PATH")
    }

    for name in [
        "upstream_proxies_list_success.json",
        "upstream_proxy_patch_enable_success.json",
        "upstream_proxy_probe_success.json",
        "https_settings_success.json",
        "https_certificate_pem_success.json",
    ] {
        try requireBearer(try asObject(try loadJSON(name)["_meta"], label: "_meta"))
    }
    print("PASS ISO-NO-UDS-HEADER")

    do {
        let mdd = try loadJSON("mdd_proxy_cert_forbidden.json")
        let meta = try asObject(mdd["_meta"], label: "_meta")
        let required = try asArray(meta["required_paths"], label: "required").compactMap { $0 as? String }
        let forbidden = try asArray(meta["forbidden_paths"], label: "forbidden").compactMap { $0 as? String }
        try require(required.contains("/api/upstream-proxies"), "required upstream")
        try require(required.contains("/api/settings/https"), "required https")
        try require(forbidden.contains("/api/instances/{id}/proxy"), "forbid MDD proxy")
        try require(forbidden.contains("/api/local-proxy"), "forbid local-proxy")
        print("PASS ISO-NO-MDD-PROXY-CERT")
    }

    do {
        let doc = try loadJSON("local_mihomo_not_upstream.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        let upstream = try asObject(meta["upstream_surface"], label: "upstream_surface")
        let paths = try asArray(upstream["paths"], label: "paths").compactMap { $0 as? String }
        try require(paths.contains("/api/upstream-proxies"), "upstream paths")
        print("PASS ISO-LOCAL-MIHOMO-NE-UPSTREAM")
    }

    do {
        let p = try stringField(try asObject(try loadJSON("upstream_proxies_list_success.json")["_meta"], label: "_meta"), "path")
        let h = try stringField(try asObject(try loadJSON("https_settings_success.json")["_meta"], label: "_meta"), "path")
        let c = try stringField(try asObject(try loadJSON("https_certificate_pem_success.json")["_meta"], label: "_meta"), "path")
        try require(p == "/api/upstream-proxies", "proxy path")
        try require(h == "/api/settings/https", "https path")
        try require(c == "/api/settings/https/certificate", "cert path")
        print("PASS ISO-GATEWAY-PATHS")
    }
}

@main
enum ProxyCertContractMain {
    static func main() {
        do {
            print("PASS proxy_cert_fixtures")
            try runAll()
            print("ProxyCertContract: all assertions passed")
        } catch ContractFailure.message(let message) {
            fputs("FAIL \(message)\n", stderr)
            exit(1)
        } catch {
            fputs("FAIL \(error)\n", stderr)
            exit(1)
        }
    }
}
