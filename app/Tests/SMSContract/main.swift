import Foundation

// Offline MAC-P4 / T34.6 SMS contract fixtures.
// Does not touch live WEB, Keychain, or product UDS APIClient.
// When MacRemote lands product Gateway SMS DTOs, prefer linking those
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

private let allowedOutcomes: Set<String> = ["delivered", "accepted_unconfirmed", "partial", "failed"]
private let allowedTransports: Set<String> = ["cellular_at", "ims"]

private func requireBearer(_ meta: [String: Any]) throws {
    let headers = try asObject(meta["request_headers"], label: "request_headers")
    let auth = try stringField(headers, "Authorization")
    try require(auth.hasPrefix("Bearer "), "Authorization must be Bearer")
    try require(!auth.lowercased().contains("x-proletarius-app-token"), "must not use UDS app token")
}

private func assertContact(_ value: Any?) throws {
    let contact = try asObject(value, label: "contact")
    for key in [
        "device_id", "device_name", "modem_imei", "iccid", "imsi", "local_phone", "peer",
        "display_name", "last_message", "last_content", "direction", "last_type",
    ] {
        _ = try nonEmptyString(contact, key)
    }
    for key in ["last_sms_id", "unread_count", "message_count"] {
        _ = try intField(contact, key)
    }
    try require(try stringField(contact, "last_type") == "sms", "last_type must be sms")
    try require(
        try stringField(contact, "last_message") == try stringField(contact, "last_content"),
        "last_message/last_content twin"
    )
    try require(contact["last_timestamp"] != nil, "last_timestamp missing")
}

private func assertThreadMessage(_ value: Any?) throws {
    let message = try asObject(value, label: "thread.message")
    _ = try intField(message, "id")
    for key in [
        "message_id", "device_id", "modem_imei", "iccid", "imsi", "local_phone", "peer",
        "direction", "body", "content", "type", "status", "source", "delivery_state",
    ] {
        _ = try stringField(message, key)
    }
    _ = try intField(message, "parts_total")
    try require(try stringField(message, "type") == "sms", "type must be sms")
    try require(
        try stringField(message, "body") == try stringField(message, "content"),
        "body/content twin"
    )
    try require(message["sender"] != nil, "sender missing")
    try require(message["recipient"] != nil, "recipient missing")
    try require(message["timestamp"] != nil, "timestamp missing")
}

private func assertSendData(_ value: Any?, requireRetrySafeFalse: Bool) throws {
    let data = try asObject(value, label: "send.data")
    _ = try nonEmptyString(data, "message_id")
    _ = try intField(data, "id")
    _ = try intField(data, "parts_total")
    _ = try intField(data, "parts_attempted")
    _ = try intField(data, "parts_accepted")
    _ = try boolField(data, "all_parts_accepted")
    _ = try boolField(data, "submission_accepted")
    _ = try boolField(data, "delivery_confirmed")
    let outcome = try nonEmptyString(data, "outcome")
    try require(allowedOutcomes.contains(outcome), "bad outcome")
    let transport = try nonEmptyString(data, "transport")
    try require(allowedTransports.contains(transport), "bad transport")
    _ = try nonEmptyString(data, "delivery_state")
    _ = try nonEmptyString(data, "submission_state")
    _ = try asArray(data["part_results"], label: "part_results")
    if requireRetrySafeFalse {
        try require(try boolField(data, "retry_safe") == false, "retry_safe must be false")
    }
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
    // Contacts list
    do {
        let doc = try loadJSON("sms_contacts_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 200, "contacts http")
        try require(try stringField(meta, "path") == "/api/sms/contacts", "contacts path")
        try requireBearer(meta)
        let data = try asArray(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(!data.isEmpty, "contacts empty")
        try assertContact(data[0])
        print("PASS FX-CONTACTS-OK")
    }

    do {
        let doc = try loadJSON("sms_contacts_empty.json")
        let data = try asArray(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(data.isEmpty, "want empty contacts")
        print("PASS FX-CONTACTS-EMPTY")
    }

    // Thread detail
    do {
        let doc = try loadJSON("sms_thread_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try requireBearer(meta)
        let data = try asArray(try asObject(doc["response"], label: "response")["data"], label: "data")
        try require(!data.isEmpty, "thread empty")
        for item in data { try assertThreadMessage(item) }
        print("PASS FX-THREAD-OK")
    }

    try assertErrorFixture(file: "sms_thread_invalid_peer.json", code: "invalid_peer", status: 400)
    print("PASS FX-THREAD-INVALID-PEER")

    // Send success + failures
    do {
        let doc = try loadJSON("sms_send_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 202, "send http 202")
        try requireBearer(meta)
        let request = try asObject(doc["request"], label: "request")
        _ = try nonEmptyString(request, "device_id")
        _ = try nonEmptyString(request, "phone")
        _ = try nonEmptyString(request, "message")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        try assertSendData(data, requireRetrySafeFalse: false)
        try require(try boolField(data, "all_parts_accepted") == true, "all_parts_accepted")
        try require(try stringField(data, "outcome") != "failed", "success outcome")
        print("PASS FX-SEND-OK")
    }

    try assertErrorFixture(file: "sms_send_device_required.json", code: "device_required", status: 400)
    print("PASS FX-SEND-DEVICE-REQUIRED")

    do {
        let doc = try loadJSON("sms_send_rate_limited.json")
        try require(try asObject(doc["_meta"], label: "_meta")["http_status"] as? Int == 429, "429")
        let error = try asObject(try asObject(doc["response"], label: "response")["error"], label: "error")
        try require(try stringField(error, "code") == "sms_rate_limited", "rate code")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        for key in ["limit", "used", "remaining", "retry_after"] { _ = try intField(data, key) }
        try require(data["reset_at"] != nil, "reset_at")
        print("PASS FX-SEND-RATE-LIMITED")
    }

    try assertErrorFixture(file: "sms_send_device_feature_unsupported.json", code: "device_feature_unsupported", status: 501)
    print("PASS FX-SEND-FEATURE-UNSUPPORTED")

    do {
        let doc = try loadJSON("sms_send_submission_failed.json")
        try require(try asObject(doc["_meta"], label: "_meta")["http_status"] as? Int == 502, "502")
        let error = try asObject(try asObject(doc["response"], label: "response")["error"], label: "error")
        try require(try stringField(error, "code") == "sms_submission_failed", "fail code")
        try assertSendData(try asObject(doc["response"], label: "response")["data"], requireRetrySafeFalse: true)
        print("PASS FX-SEND-SUBMISSION-FAILED")
    }

    try assertErrorFixture(file: "sms_contacts_unauthorized.json", code: "unauthorized", status: 401)
    print("PASS FX-CONTACTS-UNAUTHORIZED")
    try assertErrorFixture(file: "sms_send_physical_missing.json", code: "physical_device_missing", status: 503)
    print("PASS FX-SEND-PHYS-MISSING")
    try assertErrorFixture(file: "sms_send_manager_unavailable.json", code: "device_manager_unavailable", status: 503)
    print("PASS FX-SEND-MGR-UNAVAIL")

    // Golden path
    do {
        let seq = try loadJSON("golden_path_sequence.json")
        let steps = try asArray(seq["steps"], label: "steps")
        var deviceId: String?
        var peer: String?
        for raw in steps.sorted(by: {
            (try? intField(try asObject($0, label: "a"), "order") ?? 0) ?? 0
                < (try? intField(try asObject($1, label: "b"), "order") ?? 0) ?? 0
        }) {
            let step = try asObject(raw, label: "step")
            let fixtureName = try stringField(step, "fixture")
            let wantId = try stringField(step, "expect_device_id")
            let wantPeer = try stringField(step, "expect_peer")
            if deviceId == nil {
                deviceId = wantId
                peer = wantPeer
            }
            try require(wantId == deviceId, "device id drift")
            try require(wantPeer == peer, "peer drift")
            let fx = try loadJSON(fixtureName)
            let meta = try asObject(fx["_meta"], label: "_meta")
            try require(meta["path"] as? String == step["path"] as? String, "path mismatch")
        }
        print("PASS FX-GOLDEN-PATH")
    }

    // Isolation
    for name in ["sms_contacts_success.json", "sms_thread_success.json", "sms_send_success.json"] {
        try requireBearer(try asObject(try loadJSON(name)["_meta"], label: "_meta"))
    }
    print("PASS ISO-NO-UDS-HEADER")

    do {
        let mdd = try loadJSON("mdd_sms_forbidden.json")
        let meta = try asObject(mdd["_meta"], label: "_meta")
        let forbidden = try asArray(meta["forbidden_paths"], label: "forbidden_paths").compactMap { $0 as? String }
        let required = try asArray(meta["required_paths"], label: "required_paths").compactMap { $0 as? String }
        for need in ["/api/sms/contacts", "/api/sms/thread", "/api/sms/send"] {
            try require(required.contains(need), "required missing \(need)")
        }
        for bad in ["/api/instances/{id}/sms", "/api/instances/{id}/sms/send"] {
            try require(forbidden.contains(bad), "forbidden missing \(bad)")
        }
        print("PASS ISO-NO-MDD-SMS")
    }

    do {
        try require(
            try stringField(try asObject(try loadJSON("sms_contacts_success.json")["_meta"], label: "m"), "path")
                == "/api/sms/contacts",
            "contacts path"
        )
        try require(
            try stringField(try asObject(try loadJSON("sms_thread_success.json")["_meta"], label: "m"), "path")
                == "/api/sms/thread",
            "thread path"
        )
        try require(
            try stringField(try asObject(try loadJSON("sms_send_success.json")["_meta"], label: "m"), "path")
                == "/api/sms/send",
            "send path"
        )
        print("PASS ISO-GATEWAY-PATHS")
    }

    print("PASS sms_fixtures")
    print("SMSContract: all assertions passed")
}

@main
enum SMSContractMain {
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
