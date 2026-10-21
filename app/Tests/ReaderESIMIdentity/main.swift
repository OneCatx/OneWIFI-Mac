import Foundation

@main
struct ReaderESIMIdentityTests {
    @MainActor
    static func main() async {
        await delayedTerminalRefreshCannotEnterNextMutation()
        await oldOverviewCannotPublishDuringNextMutation()
        await readyIdentityChange()
        await lateReply(success: true)
        await lateReply(success: false)
        await incompleteIdentityAndReadiness()
        await transientReadinessDuringAction()
        await lateActionCannotAffectNewReader()
        await switchIdentityConfirmation()
        await switchUnknownOutcomeCannotPublishOptimistically()
        await bannerOwnership()
        await disconnectClearsAndReconnects()
        expect(ControlledAPI.shared.pendingCount == 0, "all fixture requests completed")
        print("PASS: 12 ReaderESIM identity groups; production Store + extracted View binding, no hardware/network")
    }

    @MainActor static let api = ControlledAPI.shared
    @MainActor static func expect(_ condition: @autoclosure () -> Bool, _ message: String) {
        if !condition() { fatalError(message) }
    }
    @MainActor static func settle() async {
        for _ in 0..<12 { await Task.yield() }
    }
    @MainActor static func waitForRequest(_ count: Int) async {
        for _ in 0..<1000 {
            if api.requests.count == count { return }
            await Task.yield()
        }
        fatalError("expected \(count) requests, found \(api.requests.count)")
    }
    @MainActor static func waitUntil(_ condition: () -> Bool) async {
        for _ in 0..<1000 {
            if condition() { return }
            await Task.yield()
        }
        fatalError("store did not reach expected state")
    }
    static func overview(_ provider: String, iccid: String = "synthetic-iccid") -> String {
        // Identical EID/ICCID across transports: reader identity, not card
        // contents or readiness booleans, must trigger the reload.
        """
        {"card_type":"euicc","profiles":[{"eid":"synthetic-eid","aid_hex":"A000", "profiles":[{"iccid":"\(iccid)","service_provider_name":"\(provider)","state":1}]}],"capabilities":{"can_refresh":true,"can_download":true,"can_switch":true,"can_rename":true,"can_delete":true,"can_probe_phonebook":false,"vendor_serial_available":false}}
        """
    }
    @MainActor static func fixture() -> ReaderCardViewBindingFixture {
        let view = ReaderCardViewBindingFixture()
        DashboardStore.shared = view.store
        return view
    }
    @MainActor static func loadReady(_ view: ReaderCardViewBindingFixture, name: String = "USB", provider: String = "A") async {
        let index = api.requests.count
        view.deliverContext(name: name)
        await waitForRequest(index + 1)
        api.complete(index, json: overview(provider))
        await waitUntil { view.readerESIMStore.loadPhase == .loaded }
        await settle()
    }

    static func targetProfile() -> ESIMProfileContext {
        ESIMProfileContext(eid: "synthetic-eid", aidHex: "A000", profile: ProfileItem(
            iccid: "8986001234567890123", name: "target", serviceProviderName: "target carrier",
            state: 0, stateText: nil, classText: nil))
    }

    @MainActor static func delayedTerminalRefreshCannotEnterNextMutation() async {
        let view = fixture()
        await loadReady(view)
        let store = view.readerESIMStore
        let index = api.requests.count
        store.switchProfile(targetProfile())
        await waitForRequest(index + 1)
        api.complete(index, json: #"{"switch_accepted":true,"reader_info":{"usim_info":{"iccid":"8986001234567890123"}},"operation":{"id":"switch-A","kind":"switch_profile","state":"succeeded","progress":100,"refresh_after_seconds":1,"started_at":"2026-09-30T00:00:00Z","updated_at":"2026-09-30T00:00:01Z"}}"#)
        await waitUntil { store.operation?.id == "switch-A" && store.action == nil }
        store.startDownload(ESIMDownloadRequest(smdp: "test.invalid", matchingID: nil, confirmationCode: nil, aid: nil, imei: nil))
        await waitForRequest(index + 2)
        // One bounded offline delay crosses the previous switch's timer. No
        // real reader, network service or repeated long-running loops are used.
        try? await Task.sleep(nanoseconds: 1_100_000_000)
        expect(api.requests.count == index + 2,
               "terminal refresh from switch A must not issue an eUICC read during download B")
        expect(store.action == .downloading && store.operation == nil && !store.isRefreshing,
               "new write owns its progress and must retire the previous operation card")
        expect(view.store.profileSynchronizations == 1 && store.overview != nil,
               "cancelled terminal refresh must neither clear identity again nor remove the loaded list")
        api.fail(index + 1)
        await waitUntil { !store.isBusy }
    }

    @MainActor static func oldOverviewCannotPublishDuringNextMutation() async {
        for success in [true, false] {
            let view = fixture()
            await loadReady(view)
            let store = view.readerESIMStore
            let index = api.requests.count
            store.reloadAfterMetadataChange()
            await waitForRequest(index + 1)
            store.switchProfile(targetProfile())
            await waitForRequest(index + 2)
            if success { api.complete(index, json: overview("stale carrier")) }
            else { api.fail(index) }
            await settle()
            expect(store.action == .switching(targetProfile().id) && store.banner == nil && !store.isRefreshing,
                   "older read success/failure must not alter the new write's status or notice")
            expect(store.profileGroups[0].profiles[0].profile.serviceProviderName == "A" &&
                   view.store.publishedOperators.compactMap { $0 } == ["A"],
                   "older overview cannot publish stale carrier while a card write is running")
            api.fail(index + 1)
            await waitUntil { !store.isBusy }
        }
    }
    @MainActor static func readyIdentityChange() async {
        let view = fixture()
        await loadReady(view)
        let store = view.readerESIMStore
        store.banner = ESIMBanner(tone: .error, message: "old reader error", detail: nil)
        view.readerESIMExpanded = true
        view.readerShowingDownload = true
        view.readerShowingNotifications = true
        let profile = store.profileGroups[0].profiles[0]
        view.readerRenamingProfile = profile
        view.readerDeletingProfile = profile
        view.readerEnablingProfile = profile
        let index = api.requests.count
        view.deliverContext(name: "BLE:fixture")
        expect(store.overview == nil && store.banner == nil, "reader change immediately removes old overview/banner")
        expect(!view.readerESIMExpanded && !view.readerShowingDownload && !view.readerShowingNotifications, "old sheets closed")
        expect(view.readerRenamingProfile == nil && view.readerDeletingProfile == nil && view.readerEnablingProfile == nil, "old action contexts cleared")
        await waitForRequest(index + 1)
        expect(api.requests[index].method == "GET" && api.requests[index].path == "api/reader/esim", "selection only issues a normal read")
        api.complete(index, json: overview("B"))
        await waitUntil { store.loadPhase == .loaded }
        let notice = ESIMBanner(tone: .success, message: "keep operation notice", detail: nil)
        store.banner = notice
        for _ in 0..<100 { view.deliverContext(name: "BLE:fixture") }
        await settle()
        expect(api.requests.count == index + 1, "unchanged polling does not reload")
        expect(store.banner?.id == notice.id && store.profileGroups[0].profiles[0].profile.serviceProviderName == "B", "unchanged polling does not clear")
        expect(view.store.publishedOperators.compactMap { $0 } == ["A", "B"], "new reader owns operator publication")
    }
    @MainActor static func lateReply(success: Bool) async {
        let view = fixture()
        let old = api.requests.count
        view.deliverContext(name: "USB")
        await waitForRequest(old + 1)
        view.deliverContext(name: "BLE:fixture")
        await waitForRequest(old + 2)
        api.complete(old + 1, json: overview("B"))
        await waitUntil { view.readerESIMStore.loadPhase == .loaded }
        let notice = ESIMBanner(tone: .success, message: "new reader", detail: nil)
        view.readerESIMStore.banner = notice
        if success { api.complete(old, json: overview("A")) } else { api.fail(old) }
        await settle()
        expect(view.readerESIMStore.profileGroups[0].profiles[0].profile.serviceProviderName == "B", "late old response cannot overwrite overview")
        expect(view.readerESIMStore.banner?.id == notice.id && view.readerESIMStore.loadPhase == .loaded, "late old failure cannot overwrite new state")
        expect(view.store.publishedOperators.compactMap { $0 } == ["B"], "late old response cannot publish old operator")
    }
    @MainActor static func incompleteIdentityAndReadiness() async {
        let view = fixture()
        let index = api.requests.count
        view.deliverContext(name: nil)
        view.deliverContext(name: "")
        view.deliverContext(name: "  ")
        view.deliverContext(name: "USB", connected: false)
        view.deliverContext(name: "USB", hasESIM: false)
        await settle()
        expect(api.requests.count == index, "incomplete selected identity/readiness does not read")
        view.deliverContext(name: "USB")
        for _ in 0..<10 { view.deliverContext(name: "USB") }
        await waitForRequest(index + 1)
        api.complete(index, json: overview("A"))
        await waitUntil { view.readerESIMStore.loadPhase == .loaded }
        view.deliverContext(name: "USB", hasESIM: false)
        view.deliverContext(name: "USB")
        await settle()
        expect(api.requests.count == index + 1 && view.readerESIMStore.overview != nil, "readiness-only transitions retain loaded identity")
    }
    @MainActor static func transientReadinessDuringAction() async {
        let view = fixture()
        await loadReady(view)
        let store = view.readerESIMStore
        let index = api.requests.count
        view.readerShowingDownload = true
        store.startDownload(ESIMDownloadRequest(smdp: "test.invalid", matchingID: nil, confirmationCode: nil, aid: nil, imei: nil))
        await waitForRequest(index + 1)
        view.deliverContext(name: "USB", hasESIM: false)
        view.deliverContext(name: "USB")
        await settle()
        expect(store.action == .downloading && view.readerShowingDownload, "readiness transition must not cancel active download or close sheet")
        expect(api.requests.count == index + 1, "readiness transition must not send extra read/write")
        api.fail(index)
        await waitUntil { store.action == nil }
        expect(store.banner?.message == "无法开始下载 Profile", "same-reader operation completion retained")
    }
    @MainActor static func bannerOwnership() async {
        let view = fixture()
        await loadReady(view)
        let store = view.readerESIMStore
        var index = api.requests.count
        store.load()
        await waitForRequest(index + 1)
        api.fail(index)
        await waitUntil { store.banner?.tone == .error }
        index += 1
        store.load()
        await waitForRequest(index + 1)
        api.complete(index, json: overview("A"))
        await waitUntil { store.banner == nil }
        index += 1
        store.load()
        await waitForRequest(index + 1)
        api.fail(index)
        await waitUntil { store.banner?.tone == .error }
        let operationNotice = ESIMBanner(tone: .warning, message: "keep operation warning", detail: nil)
        store.banner = operationNotice
        index += 1
        store.load()
        await waitForRequest(index + 1)
        api.complete(index, json: overview("B"))
        await waitUntil { store.profileGroups.first?.profiles.first?.profile.serviceProviderName == "B" }
        expect(store.banner?.id == operationNotice.id, "successful load must not clear newer operation banner")
    }
    @MainActor static func lateActionCannotAffectNewReader() async {
        for success in [true, false] {
            let view = fixture()
            await loadReady(view)
            let store = view.readerESIMStore
            let old = api.requests.count
            let target = ESIMProfileContext(eid: "synthetic-eid", aidHex: "A000", profile: ProfileItem(
                iccid: "8986001234567890123", name: "old target", serviceProviderName: "old carrier",
                state: 0, stateText: nil, classText: nil))
            store.switchProfile(target)
            await waitForRequest(old + 1)
            view.deliverContext(name: "BLE:fixture")
            await waitForRequest(old + 2)
            api.complete(old + 1, json: overview("B"))
            await waitUntil { store.loadPhase == .loaded }
            if success {
                api.complete(old, json: #"{"reader_info":{"usim_info":{"iccid":"8986001234567890123"}}}"#)
            } else { api.fail(old) }
            await settle()
            expect(store.action == nil && store.banner == nil, "late action result cannot replace new reader state")
            expect(view.store.publishedOperators.compactMap { $0 } == ["A", "B"], "late switch cannot publish old target operator")
            expect(view.store.profileSynchronizations == 0 && api.requests.count == old + 2, "late switch cannot synchronize or reload new reader")
        }
    }
    @MainActor static func switchIdentityConfirmation() async {
        let targetICCID = "8986001234567890123"
        for actualICCID in [targetICCID, targetICCID + "F", "8986001234567890124", ""] {
            let view = fixture()
            await loadReady(view)
            let store = view.readerESIMStore
            let index = api.requests.count
            let target = ESIMProfileContext(eid: "synthetic-eid", aidHex: "A000", profile: ProfileItem(
                iccid: targetICCID, name: "target", serviceProviderName: "target carrier",
                state: 0, stateText: nil, classText: nil))
            store.switchProfile(target)
            await waitForRequest(index + 1)
            api.complete(index, json: """
                {"switch_accepted":true,"reader_info":{"usim_info":{"iccid":"\(actualICCID)"}}}
                """)
            await waitForRequest(index + 2)
            let confirmed = actualICCID == targetICCID || actualICCID == targetICCID + "F"
            expect(view.store.publishedOperators.compactMap { $0 } == (confirmed ? ["A", "target carrier"] : ["A"]),
                   "accepted switch must not publish unconfirmed target operator")
            expect(store.banner?.tone == (confirmed ? .success : .warning), "missing operation cannot imply successful activation")
            // The automatic profile refresh must not undo the identity gate:
            // the eUICC may say enabled while its USIM still presents old data.
            api.complete(index + 1, json: overview("target carrier", iccid: targetICCID))
            await waitUntil { !store.isRefreshing }
            expect(view.store.publishedOperators.compactMap { $0 } == (confirmed ? ["A", "target carrier", "target carrier"] : ["A"]),
                   "profile enabled-state cannot override unconfirmed USIM identity")
            if !confirmed {
                view.store.readerUSIMInfo = FixtureUSIMInfo(iccid: targetICCID)
                store.load()
                await waitForRequest(index + 3)
                api.complete(index + 2, json: overview("target carrier", iccid: targetICCID))
                await waitUntil { view.store.publishedOperators.count == 2 }
                expect(view.store.publishedOperators.compactMap { $0 } == ["A", "target carrier"],
                       "verified USIM identity must allow a later profile publication")
            }
        }
    }
    @MainActor static func switchUnknownOutcomeCannotPublishOptimistically() async {
        for unknown in [true, false] {
            let view = fixture()
            await loadReady(view)
            let store = view.readerESIMStore
            let index = api.requests.count
            let targetICCID = "8986001234567890123"
            let target = ESIMProfileContext(eid: "synthetic-eid", aidHex: "A000", profile: ProfileItem(
                iccid: targetICCID, name: "target", serviceProviderName: "target carrier",
                state: 0, stateText: nil, classText: nil))
            store.switchProfile(target)
            await waitForRequest(index + 1)
            api.fail(index, error: unknown ? FixtureError.outcomeUnknown : FixtureError.failed)
            await waitUntil { store.action == nil }
            expect(store.banner?.message == (unknown ? "切换 Profile 结果待确认" : "切换 Profile 失败"),
                   "unknown outcome must remain distinct from definite failure")
            store.load(forceRefresh: true)
            await waitForRequest(index + 2)
            api.complete(index + 1, json: overview("target carrier", iccid: targetICCID))
            await waitUntil { !store.isRefreshing }
            expect(view.store.publishedOperators.compactMap { $0 } == (unknown ? ["A"] : ["A", "target carrier"]),
                   "only uncertain write outcomes must guard later operator publication")
            expect(view.store.profileSynchronizations == 0 && api.requests.count == index + 2,
                   "uncertain write must not trigger retry or another card mutation")
            if unknown {
                view.store.readerUSIMInfo = FixtureUSIMInfo(iccid: targetICCID)
                store.load(forceRefresh: true)
                await waitForRequest(index + 3)
                api.complete(index + 2, json: overview("target carrier", iccid: targetICCID))
                await waitUntil { !store.isRefreshing }
                expect(view.store.publishedOperators.compactMap { $0 } == ["A", "target carrier"],
                       "uncertain outcome gate clears only after actual USIM target is visible")
            }
        }
    }
    @MainActor static func disconnectClearsAndReconnects() async {
        let view = fixture()
        await loadReady(view)
        let index = api.requests.count
        view.deliverContext(name: "USB", connected: false)
        expect(view.readerESIMStore.overview == nil && view.readerESIMStore.loadPhase == .idle, "disconnect clears even if name retained")
        await settle()
        expect(api.requests.count == index, "disconnect does not read")
        view.deliverContext(name: "USB")
        await waitForRequest(index + 1)
        api.complete(index, json: overview("A"))
        await waitUntil { view.readerESIMStore.loadPhase == .loaded }
    }
}
