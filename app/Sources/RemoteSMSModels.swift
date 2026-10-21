import Foundation

/// Remote SMS DTOs aligned with WEB `handleSMSContacts` / `storedSMSResponse` / `handleSMSSend`.
/// Field set follows WEB Go + frontend `onewifiGateway.sms.js`. No invented keys.

struct RemoteSMSContact: Identifiable, Equatable, Hashable {
    let deviceId: String
    let deviceName: String
    let modemImei: String
    let iccid: String
    let imsi: String
    let localPhone: String
    let peer: String
    let displayName: String
    let lastMessage: String
    let lastTimestamp: Date?
    let direction: String
    let lastSmsId: Int64
    let unreadCount: Int
    let messageCount: Int

    var id: String {
        [deviceId, iccid, imsi, peer].joined(separator: "|")
    }

    var title: String {
        let name = displayName.trimmingCharacters(in: .whitespacesAndNewlines)
        if !name.isEmpty { return name }
        let peerText = peer.trimmingCharacters(in: .whitespacesAndNewlines)
        return peerText.isEmpty ? "未知联系人" : peerText
    }

    static func parse(_ value: Any) throws -> RemoteSMSContact {
        let data = try gatewayMap(value)
        let lastMessage = (try? gatewayString(data["last_message"])) ?? ""
        let lastContent = (try? gatewayString(data["last_content"])) ?? ""
        let resolvedLast = lastMessage.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            ? lastContent
            : lastMessage
        return RemoteSMSContact(
            deviceId: try gatewayString(data["device_id"]),
            deviceName: try gatewayString(data["device_name"]),
            modemImei: try gatewayString(data["modem_imei"]),
            iccid: try gatewayString(data["iccid"]),
            imsi: try gatewayString(data["imsi"]),
            localPhone: try gatewayString(data["local_phone"]),
            peer: try gatewayString(data["peer"]),
            displayName: try gatewayString(data["display_name"]),
            lastMessage: resolvedLast,
            lastTimestamp: gatewayOptionalDate(data["last_timestamp"]),
            direction: try gatewayString(data["direction"]),
            lastSmsId: try gatewayInt64(data["last_sms_id"]),
            unreadCount: try gatewayNonNegativeInt(data["unread_count"]),
            messageCount: try gatewayNonNegativeInt(data["message_count"])
        )
    }

    static func parseList(_ value: Any) throws -> [RemoteSMSContact] {
        guard let list = value as? [Any] else { throw GatewayHTTPError.dtoShape }
        return try list.map { try parse($0) }
    }
}

struct RemoteSMSMessage: Identifiable, Equatable {
    let id: Int64
    let messageId: String
    let deviceId: String
    let modemImei: String
    let iccid: String
    let imsi: String
    let localPhone: String
    let peer: String
    let direction: String
    let body: String
    let timestamp: Date
    let status: String
    let source: String
    let partsTotal: Int
    let deliveryState: String

    var isOutgoing: Bool {
        direction == "outbound" || direction == "out" || direction == "sent"
    }

    static func parse(_ value: Any) throws -> RemoteSMSMessage {
        let data = try gatewayMap(value)
        let body = (try? gatewayString(data["body"])) ?? ""
        let content = (try? gatewayString(data["content"])) ?? ""
        let resolvedBody = body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? content : body
        return RemoteSMSMessage(
            id: try gatewayInt64(data["id"]),
            messageId: try gatewayString(data["message_id"]),
            deviceId: try gatewayString(data["device_id"]),
            modemImei: try gatewayString(data["modem_imei"]),
            iccid: try gatewayString(data["iccid"]),
            imsi: try gatewayString(data["imsi"]),
            localPhone: try gatewayString(data["local_phone"]),
            peer: try gatewayString(data["peer"]),
            direction: try gatewayString(data["direction"]),
            body: resolvedBody,
            timestamp: try gatewayRequiredDate(data["timestamp"]),
            status: try gatewayString(data["status"]),
            source: try gatewayString(data["source"]),
            partsTotal: try gatewayNonNegativeInt(data["parts_total"]),
            deliveryState: try gatewayString(data["delivery_state"])
        )
    }

    static func parseList(_ value: Any) throws -> [RemoteSMSMessage] {
        guard let list = value as? [Any] else { throw GatewayHTTPError.dtoShape }
        return try list.map { try parse($0) }
    }
}

struct RemoteSMSSendResult: Equatable {
    let messageId: String
    let id: Int64
    let partsTotal: Int
    let partsAttempted: Int
    let partsAccepted: Int
    let allPartsAccepted: Bool
    let deliveryState: String
    let submissionState: String
    let transport: String
    let submissionAccepted: Bool
    let deliveryConfirmed: Bool
    let outcome: String
    let warning: String?

    static func parse(_ value: Any) throws -> RemoteSMSSendResult {
        let data = try gatewayMap(value)
        return RemoteSMSSendResult(
            messageId: try gatewayString(data["message_id"]),
            id: try gatewayInt64(data["id"]),
            partsTotal: try gatewayNonNegativeInt(data["parts_total"]),
            partsAttempted: try gatewayNonNegativeInt(data["parts_attempted"]),
            partsAccepted: try gatewayNonNegativeInt(data["parts_accepted"]),
            allPartsAccepted: try gatewayBool(data["all_parts_accepted"]),
            deliveryState: try gatewayString(data["delivery_state"]),
            submissionState: try gatewayString(data["submission_state"]),
            transport: try gatewayString(data["transport"]),
            submissionAccepted: try gatewayBool(data["submission_accepted"]),
            deliveryConfirmed: try gatewayBool(data["delivery_confirmed"]),
            outcome: try gatewayString(data["outcome"]),
            warning: data["warning"] as? String
        )
    }
}

struct RemoteSMSThreadKey: Hashable, Equatable {
    let peer: String
    let deviceId: String
    let iccid: String
    let imsi: String

    init(peer: String, deviceId: String = "", iccid: String = "", imsi: String = "") {
        self.peer = peer
        self.deviceId = deviceId
        self.iccid = iccid
        self.imsi = imsi
    }

    init(contact: RemoteSMSContact) {
        self.init(
            peer: contact.peer,
            deviceId: contact.deviceId,
            iccid: contact.iccid,
            imsi: contact.imsi
        )
    }
}

enum RemoteSMSPathBuilder {
    static func contacts(deviceId: String?) -> String {
        var items: [URLQueryItem] = []
        if let raw = deviceId?.trimmingCharacters(in: .whitespacesAndNewlines),
           !raw.isEmpty,
           raw.lowercased() != "all" {
            items.append(URLQueryItem(name: "device_id", value: raw))
        }
        return appendQuery(GatewayAPI.smsContacts, items)
    }

    static func thread(_ key: RemoteSMSThreadKey, beforeId: Int64? = nil, limit: Int = 100) -> String {
        var items: [URLQueryItem] = [
            URLQueryItem(name: "peer", value: key.peer)
        ]
        let deviceId = key.deviceId.trimmingCharacters(in: .whitespacesAndNewlines)
        if !deviceId.isEmpty, deviceId.lowercased() != "all" {
            items.append(URLQueryItem(name: "device_id", value: deviceId))
        }
        if !key.iccid.isEmpty {
            items.append(URLQueryItem(name: "iccid", value: key.iccid))
        }
        if !key.imsi.isEmpty {
            items.append(URLQueryItem(name: "imsi", value: key.imsi))
        }
        if let beforeId, beforeId > 0 {
            items.append(URLQueryItem(name: "before_id", value: String(beforeId)))
        }
        if limit != 100 {
            items.append(URLQueryItem(name: "limit", value: String(limit)))
        }
        return appendQuery(GatewayAPI.smsThread, items)
    }

    private static func appendQuery(_ path: String, _ items: [URLQueryItem]) -> String {
        guard !items.isEmpty else { return path }
        var components = URLComponents()
        components.queryItems = items
        guard let query = components.percentEncodedQuery, !query.isEmpty else { return path }
        return path + "?" + query
    }
}

// MARK: - JSON helpers (SMS-specific)

func gatewayInt64(_ value: Any?) throws -> Int64 {
    if let number = value as? Int64 { return number }
    if let number = value as? Int { return Int64(number) }
    if let number = value as? NSNumber { return number.int64Value }
    throw GatewayHTTPError.dtoShape
}

func gatewayOptionalDate(_ value: Any?) -> Date? {
    guard let value else { return nil }
    return try? gatewayRequiredDate(value)
}

func gatewayRequiredDate(_ value: Any?) throws -> Date {
    if let date = value as? Date { return date }
    if let text = value as? String {
        if let date = gatewayISO8601Date(text) { return date }
    }
    if let number = value as? NSNumber {
        let seconds = number.doubleValue
        if seconds > 1_000_000_000_000 {
            return Date(timeIntervalSince1970: seconds / 1000)
        }
        if seconds > 0 {
            return Date(timeIntervalSince1970: seconds)
        }
    }
    throw GatewayHTTPError.dtoShape
}

private func gatewayISO8601Date(_ text: String) -> Date? {
    let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !trimmed.isEmpty else { return nil }
    let withFraction = ISO8601DateFormatter()
    withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = withFraction.date(from: trimmed) { return date }
    let plain = ISO8601DateFormatter()
    plain.formatOptions = [.withInternetDateTime]
    return plain.date(from: trimmed)
}
