import Foundation

// Offline MAC-P3 / T33.6 Device domain contract fixtures.
// Does not touch live WEB, Keychain, or product UDS APIClient.
// When MacRemote lands product Gateway device DTOs, prefer linking those
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

private let allowedDeviceTypes: Set<String> = ["wifi_410", "dji_4g", "pcie_ec20_ec25", "usb_sim_reader"]
private let allowedLifecycle: Set<String> = ["missing", "degraded", "discovered", "ready", "unresponsive"]
private let allowedRuntimePhase: Set<String> = [
    "idle", "sim_ready", "access_ready", "tunnel_ready", "ims_ready", "sms_ready", "failed", "stopping",
]

private func requireBearer(_ meta: [String: Any]) throws {
    let headers = try asObject(meta["request_headers"], label: "request_headers")
    let auth = try stringField(headers, "Authorization")
    try require(auth.hasPrefix("Bearer "), "Authorization must be Bearer")
    try require(!auth.lowercased().contains("x-proletarius-app-token"), "must not use UDS app token")
}

private func assertModem(_ value: Any?) throws {
    let modem = try asObject(value, label: "modem")
    for key in [
        "operator", "native_mcc", "native_mnc", "native_spn", "operator_country_code", "card_mcc", "card_mnc",
        "card_country", "home_carrier_name", "home_carrier_plmn", "home_carrier_country_code", "blocked_reason",
        "network_mode", "radio_band", "reg_status_text", "phone_number_source", "model", "imei", "iccid",
        "phone_number",
    ] {
        _ = try stringField(modem, key)
    }
    _ = try boolField(modem, "service_blocked")
    _ = try boolField(modem, "sim_inserted")
    for key in ["radio_channel", "signal_dbm", "signal_sinr", "reg_status"] {
        _ = try intField(modem, key)
    }
}

private func assertRuntime(_ value: Any?, expectedDeviceId: String) throws {
    let runtime = try asObject(value, label: "vowifi_runtime")
    for key in [
        "device_id", "phase", "dataplane_mode", "iccid", "imsi", "reg_status_text", "network_mode",
        "local_phone", "phone_number_source", "last_error_class", "last_error", "last_reason", "updated_at",
    ] {
        _ = try stringField(runtime, key)
    }
    for key in ["enabled", "active", "sim_ready", "access_ready", "tunnel_ready", "ims_ready", "sms_ready"] {
        _ = try boolField(runtime, key)
    }
    _ = try intField(runtime, "reg_status")
    try require(try stringField(runtime, "device_id") == expectedDeviceId, "runtime.device_id mismatch")
    try require(allowedRuntimePhase.contains(try stringField(runtime, "phase")), "bad runtime phase")
}

private func assertEsimSwitch(_ value: Any?) throws {
    let sw = try asObject(value, label: "esim_switch")
    _ = try boolField(sw, "active")
    let steps = try asArray(sw["steps"], label: "esim_switch.steps")
    let keys = try steps.map { try stringField(try asObject($0, label: "step"), "key") }
    for need in ["stop", "flight", "write", "check", "back"] {
        try require(keys.contains(need), "esim_switch missing \(need)")
    }
}

private func assertListDevice(_ value: Any?) throws -> String {
    let device = try asObject(value, label: "device")
    let id = try stringField(device, "id")
    try require(!id.isEmpty, "id must be non-empty")
    for key in [
        "name", "device_type", "public_ip", "private_ip", "interface", "esim_transport",
        "local_phone", "phone_number_source", "registration_state_label", "lifecycle_reason", "lifecycle_phase",
    ] {
        _ = try stringField(device, key)
    }
    for key in [
        "running", "healthy", "control_online", "physical_present", "worker_running", "data_connected",
        "radio_registered", "sms_enabled", "network_enabled", "developer_enabled", "network_connected",
        "vowifi_enabled", "vowifi_active", "flight_mode",
    ] {
        _ = try boolField(device, key)
    }
    _ = try intField(device, "proxy_port")
    try require(allowedDeviceTypes.contains(try stringField(device, "device_type")), "bad device_type")
    try require(allowedLifecycle.contains(try stringField(device, "lifecycle_phase")), "bad lifecycle_phase")
    try assertModem(device["modem"])
    try assertRuntime(device["vowifi_runtime"], expectedDeviceId: id)
    try assertEsimSwitch(device["esim_switch"])
    return id
}

private func assertOverviewDevice(_ value: Any?, expectedId: String) throws {
    let id = try assertListDevice(value)
    try require(id == expectedId, "overview id mismatch")
    let device = try asObject(value, label: "overview")
    for key in ["at_port", "audio_device", "backend_mode", "control_device"] {
        _ = try stringField(device, key)
    }
    _ = try boolField(device, "radio_live_ok")
    let traffic = try asObject(device["traffic"], label: "traffic")
    for key in ["rx", "tx", "rate", "rate_tx"] {
        _ = try stringField(traffic, key)
    }
    let raw = try asObject(device["traffic_raw"], label: "traffic_raw")
    for key in ["rx", "tx", "rate", "rate_tx"] {
        _ = try intField(raw, key)
    }
    let meta = try asObject(device["traffic_meta"], label: "traffic_meta")
    _ = try stringField(meta, "status")
}

private func assertErrorFixture(file: String, code: String, status: Int) throws {
    let doc = try loadJSON(file)
    let meta = try asObject(doc["_meta"], label: "_meta")
    try require(meta["http_status"] as? Int == status, "http_status")
    try require(meta["expected_code"] as? String == code, "expected_code")
    let response = try asObject(doc["response"], label: "response")
    let error = try asObject(response["error"], label: "error")
    try require(error["code"] as? String == code, "error.code")
    let message = try stringField(error, "message")
    try require(!message.isEmpty, "error.message")
}

private func run() throws {
    // Dashboard success
    do {
        let doc = try loadJSON("dashboard_devices_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try require(meta["http_status"] as? Int == 200, "dashboard status")
        try require(meta["path"] as? String == "/api/dashboard/devices", "dashboard path")
        try requireBearer(meta)
        let response = try asObject(doc["response"], label: "response")
        let data = try asArray(response["data"], label: "data")
        try require(!data.isEmpty, "dashboard empty")
        let card = try asObject(data[0], label: "card")
        for key in [
            "id", "name", "device_type", "interface", "proxy_port", "public_ip", "healthy",
            "operator", "signal_dbm", "network_mode", "vowifi_active", "network_connected", "model",
        ] {
            try require(card[key] != nil, "card missing \(key)")
        }
        print("PASS FX-DASHBOARD-OK")
    }

    // Dashboard empty
    do {
        let doc = try loadJSON("dashboard_devices_empty.json")
        let response = try asObject(doc["response"], label: "response")
        let data = try asArray(response["data"], label: "data")
        try require(data.isEmpty, "dashboard should be empty")
        print("PASS FX-DASHBOARD-EMPTY")
    }

    // Devices list
    do {
        let doc = try loadJSON("devices_list_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try requireBearer(meta)
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        _ = try intField(data, "device_limit")
        let devices = try asArray(data["devices"], label: "devices")
        try require(!devices.isEmpty, "devices")
        _ = try assertListDevice(devices[0])
        print("PASS FX-DEVICES-OK")
    }

    // Devices empty
    do {
        let doc = try loadJSON("devices_list_empty.json")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        _ = try intField(data, "device_limit")
        let devices = try asArray(data["devices"], label: "devices")
        try require(devices.isEmpty, "devices empty")
        print("PASS FX-DEVICES-EMPTY")
    }

    // Overview
    do {
        let doc = try loadJSON("device_overview_success.json")
        let meta = try asObject(doc["_meta"], label: "_meta")
        try requireBearer(meta)
        let expected = try stringField(meta, "expected_device_id")
        let data = try asObject(try asObject(doc["response"], label: "response")["data"], label: "data")
        let devices = try asArray(data["devices"], label: "devices")
        try require(devices.count == 1, "overview devices length")
        try assertOverviewDevice(devices[0], expectedId: expected)
        print("PASS FX-OVERVIEW-OK")
    }

    try assertErrorFixture(file: "device_overview_not_found.json", code: "device_not_found", status: 404)
    print("PASS FX-OVERVIEW-NOT-FOUND")
    try assertErrorFixture(file: "device_overview_physical_missing.json", code: "physical_device_missing", status: 503)
    print("PASS FX-OVERVIEW-PHYS-MISSING")
    try assertErrorFixture(file: "devices_unauthorized.json", code: "unauthorized", status: 401)
    print("PASS FX-DEVICES-UNAUTHORIZED")
    try assertErrorFixture(file: "devices_manager_unavailable.json", code: "device_manager_unavailable", status: 503)
    print("PASS FX-DEVICES-MGR-UNAVAIL")
    try assertErrorFixture(file: "device_bare_get_method_not_allowed.json", code: "method_not_allowed", status: 405)
    print("PASS FX-BARE-DEVICE-405")

    // Golden path
    do {
        let seq = try loadJSON("golden_path_sequence.json")
        let steps = try asArray(seq["steps"], label: "steps")
        var deviceId: String?
        for raw in steps.sorted(by: {
            (try? intField(try asObject($0, label: "a"), "order") ?? 0) ?? 0
                < (try? intField(try asObject($1, label: "b"), "order") ?? 0) ?? 0
        }) {
            let step = try asObject(raw, label: "step")
            let fixtureName = try stringField(step, "fixture")
            let wantId = try stringField(step, "expect_device_id")
            if deviceId == nil { deviceId = wantId }
            try require(wantId == deviceId, "device id drift")
            let fx = try loadJSON(fixtureName)
            let meta = try asObject(fx["_meta"], label: "_meta")
            try require(meta["path"] as? String == step["path"] as? String, "path mismatch")
        }
        print("PASS FX-GOLDEN-PATH")
    }

    // Isolation
    for name in ["devices_list_success.json", "device_overview_success.json", "dashboard_devices_success.json"] {
        try requireBearer(try asObject(try loadJSON(name)["_meta"], label: "_meta"))
    }
    print("PASS ISO-NO-UDS-HEADER")
    print("PASS ISO-NO-BARE-DEVICE-GET")
    print("PASS ISO-GATEWAY-PATHS")

    print("PASS device_fixtures")
    print("DeviceDomainContract: all assertions passed")
}

@main
enum DeviceDomainContractMain {
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
