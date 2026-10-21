import Foundation
import Darwin

private func require(_ condition: @autoclosure () -> Bool, _ message: String) {
    precondition(condition(), message)
}
private func decode<T: Decodable>(_ type: T.Type, _ text: String) -> T {
    try! JSONDecoder().decode(type, from: Data(text.utf8))
}

@MainActor
private final class FakeAPI: MultiStandbyAPI {
    var profileResponse = decode(MultiStandbyProfilesResponse.self, """
    {"profiles":[
      {"profile_id":"a","card_id":"card","display_name":"A","desired_state":"keep_online","sms_ready":true},
      {"profile_id":"b","card_id":"card","display_name":"B","desired_state":"keep_online","sms_ready":false}
    ],"boot_id":"boot-1","revision":1}
    """)
    var inbox: [String: [MultiStandbyMessage]] = [:]
    var changeEvents: [MultiStandbyChange] = []
    var sent: [MultiStandbySendRequest] = []
    var deleted: [(String, String)] = []
    var controls: [MultiStandbyControlRequest] = []
    var inventoryRequests = 0
    var archiveCount = 0
    var archiveReads = 0
    var failSending = false
    var heldProfile: String?
    var waiters: [CheckedContinuation<MultiStandbyMessagesResponse, Error>] = []
    var holdProfiles = false
    var profileWaiters: [CheckedContinuation<MultiStandbyProfilesResponse, Error>] = []
    func cards() async throws -> MultiStandbyCardsResponse {
        inventoryRequests += 1
        return decode(MultiStandbyCardsResponse.self, """
        {"cards":[{"card_id":"card","reader":"Fixture","profile_count":2,"available":true}],"boot_id":"boot-1"}
        """)
    }
    func profiles(cardID: String) async throws -> MultiStandbyProfilesResponse {
        if holdProfiles { return try await withCheckedThrowingContinuation { profileWaiters.append($0) } }
        return profileResponse
    }
    func messages(profileID: String, cursor: String?) async throws -> MultiStandbyMessagesResponse {
        if archiveCount > 0 {
            archiveReads += 1
            let offset = min(Int(cursor ?? "0") ?? 0, archiveCount)
            let end = min(offset + 100, archiveCount)
            let page = (offset..<end).map { value in
                MultiStandbyMessage(messageID: "archive-\(value + 1)", profileID: profileID,
                    direction: "inbound", peer: "123", body: "history", createdAtMS: Int64(value + 1),
                    state: "received", kind: "message")
            }
            return MultiStandbyMessagesResponse(messages: page,
                nextCursor: MultiStandbyCursor(String(end)), revision: UInt64(archiveCount))
        }
        if heldProfile == profileID {
            return try await withCheckedThrowingContinuation { waiters.append($0) }
        }
        return MultiStandbyMessagesResponse(messages: inbox[profileID] ?? [], nextCursor: nil, revision: 10)
    }
    func changes(cursor: String?) async throws -> MultiStandbyChangesResponse {
        let revision = UInt64(cursor ?? "0") ?? 0
        let changes = changeEvents.filter { $0.revision > revision }
        return MultiStandbyChangesResponse(changes: changes,
            nextCursor: MultiStandbyCursor(String(changeEvents.last?.revision ?? revision)))
    }
    func control(start: Bool, request: MultiStandbyControlRequest) async throws -> MultiStandbyAccepted {
        controls.append(request)
        return MultiStandbyAccepted(operationID: "op", accepted: true)
    }
    func operation(id: String) async throws -> MultiStandbyOperation {
        MultiStandbyOperation(operationID: id, state: "completed", detail: "done")
    }
    func send(_ request: MultiStandbySendRequest) async throws -> MultiStandbySent {
        sent.append(request)
        if failSending { throw URLError(.networkConnectionLost) }
        return MultiStandbySent(messageID: "sent", state: "queued", revision: 11)
    }
    func delete(messageID: String, profileID: String) async throws { deleted.append((profileID, messageID)) }
}

@main
private enum MultiStandbyTests {
    @MainActor
    static func main() async throws {
        setbuf(stdout, nil)
        let suite = "Switch.MultiStandby.Tests." + UUID().uuidString
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let api = FakeAPI()
        var notifications: [String] = []
        let store = MultiStandbyStore(api: api, defaults: defaults, notificationSink: { message, masked in
            require(masked, "new feature must default to privacy masking")
            notifications.append(message.id)
        })
        store.setServiceRunning(true, schedulePolling: false)
        await store.refreshNow()
        require(api.inventoryRequests == 0, "background store must not perform physical card inventory")
        store.viewingPage = true
        await store.refreshNow()
        require(api.inventoryRequests == 1, "visible page must load its explicit card inventory")
        store.viewingPage = false
        store.selectProfile("b")
        await store.refreshMessages()
        require(!store.canSend, "unready selected profile must remain unready")
        _ = await store.send(phone: "123", body: "hello")
        require(api.sent.isEmpty && store.selectedProfileID == "b", "must never fall back to profile A")
        store.selectProfile("a")
        await store.refreshMessages()
        api.failSending = true
        let failed = await store.send(phone: "123", body: "hello")
        require(!failed, "ambiguous send must not look accepted")
        let first = api.sent.last!
        api.failSending = false
        let accepted = await store.send(phone: "123", body: "hello")
        require(accepted && api.sent.last?.clientRequestID == first.clientRequestID,
                "explicit retry must reuse its exact durable intent")
        require(api.sent.allSatisfy { $0.profileID == "a" }, "send escaped selected profile")
        print("PASS selection never falls back; ambiguous sends retain their idempotency key")

        // A deliberately uncancellable transport completes A after B is selected.
        for _ in 0..<10 { await Task.yield() }
        api.heldProfile = "a"
        let stale = Task { await store.refreshMessages() }
        while api.waiters.isEmpty { await Task.yield() }
        store.selectProfile("b")
        await store.refreshMessages()
        let pending = api.waiters
        api.waiters.removeAll()
        for waiter in pending {
            waiter.resume(returning: MultiStandbyMessagesResponse(
                messages: [message("old", "a")], nextCursor: nil, revision: 99))
        }
        await stale.value
        require(store.selectedProfileID == "b" && store.messages.isEmpty,
                "late A response overwrote B's inbox")
        api.heldProfile = nil
        print("PASS late profile inbox responses cannot overwrite the current selection")

        api.holdProfiles = true
        let oldProfileRequest = Task { await store.refreshProfiles() }
        while api.profileWaiters.isEmpty { await Task.yield() }
        api.holdProfiles = false
        api.profileResponse = decode(MultiStandbyProfilesResponse.self,
            "{\"profiles\":[],\"boot_id\":\"boot-2\",\"revision\":1}")
        await store.refreshProfiles()
        api.profileWaiters.removeFirst().resume(returning: decode(MultiStandbyProfilesResponse.self,
            "{\"profiles\":[{\"profile_id\":\"a\",\"card_id\":\"card\",\"sms_ready\":true}],\"boot_id\":\"boot-1\",\"revision\":999}"))
        await oldProfileRequest.value
        require(store.profiles.isEmpty, "late old-boot status replaced current state")
        print("PASS boot/request fencing rejects stale status even at a greater revision")

        // Changes include outgoing and status events: only confirmed incoming
        // received records create notifications, even while the page is closed.
        api.inbox["a"] = [message("incoming", "a"), message("sent", "a", direction: "outbound", state: "accepted")]
        api.inbox["b"] = [message("incoming", "b")]
        api.changeEvents = [change(1, "a", "incoming"), change(2, "a", "sent"), change(3, "b", "incoming")]
        store.viewingPage = false
        await store.pollChanges()
        await store.pollChanges()
        require(notifications == ["a:incoming", "b:incoming"], "background notifications mixed profiles or repeated/outgoing messages")
        let reopened = MultiStandbyStore(api: api, defaults: defaults, notificationSink: { message, _ in notifications.append(message.id) })
        reopened.setServiceRunning(true, schedulePolling: false)
        await reopened.pollChanges()
        require(notifications.count == 2, "restart replayed prior notifications")
        reopened.requestOpenMessage(profileID: "b", messageID: "incoming")
        require(reopened.selectedProfileID == "b" && reopened.selectedMessageID == "incoming",
                "notification destination lost profile identity")
        print("PASS app-level background notifications, profile-scoped routing and persisted cursor")

        let freshDefaults = UserDefaults(suiteName: suite + ".fresh")!
        defer { freshDefaults.removePersistentDomain(forName: suite + ".fresh") }
        var seededNotifications = 0
        let fresh = MultiStandbyStore(api: api, defaults: freshDefaults, notificationSink: { _, _ in seededNotifications += 1 })
        fresh.setServiceRunning(true, schedulePolling: false)
        await fresh.pollChanges()
        require(seededNotifications == 0, "first launch notified all historical messages")
        await reopened.delete(message("incoming", "a"))
        require(api.deleted.isEmpty, "cross-profile deletion was permitted")
        let cursor = decode(MultiStandbyChangesResponse.self, "{\"changes\":[],\"next_cursor\":42}")
        require(cursor.nextCursor?.value == "42", "numeric cursor compatibility lost")
        print("PASS first-load baseline, delete ownership and cursor contract")
        let archiveAPI = FakeAPI()
        archiveAPI.archiveCount = 3501
        archiveAPI.changeEvents = [change(3501, "a", "archive-3501")]
        var archiveNotifications: [String] = []
        let archive = MultiStandbyStore(api: archiveAPI, defaults: defaults, notificationSink: { message, _ in
            archiveNotifications.append(message.id)
        })
        archive.setServiceRunning(true, schedulePolling: false)
        await archive.pollChanges()
        require(archiveNotifications == ["a:archive-3501"] && archiveAPI.archiveReads == 1,
                "new notifications must jump to their durable revision beyond 3200 historical messages")
        require(defaults.string(forKey: "switch.multistandby.notificationCursor") == "3501",
                "large-history notification cursor failed to advance")
        archive.requestOpenMessage(profileID: "a", messageID: "archive-3501")
        for _ in 0..<1000 where !archive.messages.contains(where: { $0.messageID == "archive-3501" }) {
            await Task.yield()
        }
        require(archive.messages.contains(where: { $0.messageID == "archive-3501" }),
                "historical notification clicks must traverse beyond 32 pages")
        archive.setServiceRunning(false)
        print("PASS large archives retain live notifications, cursor progress and historical click routing")
        store.setServiceRunning(false)
        reopened.setServiceRunning(false)
        fresh.setServiceRunning(false)
    }

    static func message(_ id: String, _ profile: String, direction: String = "inbound", state: String = "received") -> MultiStandbyMessage {
        MultiStandbyMessage(messageID: id, profileID: profile, direction: direction,
            peer: "123", body: "same text", createdAtMS: 1_000, state: state, kind: nil)
    }
    static func change(_ revision: UInt64, _ profile: String, _ message: String) -> MultiStandbyChange {
        MultiStandbyChange(revision: revision, profileID: profile, messageID: message, direction: nil, kind: nil, state: nil, deleted: nil)
    }
}
