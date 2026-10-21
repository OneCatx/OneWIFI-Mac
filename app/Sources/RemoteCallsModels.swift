import Foundation

/// Remote Calls control-plane DTOs aligned with WEB `call_api.go` + `vowifi.Call`
/// and `gateway_api.dart` deviceCalls*. No invented keys. No WebRTC/media fields as engines.

struct RemoteCall: Identifiable, Equatable {
    let id: String
    let number: String
    let direction: String
    let state: String
    let startedAt: Date
    let sipCode: Int?
    let reason: String?
    let mediaReady: Bool
    let codec: String?
    let endedAt: Date?
    let hold: String?
    let mediaStalled: Bool
    let ringback: String?

    var isIncoming: Bool {
        direction == "inbound" || direction == "in" || direction == "incoming"
    }

    var isActiveLike: Bool {
        endedAt == nil && state != "ended" && state != "failed" && state != "cancelled"
    }

    var canAnswer: Bool {
        isIncoming && (state == "ringing" || state == "incoming" || state == "alerting")
    }

    var canHangup: Bool {
        isActiveLike
    }

    var canSendDTMF: Bool {
        state == "active" && endedAt == nil
    }

    static func parse(_ value: Any) throws -> RemoteCall {
        let data = try gatewayMap(value)
        return RemoteCall(
            id: try gatewayNonEmptyString(data["id"]),
            number: try gatewayString(data["number"]),
            direction: try gatewayString(data["direction"]),
            state: try gatewayString(data["state"]),
            startedAt: try gatewayRequiredDate(data["started_at"]),
            sipCode: try? gatewayInt(data["sip_code"]),
            reason: data["reason"] as? String,
            mediaReady: (try? gatewayBool(data["media_ready"])) ?? false,
            codec: data["codec"] as? String,
            endedAt: gatewayOptionalDate(data["ended_at"]),
            hold: data["hold"] as? String,
            mediaStalled: (try? gatewayBool(data["media_stalled"])) ?? false,
            ringback: data["ringback"] as? String
        )
    }

    static func parseListEnvelope(_ value: Any) throws -> [RemoteCall] {
        let data = try gatewayMap(value)
        let list = try gatewayList(data["calls"])
        return try list.map { try parse($0) }
    }
}

struct RemoteCallHistoryItem: Identifiable, Equatable {
    let id: Int64
    let callId: String?
    let numberTail: String
    let direction: String
    let path: String
    let state: String
    let startedAt: Date
    let endedAt: Date
    let durationSeconds: Int

    var isIncoming: Bool {
        direction == "inbound" || direction == "in" || direction == "incoming"
    }

    static func parse(_ value: Any) throws -> RemoteCallHistoryItem {
        let data = try gatewayMap(value)
        let callIdRaw = (data["call_id"] as? String)?.trimmingCharacters(in: .whitespacesAndNewlines)
        return RemoteCallHistoryItem(
            id: try gatewayInt64(data["id"]),
            callId: (callIdRaw?.isEmpty == false) ? callIdRaw : nil,
            numberTail: try gatewayString(data["number_tail"]),
            direction: try gatewayString(data["direction"]),
            path: try gatewayString(data["path"]),
            state: try gatewayString(data["state"]),
            startedAt: try gatewayRequiredDate(data["started_at"]),
            endedAt: try gatewayRequiredDate(data["ended_at"]),
            durationSeconds: try gatewayNonNegativeInt(data["duration_seconds"])
        )
    }

    static func parseListEnvelope(_ value: Any) throws -> [RemoteCallHistoryItem] {
        let data = try gatewayMap(value)
        let list = try gatewayList(data["calls"])
        return try list.map { try parse($0) }
    }
}

struct RemoteCallHangupResult: Equatable {
    let ended: Bool
    let id: String

    static func parse(_ value: Any) throws -> RemoteCallHangupResult {
        let data = try gatewayMap(value)
        return RemoteCallHangupResult(
            ended: try gatewayBool(data["ended"]),
            id: try gatewayString(data["id"])
        )
    }
}

struct RemoteCallDTMFResult: Equatable {
    let sent: Bool

    static func parse(_ value: Any) throws -> RemoteCallDTMFResult {
        let data = try gatewayMap(value)
        return RemoteCallDTMFResult(sent: try gatewayBool(data["sent"]))
    }
}
