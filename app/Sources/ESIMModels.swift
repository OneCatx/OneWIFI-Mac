import Foundation

// MARK: - eSIM

struct ESIMProfileContext: Identifiable {
    let eid: String
    let aidHex: String
    let profile: ProfileItem

    var id: String { "\(aidHex):\(profile.iccid)" }
}

struct ESIMBanner: Identifiable {
    enum Tone {
        case info
        case success
        case warning
        case error
    }

    let id = UUID()
    let tone: Tone
    let message: String
    let detail: String?
}

struct EUICCInfo: Decodable {
    let aid: String?
    let eid: String?
    let spec: String?
    let freeNvram: String?
    let freeNvramBytes: Int32?
    let firmware: String?
    let manufacturer: String?
    let sasAccreditationNumber: String?
    let specGuess: String?
    let specConfidence: String?
    let infoSource: String?
    let infoVersion: String?
    let infoError: String?
    let defaultSMDPAddress: String?
    let rootDSAddress: String?

    enum CodingKeys: String, CodingKey {
        case aid, eid, spec, firmware, manufacturer
        case freeNvram = "free_nvram"
        case freeNvramBytes = "free_nvram_bytes"
        case sasAccreditationNumber = "sas_accreditation_number"
        case specGuess = "spec_guess"
        case specConfidence = "spec_confidence"
        case infoSource = "info_source"
        case infoVersion = "info_version"
        case infoError = "info_error"
        case defaultSMDPAddress = "default_smdp_address"
        case rootDSAddress = "root_ds_address"
    }
}

struct CardIdentity: Decodable {
    let brand: String?
    let model: String?
    let hardwareRevision: String?
    let source: String?
    let confidence: String?
    let ruleID: String?
    let sourceURL: String?
    let sourceVersion: String?
    let evidence: [CardIdentityEvidence]?

    enum CodingKeys: String, CodingKey {
        case brand, model, source, confidence, evidence
        case hardwareRevision = "hardware_revision"
        case ruleID = "rule_id"
        case sourceURL = "source_url"
        case sourceVersion = "source_version"
    }
}

struct CardIdentityEvidence: Decodable {
    let kind: String
    let name: String
    let url: String
    let version: String
}

struct ChipInfo: Decodable {
    let eids: [EUICCInfo]?
    let skuName: String?
    let serialNumber: String?
    let firmware: String?
    let identity: CardIdentity?

    enum CodingKeys: String, CodingKey {
        case eids
        case skuName = "sku_name"
        case serialNumber = "serial_number"
        case firmware, identity
    }
}

struct ProfileItem: Decodable, Identifiable {
    let iccid: String
    let name: String?
    let serviceProviderName: String?
    let state: Int?
    let stateText: String?
    let classText: String?

    var id: String { iccid }
    var isEnabled: Bool { state == 1 }
    var displayName: String {
        if let name = name?.trimmingCharacters(in: .whitespacesAndNewlines), !name.isEmpty {
            return name
        }
        if let provider = serviceProviderName?.trimmingCharacters(in: .whitespacesAndNewlines), !provider.isEmpty {
            return provider
        }
        return iccid.isEmpty ? "Profile" : "Profile · …\(iccid.suffix(6))"
    }

    /// 基于 ICCID 推断国旗 emoji
    var flagEmoji: String? { iccidFlagEmoji(iccid) }

    enum CodingKeys: String, CodingKey {
        case iccid, name, state
        case serviceProviderName = "service_provider_name"
        case stateText = "state_text"
        case classText = "class_text"
    }
}

struct EUICCProfiles: Decodable {
    let eid: String?
    let aidHex: String?
    let readState: String?
    let readError: String?
    let profiles: [ProfileItem]?

    enum CodingKeys: String, CodingKey {
        case eid
        case aidHex = "aid_hex"
        case readState = "read_state"
        case readError = "read_error"
        case profiles
    }
}

struct ESIMCapabilities: Decodable {
    let canRefresh: Bool
    let canDownload: Bool
    let canSwitch: Bool
    let canRename: Bool
    let canDelete: Bool
    let canProbePhonebook: Bool
    let vendorSerialAvailable: Bool

    enum CodingKeys: String, CodingKey {
        case canRefresh = "can_refresh"
        case canDownload = "can_download"
        case canSwitch = "can_switch"
        case canRename = "can_rename"
        case canDelete = "can_delete"
        case canProbePhonebook = "can_probe_phonebook"
        case vendorSerialAvailable = "vendor_serial_available"
    }
}

/// GET /api/esim
struct ESIMOverview: Decodable {
    let cardType: String?
    let message: String?
    let chipInfo: ChipInfo?
    let profiles: [EUICCProfiles]?
    let capabilities: ESIMCapabilities?
    let updatedAt: Date?
    let operation: ESIMOperationSnapshot?

    enum CodingKeys: String, CodingKey {
        case message, capabilities, operation
        case cardType = "card_type"
        case chipInfo = "chip_info"
        case profiles
        case updatedAt = "updated_at"
    }
}

/// GET /api/esim/notes
struct ProfileNote: Decodable {
    let label: String?
    let phone: String?
    let tags: String?
}

func usableSIMPhoneNumber(_ candidate: String?) -> String? {
    guard let value = candidate?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else { return nil }
    let normalized = value.lowercased()
    if ["-", "unknown", "unknown number", "n/a", "号码未知", "未知号码"].contains(normalized) { return nil }
    let digits = value.filter(\.isNumber)
    if digits.isEmpty || digits.allSatisfy({ $0 == "0" }) { return nil }
    return value
}

struct NotesResponse: Decodable {
    let notes: [String: ProfileNote]?
}

/// PUT /api/esim/notes
struct SaveNoteRequest: Encodable {
    let iccid: String
    let label: String
    let phone: String
    let tags: String
}

struct MessageResponse: Decodable {
    let message: String?
}

/// GET /api/esim/module-notes
struct ModuleProfileNote: Decodable {
    let index: Int?
    let iccid: String?
    let label: String?
    let phone: String?
    let tags: String?
}

struct ModuleNotesResponse: Decodable {
    let notes: [String: ModuleProfileNote]?
    let used: Int?
    let total: Int?
}

/// PUT /api/esim/module-notes
struct SaveModuleNoteRequest: Encodable {
    let iccid: String
    let label: String
    let phone: String
    let tags: String
}

/// POST /api/esim/phonebook/probe
struct PhonebookProbeResult: Decodable {
    let storage: String?
    let storageSupported: Bool?
    let storageSelected: Bool?
    let readSupported: Bool?
    let writeSupported: Bool?
    let storageStatus: String?
    let responses: [String: String]?

    enum CodingKeys: String, CodingKey {
        case storage, responses
        case storageSupported = "storage_supported"
        case storageSelected = "storage_selected"
        case readSupported = "read_supported"
        case writeSupported = "write_supported"
        case storageStatus = "storage_status"
    }
}

/// POST /api/esim/switch
struct ESIMSwitchRequest: Encodable {
    let iccid: String
    let aid: String?
}

struct ESIMSwitchResult: Decodable {
    let switchAccepted: Bool?
    let phase: String?
    let targetIccid: String?
    let recoveryPending: Bool?
    let moduleRebootRequested: Bool?
    let moduleRebootResponse: String?
    let moduleRebootWarning: String?
    let reconnectWaitSeconds: Int?
	let operation: ESIMOperationSnapshot?
	let readerInfo: ReaderInfoResponse?

    enum CodingKeys: String, CodingKey {
		case phase, operation
		case readerInfo = "reader_info"
        case switchAccepted = "switch_accepted"
        case targetIccid = "target_iccid"
        case recoveryPending = "recovery_pending"
        case moduleRebootRequested = "module_reboot_requested"
        case moduleRebootResponse = "module_reboot_response"
        case moduleRebootWarning = "module_reboot_warning"
        case reconnectWaitSeconds = "reconnect_wait_seconds"
    }
}

/// PATCH /api/esim/profile
struct ESIMRenameRequest: Encodable {
    let iccid: String
    let aid: String?
    let name: String
}

/// DELETE /api/esim/profile
struct ESIMDeleteRequest: Encodable {
    let iccid: String
    let aid: String?
}

/// 删除/下载 Profile 的结果。
struct SpaceDelta: Decodable {
    let direction: String?
    let bytes: Int?
}

struct ESIMProfileResult: Decodable {
    let warning: String?
    let warningCode: String?
    let spaceDelta: SpaceDelta?

    enum CodingKeys: String, CodingKey {
        case warning
        case warningCode = "warning_code"
        case spaceDelta = "space_delta"
    }
}

struct ESIMOperationResult: Decodable {
    let warning: String?
    let warningCode: String?
    let spaceDelta: SpaceDelta?

    enum CodingKeys: String, CodingKey {
        case warning
        case warningCode = "warning_code"
        case spaceDelta = "space_delta"
    }
}

struct ESIMOperationSnapshot: Decodable, Identifiable {
    let id: String
    let kind: String
    let state: String
    let step: String?
    let message: String?
    let progress: Int
    let estimatedProfileSizeBytes: Int64?
    let targetICCID: String?
    let errorCode: String?
    let error: String?
    let diagnostic: String?
    let recoverable: Bool?
    let refreshAfterSeconds: Int?
    let result: ESIMOperationResult?
    let startedAt: Date
    let updatedAt: Date
    let finishedAt: Date?

    var isActive: Bool { state == "queued" || state == "running" || state == "awaiting_confirmation" }
    var awaitsDownloadConfirmation: Bool { state == "awaiting_confirmation" }
    var isCancelled: Bool { state == "cancelled" }
    var isSuccessful: Bool { state == "succeeded" }
    var hasWarning: Bool { state == "warning" }
    var isFailed: Bool { state == "failed" }

    enum CodingKeys: String, CodingKey {
        case id, kind, state, step, message, progress, error, diagnostic, recoverable, result
        case estimatedProfileSizeBytes = "estimated_profile_size_bytes"
        case targetICCID = "target_iccid"
        case errorCode = "error_code"
        case refreshAfterSeconds = "refresh_after_seconds"
        case startedAt = "started_at"
        case updatedAt = "updated_at"
        case finishedAt = "finished_at"
    }
}

struct ESIMOperationResponse: Decodable {
    let operation: ESIMOperationSnapshot?
}

/// Pending GSMA SGP.22 notification stored by the eUICC. This is a protocol
/// notification for the operator/SM-DP+, not a macOS user notification.
struct ESIMNotificationItem: Decodable, Identifiable, Equatable {
    let sequenceNumber: Int64
    let event: String
    let iccid: String?
    let address: String?
    let aidHex: String?
    let canRetry: Bool

    var id: String { "\(aidHex ?? ""):\(sequenceNumber)" }

    enum CodingKeys: String, CodingKey {
        case event, iccid, address
        case sequenceNumber = "sequence_number"
        case aidHex = "aid_hex"
        case canRetry = "can_retry"
    }
}

struct ESIMNotificationListResponse: Decodable {
    let notifications: [ESIMNotificationItem]
}

struct ESIMNotificationRetryRequest: Encodable {
    let sequenceNumber: Int64
    let aid: String?

    enum CodingKeys: String, CodingKey {
        case sequenceNumber = "sequence_number"
        case aid
    }
}

/// POST /api/esim/download
struct ESIMDownloadRequest: Encodable {
    let smdp: String
    let matchingID: String?
    let confirmationCode: String?
    let aid: String?
    let imei: String?

    enum CodingKeys: String, CodingKey {
        case smdp, aid, imei
        case matchingID = "matching_id"
        case confirmationCode = "confirmation_code"
    }
}

struct ESIMDownloadConfirmationRequest: Encodable {
    let operationID: String
    let confirm: Bool

    enum CodingKeys: String, CodingKey {
        case confirm
        case operationID = "operation_id"
    }
}

/// GET /api/vowifi
struct VoWiFiStatus: Decodable, Equatable {
    // 操作状态
    let state: String
    let summary: String
    let detail: String?

    // 飞行模式
    let airplaneMode: Bool
    let cfunMode: Int

    // USB 网络模式
    let usbnetMode: Int
    let usbnetReady: Bool

    // AT 层面
    let imsWiFiCallingEnabled: Bool
    let imsCallModeSet: Bool
    let wifiCallingMode: Int

    // ePDG 配置
    let epdgAddress: String?
    let epdgIPAddress: String?
    let epdgAutoDetect: Bool
    let epdgFqdn: String?

    // 运营商信息
    let operatorMCC: String?
    let operatorMNC: String?
    let operatorName: String?
    /// ICCID owning this module VoWiFi session; reader statuses omit it.
    let simICCID: String?

    // IMS 注册状态
    let imsRegistered: Bool

    // VoWiFi 综合激活状态
    let vowifiActive: Bool

    // Host-routed startup pipeline. Optional fields preserve compatibility
    // with an older local daemon while the app/backend are being upgraded.
    let progressStep: Int?
    let progressDesc: String?
    let smsReady: Bool?
    let callReady: Bool?

    // QMI 辅助（可能不可用）
    let qmiEnabled: Bool?
    let qmiWiFiOnly: Bool?
    let qmiWiFiFirst: Bool?
    let qmiInUse: Bool?
    let qmiWiFiRegistered: Bool?
    let qmiError: String?

    enum CodingKeys: String, CodingKey {
        case state
        case summary
        case detail
        case airplaneMode = "airplane_mode"
        case cfunMode = "cfun_mode"
        case usbnetMode = "usbnet_mode"
        case usbnetReady = "usbnet_ready"
        case imsWiFiCallingEnabled = "ims_wifi_calling_enabled"
        case imsCallModeSet = "ims_call_mode_set"
        case wifiCallingMode = "wifi_calling_mode"
        case epdgAddress = "epdg_address"
        case epdgIPAddress = "epdg_ip_address"
        case epdgAutoDetect = "epdg_auto_detect"
        case epdgFqdn = "epdg_fqdn"
        case operatorMCC = "operator_mcc"
        case operatorMNC = "operator_mnc"
        case operatorName = "operator_name"
        case simICCID = "sim_iccid"
        case imsRegistered = "ims_registered"
        case vowifiActive = "vowifi_active"
        case progressStep = "progress_step"
        case progressDesc = "progress_desc"
        case smsReady = "sms_ready"
        case callReady = "call_ready"
        case qmiEnabled = "qmi_enabled"
        case qmiWiFiOnly = "qmi_wifi_only"
        case qmiWiFiFirst = "qmi_wifi_first"
        case qmiInUse = "qmi_in_use"
        case qmiWiFiRegistered = "qmi_wifi_registered"
        case qmiError = "qmi_error"
    }
}

/// POST /api/vowifi
struct VoWiFiRequest: Encodable {
    let enable: Bool?
    let wifiCallingMode: Int?

    enum CodingKeys: String, CodingKey {
        case enable
        case wifiCallingMode = "wifi_calling_mode"
    }
}

/// GET /api/vowifi/result
struct VoWiFiResult: Decodable, Equatable {
    let done: Bool
    let error: String?
    let status: VoWiFiStatus?
}

/// GET /api/esim/health
struct ESIMHealth: Decodable {
    let ok: Bool?
    let cardType: String?
    let state: String?
    let message: String?
    let activeProfile: ProfileItem?
    let moduleIccid: String?
    let imsi: String?
    let operatorName: String?
    let registration: String?
    let registered: Bool?
    let signalDbm: Int?
    let networkMode: String?
    let profileMatchesModule: Bool?
    let operation: ESIMOperationSnapshot?

    enum CodingKeys: String, CodingKey {
        case ok, state, message, registration, registered, imsi, operation
        case cardType = "card_type"
        case activeProfile = "active_profile"
        case moduleIccid = "module_iccid"
        case operatorName = "operator"
        case signalDbm = "signal_dbm"
        case networkMode = "network_mode"
        case profileMatchesModule = "profile_matches_module"
    }
}
