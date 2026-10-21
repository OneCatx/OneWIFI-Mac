import Foundation

struct MultiStandbyCard: Decodable, Identifiable, Equatable {
    let cardID: String
    let reader: String
    let eid: String?
    let profileCount: Int
    let available: Bool
    let detail: String?
    var id: String { cardID }
    enum CodingKeys: String, CodingKey {
        case cardID = "card_id", reader, eid, profileCount = "profile_count", available, detail
    }
}

struct MultiStandbyProfile: Decodable, Identifiable, Equatable {
    let profileID: String
    let cardID: String
    let displayName: String?
    let phoneNumber: String?
    let iccid: String?
    let physicalEnabled: Bool?
    let desiredState: String?
    let authState: String?
    let tunnelState: String?
    let imsState: String?
    let smsReady: Bool?
    let health: String?
    let lastError: String?
    let sessionGeneration: UInt64?
    var id: String { profileID }
    var title: String {
        if let displayName, !displayName.isEmpty { return displayName }
        if let iccid, !iccid.isEmpty { return "Profile · " + iccid.suffix(4) }
        return "eSIM Profile"
    }
    var isReady: Bool { smsReady == true && ["keep_online", "running"].contains(desiredState ?? "") }
    var isStopped: Bool { desiredState == nil || desiredState == "stopped" }
    func label(masked: Bool) -> String {
        guard let phoneNumber, !phoneNumber.isEmpty else {
            if let displayName, !displayName.isEmpty, let iccid, !iccid.isEmpty {
                return title + " · ICCID 尾号 " + iccid.suffix(4)
            }
            return title
        }
        return title + " · " + (masked ? PrivacyMask.value(phoneNumber, enabled: true) : phoneNumber)
    }
    enum CodingKeys: String, CodingKey {
        case profileID = "profile_id", cardID = "card_id", displayName = "display_name"
        case phoneNumber = "phone_number", iccid, physicalEnabled = "physical_enabled"
        case desiredState = "desired_state", authState = "auth_state", tunnelState = "tunnel_state"
        case imsState = "ims_state", smsReady = "sms_ready", health, lastError = "last_error"
        case sessionGeneration = "session_generation"
    }
}

struct MultiStandbyMessage: Decodable, Identifiable, Equatable {
    let messageID: String
    let profileID: String
    let direction: String
    let peer: String
    let body: String
    let createdAtMS: Int64
    let state: String
    let kind: String?
    var id: String { profileID + ":" + messageID }
    var date: Date { Date(timeIntervalSince1970: Double(createdAtMS) / 1000) }
    var isIncoming: Bool { ["inbound", "in"].contains(direction) && (kind == nil || kind == "message") && state == "received" }
    var isOutgoing: Bool { ["outbound", "out"].contains(direction) }
    enum CodingKeys: String, CodingKey {
        case messageID = "message_id", profileID = "profile_id", direction, peer, body
        case createdAtMS = "created_at_ms", state, kind
    }
}

/// Rust cursors are opaque to the UI. Accept numeric fixtures as well as strings.
struct MultiStandbyCursor: Decodable, Equatable {
    let value: String
    init(_ value: String) { self.value = value }
    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if let value = try? container.decode(String.self) { self.value = value }
        else { value = String(try container.decode(UInt64.self)) }
    }
}

struct MultiStandbyCardsResponse: Decodable {
    let cards: [MultiStandbyCard]
    let bootID: String
    let detail: String?
    enum CodingKeys: String, CodingKey { case cards, bootID = "boot_id", detail }
}
struct MultiStandbyProfilesResponse: Decodable {
    let profiles: [MultiStandbyProfile]
    let bootID: String
    let revision: UInt64
    enum CodingKeys: String, CodingKey { case profiles, bootID = "boot_id", revision }
}
struct MultiStandbyMessagesResponse: Decodable {
    let messages: [MultiStandbyMessage]
    let nextCursor: MultiStandbyCursor?
    let revision: UInt64
    enum CodingKeys: String, CodingKey { case messages, nextCursor = "next_cursor", revision }
}
struct MultiStandbyChange: Decodable {
    let revision: UInt64
    let profileID: String
    let messageID: String
    let direction: String?
    let kind: String?
    let state: String?
    let deleted: Bool?
    enum CodingKeys: String, CodingKey {
        case revision, profileID = "profile_id", messageID = "message_id", direction, kind, state, deleted
    }
}
struct MultiStandbyChangesResponse: Decodable {
    let changes: [MultiStandbyChange]
    let nextCursor: MultiStandbyCursor?
    enum CodingKeys: String, CodingKey { case changes, nextCursor = "next_cursor" }
}
struct MultiStandbyControlRequest: Encodable {
    let cardID: String
    let profileIDs: [String]?
    let all: Bool?
    let clientRequestID: String
    enum CodingKeys: String, CodingKey {
        case cardID = "card_id", profileIDs = "profile_ids", all, clientRequestID = "client_request_id"
    }
}
struct MultiStandbyAccepted: Decodable {
    let operationID: String
    let accepted: Bool
    enum CodingKeys: String, CodingKey { case operationID = "operation_id", accepted }
}
struct MultiStandbyOperation: Decodable, Identifiable {
    let operationID: String
    let state: String
    let detail: String?
    var id: String { operationID }
    var isFinished: Bool { ["completed", "failed", "cancelled", "partial", "stopped"].contains(state) }
    enum CodingKeys: String, CodingKey { case operationID = "operation_id", state, detail }
}
struct MultiStandbySendRequest: Encodable, Equatable {
    let profileID: String
    let phone: String
    let body: String
    let clientRequestID: String
    enum CodingKeys: String, CodingKey {
        case profileID = "profile_id", phone, body, clientRequestID = "client_request_id"
    }
}
struct MultiStandbySent: Decodable {
    let messageID: String
    let state: String
    let revision: UInt64
    enum CodingKeys: String, CodingKey { case messageID = "message_id", state, revision }
}

@MainActor
protocol MultiStandbyAPI {
    func cards() async throws -> MultiStandbyCardsResponse
    func profiles(cardID: String) async throws -> MultiStandbyProfilesResponse
    func messages(profileID: String, cursor: String?) async throws -> MultiStandbyMessagesResponse
    func changes(cursor: String?) async throws -> MultiStandbyChangesResponse
    func control(start: Bool, request: MultiStandbyControlRequest) async throws -> MultiStandbyAccepted
    func operation(id: String) async throws -> MultiStandbyOperation
    func send(_ request: MultiStandbySendRequest) async throws -> MultiStandbySent
    func delete(messageID: String, profileID: String) async throws
}

@MainActor
struct MultiStandbyHTTPAPI: MultiStandbyAPI {
    private let client = APIClient()
    private func path(_ endpoint: String, _ query: [(String, String?)] = []) -> String {
        var components = URLComponents()
        components.queryItems = query.compactMap { name, value in
            guard let value, !value.isEmpty else { return nil }
            return URLQueryItem(name: name, value: value)
        }
        let suffix = components.percentEncodedQuery.map { "?" + $0 } ?? ""
        return "api/multistandby/" + endpoint + suffix
    }
    func cards() async throws -> MultiStandbyCardsResponse { try await client.get(path("cards")) }
    func profiles(cardID: String) async throws -> MultiStandbyProfilesResponse {
        try await client.get(path("profiles", [("card_id", cardID)]))
    }
    func messages(profileID: String, cursor: String?) async throws -> MultiStandbyMessagesResponse {
        try await client.get(path("messages", [("profile_id", profileID), ("cursor", cursor)]))
    }
    func changes(cursor: String?) async throws -> MultiStandbyChangesResponse {
        try await client.get(path("changes", [("cursor", cursor)]))
    }
    func control(start: Bool, request: MultiStandbyControlRequest) async throws -> MultiStandbyAccepted {
        try await client.send(path(start ? "start" : "stop"), body: request)
    }
    func operation(id: String) async throws -> MultiStandbyOperation {
        try await client.get(path("operations/" + id))
    }
    func send(_ request: MultiStandbySendRequest) async throws -> MultiStandbySent {
        try await client.send(path("messages/send"), body: request)
    }
    func delete(messageID: String, profileID: String) async throws {
        try await client.send(path("messages/" + messageID, [("profile_id", profileID)]), method: "DELETE")
    }
}
