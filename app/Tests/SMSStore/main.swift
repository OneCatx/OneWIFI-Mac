import Foundation

private struct CheckFailure: Error, CustomStringConvertible { let description: String }
@MainActor private func expect(_ condition: @autoclosure () -> Bool, _ message: String) throws {
    if !condition() { throw CheckFailure(description: message) }
}
@MainActor private func until(_ predicate: () -> Bool) async throws {
    for _ in 0..<2_000 {
        if predicate() { return }
        await Task.yield()
    }
    throw CheckFailure(description: "async test did not reach expected state")
}
@MainActor private func drain() async { for _ in 0..<30 { await Task.yield() } }

@main @MainActor private enum SMSStoreTests {
    static let api = SMSTestAPI.shared
    static let message = "[{\"id\":\"one\",\"sender\":\"888\",\"content\":\"BAL\",\"timestamp\":1}]"
    static func fixture() -> SMSStore {
        api.reset()
        SMSTestDefaults.shared.values = ["smsSeenIDsSeeded": true]
        UNUserNotificationCenter.shared.posts = []
        UNUserNotificationCenter.shared.holdSettings = false
        return SMSStore(backend: BackendProcess(), attentionStore: AttentionStore())
    }
    static func refresh(_ store: SMSStore, force: Bool = false) async throws {
        store.refresh(force: force)
        try await until { !store.refreshInFlight }
    }
    static func emptyMailboxAndDeviceTransition() async throws {
        let store = fixture()
        try await refresh(store)
        try await refresh(store)
        try await refresh(store)
        try expect(api.count("POST api/sms/refresh") == 1, "confirmed empty mailbox was repeatedly read from hardware")
        api.defaults["GET api/sms/devices"] = "{\"devices\":[{\"id\":\"dji\",\"name\":\"Module\",\"available\":true,\"ready\":false}]}"
        try await refresh(store)
        try expect(api.count("POST api/sms/refresh") == 2, "newly connected device was not read")
        api.defaults["GET api/sms/devices"] = "{\"devices\":[{\"id\":\"dji\",\"name\":\"Module\",\"available\":true,\"ready\":true}]}"
        try await refresh(store)
        try expect(api.count("POST api/sms/refresh") == 3, "newly ready device was not read")
        try await refresh(store)
        try expect(api.count("POST api/sms/refresh") == 3, "ready device was repeatedly re-read")
        try await refresh(store, force: true)
        try expect(api.count("POST api/sms/refresh") == 4, "manual refresh did not read device")
        api.defaults["GET api/sms"] = message
        try await refresh(store)
        try expect(store.items.count == 1 && store.attentionStore.received == 1,
                   "new message discovery/attention broke after empty mailbox optimization")
        try await refresh(store)
        try expect(store.attentionStore.received == 1 && UNUserNotificationCenter.shared.posts.count == 1,
                   "regular refresh duplicated notification")
    }
    static func forcedRefreshIsCoalesced() async throws {
        let store = fixture()
        try await refresh(store)
        api.heldRoutes.insert("GET api/sms")
        store.refresh()
        try await until { !(api.waiters["GET api/sms"] ?? []).isEmpty }
        store.refresh(force: true)
        store.refresh(force: true)
        try expect(store.busy, "queued manual refresh lost busy feedback")
        api.heldRoutes.remove("GET api/sms")
        api.resolve("GET api/sms", json: "[]")
        try await until { !store.refreshInFlight }
        try expect(api.count("POST api/sms/refresh") == 2, "manual refresh was lost or executed twice")
    }
    static func deletionInvalidatesLateList(senderDeletion: Bool) async throws {
        let store = fixture()
        api.defaults["GET api/sms"] = message
        try await refresh(store)
        let item = store.items[0]
        api.heldRoutes.insert("GET api/sms")
        store.refresh()
        try await until { !(api.waiters["GET api/sms"] ?? []).isEmpty }
        api.heldRoutes.remove("GET api/sms")
        api.defaults["GET api/sms"] = "[]"
        if senderDeletion { _ = try await store.deleteMessages(from: " 888 ") }
        else { _ = try await store.deleteMessage(item) }
        await drain()
        api.resolve("GET api/sms", json: message)
        try await until { !store.refreshInFlight }
        await drain()
        try expect(store.items.isEmpty, "late pre-deletion response restored deleted message")
        try expect(api.count("GET api/sms") == 3, "deletion did not schedule reconciliation")
        try expect(store.attentionStore.received == 1, "stale list repeated attention notification")
    }
    static func stoppedLifecycleRejectsOldResultAndError() async throws {
        for reject in [false, true] {
            let store = fixture()
            api.heldRoutes.insert("GET api/sms")
            store.refresh(force: true)
            try await until { !(api.waiters["GET api/sms"] ?? []).isEmpty }
            store.backend.state = .stopped
            store.stopPolling()
            try expect(!store.busy && !store.refreshInFlight, "stop retained busy/in-flight state")
            store.backend.state = .running
            api.heldRoutes.remove("GET api/sms")
            try await refresh(store)
            if reject { api.reject("GET api/sms") }
            else { api.resolve("GET api/sms", json: message) }
            await drain()
            try expect(store.items.isEmpty && store.lastError == nil && store.attentionStore.received == 0,
                       "old lifecycle response/error changed current state")
            try expect(api.count("POST api/sms/refresh") == 2, "backend restart skipped initial device read")
        }
    }
    static func notificationAuthorizationCannotResurrectDeletedItem() async throws {
        let store = fixture()
        api.defaults["GET api/sms"] = message
        let center = UNUserNotificationCenter.shared
        center.holdSettings = true
        store.refresh()
        try await until { center.settingsWaiter != nil }
        try expect(store.items.count == 1, "list waited on notification permission before publishing")
        api.defaults["GET api/sms"] = "[]"
        _ = try await store.deleteMessage(store.items[0])
        try await until { !store.refreshInFlight }
        center.holdSettings = false
        center.settingsWaiter?.resume(returning: UNNotificationSettings())
        center.settingsWaiter = nil
        await drain()
        try expect(store.items.isEmpty && center.posts.isEmpty, "late notification authorization posted deleted message")
    }
    static func oldDeleteCannotMutateNewLifecycle() async throws {
        let store = fixture()
        api.defaults["GET api/sms"] = message
        try await refresh(store)
        api.heldRoutes.insert("POST api/sms/delete")
        let item = store.items[0]
        let deletion = Task { try await store.deleteMessage(item) }
        try await until { !(api.waiters["POST api/sms/delete"] ?? []).isEmpty }
        store.stopPolling()
        try await refresh(store)
        api.resolve("POST api/sms/delete", json: "{\"deleted\":true}")
        _ = try await deletion.value
        try expect(store.items.count == 1, "old backend deletion mutated restarted backend snapshot")
    }
    static func main() async {
        let tests: [(String, () async throws -> Void)] = [
            ("known empty mailbox, device transitions, manual refresh and new message", emptyMailboxAndDeviceTransition),
            ("in-flight manual refresh is retained and coalesced", forcedRefreshIsCoalesced),
            ("single delete rejects late old list", { try await deletionInvalidatesLateList(senderDeletion: false) }),
            ("sender delete rejects late old list", { try await deletionInvalidatesLateList(senderDeletion: true) }),
            ("stop/restart rejects stale success and error", stoppedLifecycleRejectsOldResultAndError),
            ("notification authorization cannot publish deleted message", notificationAuthorizationCannotResurrectDeletedItem),
            ("old delete cannot mutate new backend lifecycle", oldDeleteCannotMutateNewLifecycle)
        ]
        let filter = ProcessInfo.processInfo.environment["SMS_STORE_TEST_FILTER"]
        let selectedTests = tests.filter { filter == nil || $0.0.contains(filter!) }
        guard !selectedTests.isEmpty else { print("No SMS store tests matched filter"); exit(2) }
        var failures = 0
        for (name, test) in selectedTests {
            do { try await test(); print("PASS: \(name)") }
            catch { failures += 1; print("FAIL: \(name): \(error)") }
        }
        if failures > 0 { exit(1) }
        print("SMS store: \(selectedTests.count) production async event groups passed (isolated IO; no hardware)")
    }
}
