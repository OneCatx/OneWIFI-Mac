import Foundation

// MARK: - AT

struct ATRequest: Encodable {
    let command: String
}

struct ATResult: Decodable {
    let response: String?
}

// MARK: - 网络

struct PDPContext: Decodable {
    let id: Int?
    let pdn: String?
    let apn: String?
}

struct MACInterface: Decodable {
    let name: String?
    let status: String?
    let ipv4: String?
    let kind: String?
}

struct DefaultRoute: Decodable {
    let interface: String?
    let gateway: String?
}

/// GET /api/network
struct NetworkDiagnostic: Decodable {
    let usbnetMode: String?
    let usbcfg: String?
    let pdpContexts: [PDPContext]?
    let activeContexts: [Int]?
    let pdpAddresses: [String]?
    let macInterfaces: [MACInterface]?
    let defaultRoute: DefaultRoute?
    let usbNetworkPresent: Bool?
    let errors: [String: String]?

    enum CodingKeys: String, CodingKey {
        case usbcfg
        case usbnetMode = "usbnet_mode"
        case pdpContexts = "pdp_contexts"
        case activeContexts = "active_contexts"
        case pdpAddresses = "pdp_addresses"
        case macInterfaces = "mac_interfaces"
        case defaultRoute = "default_route"
        case usbNetworkPresent = "usb_network_present"
        case errors
    }
}

/// GET /api/network/traffic
struct TrafficSnapshot: Decodable {
    let available: Bool
    let interface: String?
    let rxBytes: UInt64?
    let txBytes: UInt64?
    let sessionRX: UInt64?
    let sessionTX: UInt64?
    let sessionTotal: UInt64?
    let sampledAtMS: Int64?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case available, interface, error
        case rxBytes = "rx_bytes"
        case txBytes = "tx_bytes"
        case sessionRX = "session_rx_bytes"
        case sessionTX = "session_tx_bytes"
        case sessionTotal = "session_total_bytes"
        case sampledAtMS = "sampled_at_ms"
    }
}

/// POST /api/network/check-4g、check-proxy
struct CheckResult: Decodable {
    let ok: Bool
    let summary: String?
    let detail: String?
}

/// POST /api/network/usbnet
struct USBNetRequest: Encodable {
    let mode: Int
}

struct USBNetResult: Decodable {
    let mode: Int?
    let response: String?
    let needsReboot: Bool?

    enum CodingKeys: String, CodingKey {
        case mode, response
        case needsReboot = "needs_reboot"
    }
}

/// POST /api/network/reboot-module（202）
struct RebootResult: Decodable {
    let accepted: Bool
    let response: String?
}

/// GET/POST /api/module/flight-mode. `enabled` is only authoritative when the
/// backend has verified the module's RF mode, signal query, and registration.
struct ModuleFlightModeStatus: Decodable, Equatable {
    let available: Bool
    let enabled: Bool
    let verified: Bool
    let userConfigured: Bool
    let cfunMode: Int
    let owner: String
    let canToggle: Bool
    let registrationKnown: Bool
    let cellularRegistered: Bool?
    let ceregState: Int?
    let cregState: Int?
    let signalKnown: Bool
    let signalDetected: Bool?
    let signalLevel: Int?
    let summary: String
    let detail: String?

    enum CodingKeys: String, CodingKey {
        case available, enabled, verified, owner, summary, detail
        case userConfigured = "user_configured"
        case cfunMode = "cfun_mode"
        case canToggle = "can_toggle"
        case registrationKnown = "registration_known"
        case cellularRegistered = "cellular_registered"
        case ceregState = "cereg_state"
        case cregState = "creg_state"
        case signalKnown = "signal_known"
        case signalDetected = "signal_detected"
        case signalLevel = "signal_level"
    }
}

struct ModuleFlightModeRequest: Encodable {
    let enabled: Bool
    let userInitiated: Bool

    enum CodingKeys: String, CodingKey {
        case enabled
        case userInitiated = "user_initiated"
    }
}

struct NetworkService: Decodable, Identifiable {
    let name: String
    let device: String?
    let port: String?
    let usb: Bool?
    let module: Bool?

    var id: String { name }
}

/// PUT /api/network/services-order
struct ServicesOrderRequest: Encodable {
    let services: [String]
}
