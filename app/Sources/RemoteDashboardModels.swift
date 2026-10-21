import Foundation

/// Remote dashboard / device summary DTOs.
/// Field set aligned with Android `overview_dto.dart` and WEB
/// `handleDashboardHost` / `dashboardDevices`. No invented keys.
enum RemoteDeviceRuntimePhase: String, Equatable {
    case idle
    case simReady = "sim_ready"
    case accessReady = "access_ready"
    case tunnelReady = "tunnel_ready"
    case imsReady = "ims_ready"
    case smsReady = "sms_ready"
    case failed
    case stopping
}

struct RemoteHostOverview: Equatable {
    let cpuModel: String
    let boardModel: String
    let cpuPercent: Double
    let memoryPercent: Double
    let memoryUsedBytes: Int
    let memoryTotalBytes: Int
    let diskPercent: Double
    let diskUsedBytes: Int
    let diskTotalBytes: Int
    let netRxBps: Double
    let netTxBps: Double

    static func parse(_ value: Any) throws -> RemoteHostOverview {
        let data = try gatewayMap(value)
        let host = try gatewayMap(data["host"])
        let perf = try gatewayMap(data["perf"])
        // WEB always sends these; Dart validates presence without storing.
        _ = try gatewayString(host["memory_model"])
        _ = try gatewayString(host["disk_model"])
        return RemoteHostOverview(
            cpuModel: try gatewayString(host["cpu_model"]),
            boardModel: try gatewayString(host["board_model"]),
            cpuPercent: try gatewayPercentage(perf["cpu_percent"]),
            memoryPercent: try gatewayPercentage(perf["memory_percent"]),
            memoryUsedBytes: try gatewayNonNegativeInt(perf["memory_used_bytes"]),
            memoryTotalBytes: try gatewayNonNegativeInt(perf["memory_total_bytes"]),
            diskPercent: try gatewayPercentage(perf["disk_percent"]),
            diskUsedBytes: try gatewayNonNegativeInt(perf["disk_used_bytes"]),
            diskTotalBytes: try gatewayNonNegativeInt(perf["disk_total_bytes"]),
            netRxBps: try gatewayNonNegativeDouble(perf["net_rx_bps"]),
            netTxBps: try gatewayNonNegativeDouble(perf["net_tx_bps"])
        )
    }
}

struct RemoteDeviceRuntimeOverview: Equatable {
    let phase: RemoteDeviceRuntimePhase
    let simReady: Bool
    let accessReady: Bool
    let tunnelReady: Bool
    let imsReady: Bool
    let smsReady: Bool

    static func parse(_ value: Any?) throws -> RemoteDeviceRuntimeOverview {
        let data = try gatewayMap(value)
        _ = try gatewayString(data["device_id"])
        _ = try gatewayBool(data["enabled"])
        _ = try gatewayBool(data["active"])
        _ = try gatewayString(data["dataplane_mode"])
        _ = try gatewayString(data["iccid"])
        _ = try gatewayString(data["imsi"])
        _ = try gatewayInt(data["reg_status"])
        _ = try gatewayString(data["reg_status_text"])
        _ = try gatewayString(data["network_mode"])
        _ = try gatewayString(data["local_phone"])
        _ = try gatewayString(data["phone_number_source"])
        _ = try gatewayString(data["last_error_class"])
        _ = try gatewayString(data["last_error"])
        _ = try gatewayString(data["last_reason"])
        _ = try gatewayString(data["updated_at"])
        return RemoteDeviceRuntimeOverview(
            phase: try RemoteDeviceRuntimePhase.parse(data["phase"]),
            simReady: try gatewayBool(data["sim_ready"]),
            accessReady: try gatewayBool(data["access_ready"]),
            tunnelReady: try gatewayBool(data["tunnel_ready"]),
            imsReady: try gatewayBool(data["ims_ready"]),
            smsReady: try gatewayBool(data["sms_ready"])
        )
    }
}

extension RemoteDeviceRuntimePhase {
    static func parse(_ value: Any?) throws -> RemoteDeviceRuntimePhase {
        let raw = try gatewayString(value)
        guard let phase = RemoteDeviceRuntimePhase(rawValue: raw) else {
            throw GatewayHTTPError.dtoShape
        }
        return phase
    }

    var displayName: String {
        switch self {
        case .idle: return "空闲"
        case .simReady: return "SIM 就绪"
        case .accessReady: return "接入就绪"
        case .tunnelReady: return "隧道就绪"
        case .imsReady: return "IMS 就绪"
        case .smsReady: return "短信就绪"
        case .failed: return "失败"
        case .stopping: return "停止中"
        }
    }
}

struct RemoteDeviceOverview: Identifiable, Equatable {
    let id: String
    let name: String
    let model: String
    let type: String
    let healthy: Bool
    let operatorName: String
    let signalDbm: Int
    let networkMode: String
    let vowifiActive: Bool
    let networkConnected: Bool
    let runtime: RemoteDeviceRuntimeOverview

    static func parse(_ value: Any) throws -> RemoteDeviceOverview {
        let data = try gatewayMap(value)
        _ = try gatewayString(data["interface"])
        _ = try gatewayInt(data["proxy_port"])
        _ = try gatewayString(data["public_ip"])
        _ = try gatewayString(data["network_duplex"])
        return RemoteDeviceOverview(
            id: try gatewayString(data["id"]),
            name: try gatewayString(data["name"]),
            model: try gatewayString(data["model"]),
            type: try gatewayString(data["device_type"]),
            healthy: try gatewayBool(data["healthy"]),
            operatorName: try gatewayString(data["operator"]),
            signalDbm: try gatewayInt(data["signal_dbm"]),
            networkMode: try gatewayString(data["network_mode"]),
            vowifiActive: try gatewayBool(data["vowifi_active"]),
            networkConnected: try gatewayBool(data["network_connected"]),
            runtime: try RemoteDeviceRuntimeOverview.parse(data["vowifi_runtime"])
        )
    }

    static func parseList(_ value: Any) throws -> [RemoteDeviceOverview] {
        guard let list = value as? [Any] else { throw GatewayHTTPError.dtoShape }
        return try list.map { try parse($0) }
    }
}

/// GET /api/devices/{id}/overview → `{ devices: [one] }` (+ traffic triad).
struct RemoteDeviceOverviewDetail: Equatable {
    let id: String
    let name: String
    let deviceType: String
    let healthy: Bool
    let physicalPresent: Bool
    let controlOnline: Bool
    let networkConnected: Bool
    let vowifiEnabled: Bool
    let vowifiActive: Bool
    let publicIP: String
    let interfaceName: String
    let localPhone: String
    let operatorName: String
    let signalDbm: Int
    let networkMode: String
    let modemModel: String
    let runtime: RemoteDeviceRuntimeOverview
    let traffic: RemoteDeviceTraffic?

    static func parse(_ value: Any, expectedDeviceId: String) throws -> RemoteDeviceOverviewDetail {
        let data = try gatewayMap(value)
        let devices = try gatewayList(data["devices"])
        guard devices.count == 1 else { throw GatewayHTTPError.dtoShape }
        let device = try gatewayMap(devices[0])
        let id = try gatewayNonEmptyString(device["id"])
        guard id == expectedDeviceId else { throw GatewayHTTPError.dtoShape }

        let modem = try gatewayMap(device["modem"])
        let traffic = RemoteDeviceTraffic.parseOptional(from: device)

        return RemoteDeviceOverviewDetail(
            id: id,
            name: try gatewayString(device["name"]),
            deviceType: try gatewayString(device["device_type"]),
            healthy: try gatewayBool(device["healthy"]),
            physicalPresent: try gatewayBool(device["physical_present"]),
            controlOnline: try gatewayBool(device["control_online"]),
            networkConnected: try gatewayBool(device["network_connected"]),
            vowifiEnabled: try gatewayBool(device["vowifi_enabled"]),
            vowifiActive: try gatewayBool(device["vowifi_active"]),
            publicIP: try gatewayString(device["public_ip"]),
            interfaceName: try gatewayString(device["interface"]),
            localPhone: try gatewayString(device["local_phone"]),
            operatorName: try gatewayString(modem["operator"]),
            signalDbm: try gatewayInt(modem["signal_dbm"]),
            networkMode: try gatewayString(modem["network_mode"]),
            modemModel: try gatewayString(modem["model"]),
            runtime: try RemoteDeviceRuntimeOverview.parse(device["vowifi_runtime"]),
            traffic: traffic
        )
    }
}

struct RemoteDeviceTraffic: Equatable {
    let rx: String
    let tx: String
    let rate: String
    let rateTx: String
    let status: String?

    /// Align Dart: missing/empty traffic|traffic_raw|traffic_meta → unavailable.
    static func parseOptional(from device: [String: Any]) -> RemoteDeviceTraffic? {
        guard let formatted = device["traffic"] as? [String: Any],
              let raw = device["traffic_raw"] as? [String: Any],
              let meta = device["traffic_meta"] as? [String: Any],
              !formatted.isEmpty,
              !raw.isEmpty,
              !meta.isEmpty,
              let rx = formatted["rx"] as? String,
              let tx = formatted["tx"] as? String,
              let rate = formatted["rate"] as? String,
              let rateTx = formatted["rate_tx"] as? String else {
            return nil
        }
        return RemoteDeviceTraffic(
            rx: rx,
            tx: tx,
            rate: rate,
            rateTx: rateTx,
            status: meta["status"] as? String
        )
    }
}

// MARK: - JSON helpers

func gatewayMap(_ value: Any?) throws -> [String: Any] {
    guard let map = value as? [String: Any] else { throw GatewayHTTPError.dtoShape }
    return map
}

func gatewayList(_ value: Any?) throws -> [Any] {
    guard let list = value as? [Any] else { throw GatewayHTTPError.dtoShape }
    return list
}

func gatewayString(_ value: Any?) throws -> String {
    guard let text = value as? String else { throw GatewayHTTPError.dtoShape }
    return text
}

func gatewayNonEmptyString(_ value: Any?) throws -> String {
    let text = try gatewayString(value).trimmingCharacters(in: .whitespacesAndNewlines)
    if text.isEmpty { throw GatewayHTTPError.dtoShape }
    return text
}

func gatewayBool(_ value: Any?) throws -> Bool {
    guard let flag = value as? Bool else { throw GatewayHTTPError.dtoShape }
    return flag
}

func gatewayInt(_ value: Any?) throws -> Int {
    if let number = value as? Int { return number }
    if let number = value as? NSNumber { return number.intValue }
    throw GatewayHTTPError.dtoShape
}

func gatewayNonNegativeInt(_ value: Any?) throws -> Int {
    let number = try gatewayInt(value)
    if number < 0 { throw GatewayHTTPError.dtoShape }
    return number
}

func gatewayNonNegativeDouble(_ value: Any?) throws -> Double {
    let number: Double
    if let d = value as? Double {
        number = d
    } else if let i = value as? Int {
        number = Double(i)
    } else if let n = value as? NSNumber {
        number = n.doubleValue
    } else {
        throw GatewayHTTPError.dtoShape
    }
    if !number.isFinite || number < 0 { throw GatewayHTTPError.dtoShape }
    return number
}

func gatewayPercentage(_ value: Any?) throws -> Double {
    let number = try gatewayNonNegativeDouble(value)
    if number > 100 { throw GatewayHTTPError.dtoShape }
    return number
}
