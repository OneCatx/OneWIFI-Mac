import Foundation

// Offline MAC-RP3 / T43.6 Calls control-plane contract fixtures.
// Does not touch live WEB, Keychain, product UDS APIClient, or WebRTC.
// When MacRemote lands product Gateway Calls DTOs, prefer linking those
// Sources instead of the test-local mirrors below — assertions stay strict.

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

private func intField(_ data: [String: Any], _ key: String) throws -> Int {
    if let value = data[key] as? Int { return value }
    if let value = data[key] as? NSNumber { return value.intValue }
    throw ContractFailure.message("\(key) must be integral")
}

private let allowedDirections: Set<String> = ["inbound", "outbound"]
private let devicePrefix = "/api/devices/dev-ec25-01/calls"

private func requireBearer(_ meta: [String: Any]) throws {
    let headers = try asObject(meta["request_headers"], label: "request_headers")
    let auth = try stringField(headers, "Authorization")
    try require(auth.hasPrefix("Bearer "), "Authorization must be Bearer")
    try require(!auth.lowercased().contains("x-proletarius-app-token"), "must not use UDS app token")
}

private func assertCall(_ value: Any?, label: String) throws {
    let call = try asObject(value, label: label)
    _ = try nonEmptyString(call, "id")
    _ = try nonEmptyString(call, "number")
    let direction = try nonEmptyString(call, "direction")
    try require(allowedDirections.contains(direction), "bad direction")
    _ = try nonEmptyString(call, "state")
    try require(call["started_at"] != nil, "started_at missing")
}

private func assertHistoryItem(_ value: Any?) throws {
    let item = try asObject(value, label: "historyItem")
    _ = try intField(item, "id")
    _ = try nonEmptyString(item, "number_tail")
    let direction = try nonEmptyString(item, "direction")
    try require(allowedDirections.contains(direction), "bad history direction")
    _ = try nonEmptyString(item, "path")
    _ = try nonEmptyString(item, "state")
    try require(item["started_at"] != nil, "started_at missing")
    try require(item["ended_at"] != nil, "ended_at missing")
    _ = try intField(item, "duration_seconds")
}

private func assertErrorFixture(file: String, code: String, status: Int) throws {
    let doc = try loadJSON(file)
    let meta = try asObject(doc["_meta"], label: "_meta")
    try require(meta["http_status"] as? Int == status, "\(file) http_status")
    try require(try stringField(meta, "expected_code") == code, "\(file) expected_code")
    let error = try asObject(try asObject(doc["response"], label: "response")["error"], label: "error")
    try require(try stringField(error, "code") == code, "\(file) error.code")
    _ = try nonEmptyString(error, "message")
}

private func run() throws {
    // List
    do {
        let doc = try loadJSON("calls_list_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 200, "list http")
        try require(try stringField(meta, "path") == "\(devicePrefix)", "list path")
        try requireBearer(meta)
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        let calls = try asArray(data["calls"], label: "calls")
        try require(!calls.isEmpty, "calls empty")
        try assertCall(calls[0], label: "list.call")
        print("PASS FX-LIST-OK")
    }

    do {
        let doc = try loadJSON("calls_list_empty.json")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        let calls = try asArray(data["calls"], label: "calls")
        try require(calls.isEmpty, "want empty calls")
        print("PASS FX-LIST-EMPTY")
    }

    // Dial
    do {
        let doc = try loadJSON("calls_dial_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 202, "dial http 202")
        try require(try stringField(meta, "path") == "\(devicePrefix)/dial", "dial path")
        try requireBearer(meta)
        let request = try asObject(doc["request"], label: "request")
        _ = try nonEmptyString(request, "number")
        try assertCall(try asObject(doc["response"], label: "response")["data"], label: "dial.data")
        print("PASS FX-DIAL-OK")
    }

    // Answer
    do {
        let doc = try loadJSON("calls_answer_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 200, "answer http")
        try requireBearer(meta)
        let request = try asObject(doc["request"], label: "request")
        _ = try nonEmptyString(request, "id")
        let call = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        try assertCall(call, label: "answer.data")
        try require(try stringField(call, "state") == "active", "answer state")
        print("PASS FX-ANSWER-OK")
    }

    // Hangup
    do {
        let doc = try loadJSON("calls_hangup_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 200, "hangup http")
        try requireBearer(meta)
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(try boolField(data, "ended") == true, "ended")
        _ = try nonEmptyString(data, "id")
        print("PASS FX-HANGUP-OK")
    }

    // History
    do {
        let doc = try loadJSON("calls_history_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 200, "history http")
        try require(try stringField(meta, "path") == "\(devicePrefix)/history", "history path")
        try requireBearer(meta)
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        let calls = try asArray(data["calls"], label: "history.calls")
        try require(!calls.isEmpty, "history empty")
        try assertHistoryItem(calls[0])
        print("PASS FX-HISTORY-OK")
    }

    do {
        let doc = try loadJSON("calls_history_empty.json")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        let calls = try asArray(data["calls"], label: "history.calls")
        try require(calls.isEmpty, "want empty history")
        print("PASS FX-HISTORY-EMPTY")
    }

    // Failures
    try assertErrorFixture(file: "calls_dial_invalid_number.json", code: "invalid_dial_number", status: 400)
    print("PASS FX-DIAL-INVALID-NUMBER")
    try assertErrorFixture(file: "calls_answer_invalid_call_id.json", code: "invalid_call_id", status: 400)
    print("PASS FX-ANSWER-INVALID-CALL-ID")
    try assertErrorFixture(file: "calls_hangup_call_not_found.json", code: "call_not_found", status: 404)
    print("PASS FX-HANGUP-CALL-NOT-FOUND")
    try assertErrorFixture(file: "calls_answer_call_state.json", code: "call_state", status: 409)
    print("PASS FX-ANSWER-CALL-STATE")
    try assertErrorFixture(file: "calls_list_unauthorized.json", code: "unauthorized", status: 401)
    print("PASS FX-LIST-UNAUTHORIZED")
    try assertErrorFixture(file: "calls_list_device_not_found.json", code: "device_not_found", status: 404)
    print("PASS FX-LIST-DEVICE-NOT-FOUND")
    try assertErrorFixture(file: "calls_dial_vowifi_unavailable.json", code: "vowifi_runtime_unavailable", status: 503)
    print("PASS FX-DIAL-VOWIFI-UNAVAIL")
    try assertErrorFixture(file: "calls_history_unavailable.json", code: "call_history_unavailable", status: 503)
    print("PASS FX-HISTORY-UNAVAIL")

    // Golden path
    do {
        let seq = try loadJSON("golden_path_sequence.json")
        let steps = try asArray(seq["steps"], label: "steps")
        try require(steps.count == 5, "want 5 steps")
        var deviceId: String?
        var callId: String?
        for raw in steps.sorted(by: {
            (try? intField(try asObject($0, label: "a"), "order") ?? 0) ?? 0
                < (try? intField(try asObject($1, label: "b"), "order") ?? 0) ?? 0
        }) {
            let step = try asObject(raw, label: "step")
            let fixtureName = try stringField(step, "fixture")
            let wantId = try stringField(step, "expect_device_id")
            let wantCall = try stringField(step, "expect_call_id")
            if deviceId == nil {
                deviceId = wantId
                callId = wantCall
            }
            try require(wantId == deviceId, "device id drift")
            try require(wantCall == callId, "call id drift")
            let fx = try loadJSON(fixtureName)
            let meta = try asObject(fx["_meta"], label: "_meta")
            try require(meta["path"] as? String == step["path"] as? String, "path mismatch")
        }
        print("PASS FX-GOLDEN-PATH")
    }

    // Isolation
    for name in [
        "calls_list_success.json",
        "calls_dial_success.json",
        "calls_answer_success.json",
        "calls_hangup_success.json",
        "calls_history_success.json",
    ] {
        try requireBearer(try asObject(try loadJSON(name)["_meta"], label: "_meta"))
    }
    print("PASS ISO-NO-UDS-HEADER")

    do {
        let mdd = try loadJSON("mdd_calls_forbidden.json")
        let meta = try asObject(mdd["_meta"], label: "_meta")
        let forbidden = try asArray(meta["forbidden_paths"], label: "forbidden_paths").compactMap { $0 as? String }
        let required = try asArray(meta["required_paths"], label: "required_paths").compactMap { $0 as? String }
        for need in [
            "/api/devices/{id}/calls",
            "/api/devices/{id}/calls/dial",
            "/api/devices/{id}/calls/answer",
            "/api/devices/{id}/calls/hangup",
            "/api/devices/{id}/calls/history",
        ] {
            try require(required.contains(need), "required missing \(need)")
        }
        for bad in ["/api/instances/{id}/call", "/api/instances/{id}/calls", "/api/instances/{id}/calls/dial"] {
            try require(forbidden.contains(bad), "forbidden missing \(bad)")
        }
        print("PASS ISO-NO-MDD-CALLS")
    }

    do {
        let webrtc = try loadJSON("no_webrtc_forbidden.json")
        let meta = try asObject(webrtc["_meta"], label: "_meta")
        let forbidden = try asArray(meta["forbidden_paths"], label: "forbidden_paths").compactMap { $0 as? String }
        for bad in [
            "/api/devices/{id}/calls/webrtc",
            "/api/calls/events",
            "/api/calls/media/capabilities",
            "/api/calls/push",
        ] {
            try require(forbidden.contains(bad), "webrtc forbidden missing \(bad)")
        }
        print("PASS ISO-NO-WEBRTC")
    }

    do {
        try require(
            try stringField(try asObject(try loadJSON("calls_list_success.json")["_meta"], label: "m"), "path")
                == "\(devicePrefix)",
            "list path"
        )
        try require(
            try stringField(try asObject(try loadJSON("calls_dial_success.json")["_meta"], label: "m"), "path")
                == "\(devicePrefix)/dial",
            "dial path"
        )
        try require(
            try stringField(try asObject(try loadJSON("calls_answer_success.json")["_meta"], label: "m"), "path")
                == "\(devicePrefix)/answer",
            "answer path"
        )
        try require(
            try stringField(try asObject(try loadJSON("calls_hangup_success.json")["_meta"], label: "m"), "path")
                == "\(devicePrefix)/hangup",
            "hangup path"
        )
        try require(
            try stringField(try asObject(try loadJSON("calls_history_success.json")["_meta"], label: "m"), "path")
                == "\(devicePrefix)/history",
            "history path"
        )
        print("PASS ISO-GATEWAY-PATHS")
    }

    print("PASS calls_fixtures")
    print("CallsContract: all assertions passed")
}

@main
enum CallsContractMain {
    static func main() {
        do {
            try run()
        } catch ContractFailure.message(let message) {
            fputs("FAIL \(message)\n", stderr)
            exit(1)
        } catch {
            fputs("FAIL \(error)\n", stderr)
            exit(1)
        }
    }
}
