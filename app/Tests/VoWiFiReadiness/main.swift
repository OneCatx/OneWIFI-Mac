import Foundation

@main
struct VoWiFiReadinessTests {
    @MainActor static let api = ControlledAPI.shared

    @MainActor
    static func main() async {
        sameReaderIdentityGapPreservesOnlyCapabilities()
        await switchSynchronizationPreservesReaderCapabilities()
        await directStatusCannotRestoreReadinessWithoutIdentity()
        await explicitRustRefresh()
        await missingNativeSelectionVerifiesGoBeforeAutoSwitch()
        await fallbackIdentityPrecedesStatusDuringModuleHang()
        await repeatedAggregateFailuresDoNotStarveIdentityRecovery()
        await fallbackIdentityReplyCannotUndoAnotherProfileSwitch()
        await fallbackStatusReplyCannotUndoAnotherProfileSwitch()
        await fallbackRejectsUnseenProfileChangeBetweenInfoAndStatus()
        await readerMutationBlocksVoWiFiRequests()
        await aggregateIdentityIssueFencesOldDirectGET()
        await newerAggregateStatusFencesOldDirectGET()
        await sameReaderSameSIMReconnectFencesOldGeneration()
        await disableWithdrawsReadinessAndRechecksRustStatus()
        await failedAggregateFallsBackToDirectStatus()
        await aggregateFailureDoesNotDisturbReaderAction()
        await repeatedDirectFailuresWithdrawOldReadiness()
        await lateDirectReplyCannotRestoreOldProfile()
        await lateDirectReplyCannotRestoreOldSelectedReader()
        await lateDirectReplyCannotRestoreOldReaderAfterListChange()
        await switchSyncStatusCannotRestoreNewerProfile()
        await switchSyncRefreshCannotReapplyOldProfile()
        sameSIMReaderIdentityChangeWithdrawsReadiness()
        await connectingSameSIMReaderWithdrawsOldReadiness()
        await disconnectingReaderWithdrawsOldReadiness()
        await failedReaderTransitionsRecheckStatus()
        await readerMutationsDoNotInterruptActiveVoWiFiAction()
        await lateDirectReplyCannotRestoreOldCard()
        progressStagesFollowActualReadiness()
        expect(api.pendingCount == 0, "all fixture requests completed")
        print("PASS: reader VoWiFi fallback, manual refresh, failure fencing, and five-stage presentation")
    }

    @MainActor static func expect(_ condition: @autoclosure () -> Bool, _ message: String) {
        if !condition() { fatalError(message) }
    }

    @MainActor static func waitUntil(
        _ condition: () -> Bool,
        file: StaticString = #filePath,
        line: UInt = #line
    ) async {
        for _ in 0..<1_000 {
            if condition() { return }
            await Task.yield()
        }
        fatalError("fixture did not reach expected state at \(file):\(line); paths=\(api.requests.map(\.path))")
    }

    @MainActor static func waitForRequest(_ index: Int, path: String) async {
        await waitUntil { api.requests.count > index }
        expect(api.requests[index].path == path, "expected \(path), got \(api.requests[index].path)")
    }

    @MainActor static func waitForPath(_ path: String, after index: Int) async -> Int {
        await waitUntil { api.requests.dropFirst(index).contains { $0.path == path } }
        return api.requests.indices.first { $0 >= index && api.requests[$0].path == path }!
    }

    static func statusJSON(
        smsReady: Bool,
        callReady: Bool = true,
        readerID: String = "Fixture Reader",
        simID: String = "synthetic-sim",
        readerGeneration: UInt64? = 1
    ) -> String {
        """
        {"reader_id":"\(readerID)","sim_id":"\(simID)","available":true,
         \(readerGeneration.map { "\"reader_generation\":\($0)," } ?? "")
         "tunnel_state":"connected","ims_state":"registered",
         "sms_ready":\(smsReady),"call_ready":\(callReady),"progress_step":5}
        """
    }

    static let readerSnapshotJSON = """
        {"readers":["Fixture Reader"],"selected":"Fixture Reader",
         "info":{"status":{"connected":true,"reader_name":"Fixture Reader",
                            "reader_generation":1,
                            "has_card":true,"has_usim_info":true,"has_esim":false},
                 "usim_info":{"iccid":"synthetic-sim","imsi":"synthetic-imsi",
                              "mcc":"001","mnc":"01"}},
        "vowifi":\(statusJSON(smsReady: true))}
        """

    static func readerInfoJSON(
        readerName: String,
        simID: String,
        imsi: String? = nil,
        readerGeneration: UInt64 = 1
    ) -> String {
        """
        {"status":{"connected":true,"reader_name":"\(readerName)",
                   "reader_generation":\(readerGeneration),
                   "has_card":true,"has_usim_info":true,"has_esim":false},
         "usim_info":{"iccid":"\(simID)","imsi":"\(imsi ?? "synthetic-imsi-\(simID)")",
                      "mcc":"001","mnc":"01"}}
        """
    }

    @MainActor static func readyStore() -> ReaderRuntimeStore {
        let reader = ReaderRuntimeStore(backend: BackendProcess())
        let snapshot = try! JSONDecoder().decode(
            ReaderDeviceSnapshot.self, from: Data(readerSnapshotJSON.utf8))
        reader.applySnapshot(snapshot, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.voWiFiStatus?.smsReady == true, "fixture starts ready")
        return reader
    }

    @MainActor static func euiccStore() -> ReaderRuntimeStore {
        let reader = readyStore()
        let json = readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim")
            .replacingOccurrences(of: "\"has_esim\":false", with: "\"has_esim\":true,\"card_type\":\"euicc\"")
        reader.applyInfo(try! JSONDecoder().decode(ReaderInfoResponse.self, from: Data(json.utf8)))
        reader.updateProfileOperatorName("Previous carrier")
        return reader
    }

    @MainActor static func sameReaderIdentityGapPreservesOnlyCapabilities() {
        let reader = euiccStore()
        let missing = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data("""
            {"readers":["Fixture Reader"],"selected":"Fixture Reader",
             "vowifi":\(statusJSON(smsReady: true))}
            """.utf8))
        reader.applySnapshot(missing, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.connected && reader.hasESIM && reader.cardType == "euicc",
               "same-reader missing snapshot must keep known eUICC presentation capability")
        expect(reader.usimInfo == nil && reader.profileOperatorName == nil && reader.epdgFQDN == nil,
               "pending identity must never retain old SIM fields")
        expect(reader.voWiFiStatus == nil, "missing identity must reject even an attached stale ready snapshot")

        let pending = #"{"status":{"connected":true,"reader_name":"Fixture Reader","reader_generation":2,"has_card":true,"has_usim_info":false,"has_esim":false}}"#
        reader.applyInfo(try! JSONDecoder().decode(ReaderInfoResponse.self, from: Data(pending.utf8)))
        expect(reader.hasESIM && reader.cardType == "euicc" && reader.usimInfo == nil,
               "unresolved same-reader capability is a skeleton, not proof of SIM identity")
        for replacement in [
            pending.replacingOccurrences(of: "\"has_card\":true", with: "\"has_card\":false"),
            pending.replacingOccurrences(of: "\"has_esim\":false", with: "\"has_esim\":false,\"card_type\":\"physical_sim\""),
            readerInfoJSON(readerName: "Fixture Reader", simID: "replacement-physical-sim"),
            pending.replacingOccurrences(of: "Fixture Reader", with: "Another Reader")
        ] {
            let replaced = euiccStore()
            replaced.applyInfo(try! JSONDecoder().decode(ReaderInfoResponse.self, from: Data(replacement.utf8)))
            expect(!replaced.hasESIM && replaced.cardType != "euicc",
                   "confirmed card removal, physical SIM or reader change must retire old eUICC capability")
        }
        let other = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data(
            #"{"readers":["Another Reader"],"selected":"Another Reader"}"#.utf8))
        reader.applySnapshot(other, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(!reader.hasESIM && reader.cardType == nil, "unidentified new reader cannot inherit old capabilities")
        reader.clearRuntimeState()
        expect(!reader.connected && reader.usimInfo == nil && !reader.hasESIM && reader.cardType == nil,
               "real disconnect clears all reader-backed presentation and identity")
    }

    @MainActor static func switchSynchronizationPreservesReaderCapabilities() async {
        let reader = euiccStore()
        let index = api.requests.count
        reader.synchronizeAfterProfileSwitch()
        expect(reader.hasESIM && reader.cardType == "euicc" && reader.connected,
               "profile confirmation gap must retain the same eUICC page skeleton")
        expect(reader.usimInfo == nil && reader.profileOperatorName == nil && reader.voWiFiStatus == nil,
               "profile confirmation gap must immediately withdraw old identity and service readiness")
        await waitForRequest(index, path: "api/reader/refresh")
        // A later real disconnect still owns the state even if the old refresh succeeds.
        reader.clearRuntimeState()
        api.complete(index, json: readerInfoJSON(readerName: "Fixture Reader", simID: "late-card"))
        for _ in 0..<20 { await Task.yield() }
        expect(!reader.connected && !reader.hasESIM && reader.usimInfo == nil,
               "old synchronization cannot restore a disconnected card")
        expect(api.requests.count == index + 1, "cancelled generation does not launch follow-up VoWiFi read")
    }

    @MainActor static func directStatusCannotRestoreReadinessWithoutIdentity() async {
        let reader = euiccStore()
        let missing = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data(
            #"{"readers":["Fixture Reader"],"selected":"Fixture Reader"}"#.utf8))
        reader.applySnapshot(missing, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.identityPending && reader.hasESIM, "fixture keeps capability, not SIM identity")
        let index = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(index, path: "api/reader/vowifi")
        api.complete(index, json: statusJSON(smsReady: true))
        await waitUntil { !reader.voWiFiRefreshInFlight }
        expect(reader.voWiFiStatus == nil && reader.usimInfo == nil && reader.hasESIM,
               "direct status refresh cannot restore ready state while USIM identity is absent")
        expect(reader.error?.contains("已拒绝显示就绪") == true,
               "direct identity mismatch remains explicit instead of presenting a false ready service")
    }

    @MainActor static func missingNativeSelectionVerifiesGoBeforeAutoSwitch() async {
        let reader = readyStore()
        let firstRequest = api.requests.count
        api.readerListJSON = #"{"readers":["Fixture Reader","Fixture Reader B"],"selected":"Fixture Reader"}"#
        defer { api.readerListJSON = #"{"readers":["Fixture Reader"],"selected":"Fixture Reader"}"# }
        let missing = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data(
            #"{"readers":["Fixture Reader B"],"selected":null}"#.utf8))

        reader.applySnapshot(missing, issues: [], expectedGeneration: reader.generation, autoConnect: true)
        expect(reader.connected && reader.name == "Fixture Reader",
               "one native inventory miss must retain Go's selected reader until verification")
        expect(reader.voWiFiStatus == nil, "unverified aggregate must withdraw old VoWiFi readiness")
        expect(!reader.actionInFlight, "one native inventory miss must not auto-connect Reader B")

        let infoIndex = await waitForPath("api/reader/info", after: firstRequest)
        expect(!api.requests.dropFirst(firstRequest).contains { $0.path == "api/reader/connect" },
               "native scan did not issue a competing reader selection")
        api.complete(infoIndex, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim", imsi: "synthetic-imsi"))
        let statusIndex = await waitForPath("api/reader/vowifi", after: infoIndex + 1)
        api.complete(statusIndex, json: statusJSON(smsReady: true))
        await waitUntil { !reader.fallbackRecoveryInFlight }
        expect(reader.connected && reader.name == "Fixture Reader",
               "Go list recovery restored the original reader without switching")
        expect(!api.requests.dropFirst(firstRequest).contains { $0.path == "api/reader/connect" },
               "Go identity recovery never auto-selected Reader B")
    }

    @MainActor static func explicitRustRefresh() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.refreshVoWiFi()
        expect(api.requests.count == index, "normal Rust snapshot polling does not add direct requests")
        // Explicit refresh must bypass only the normal Rust snapshot dedupe.
        reader.refreshVoWiFi(force: true)
        await waitForRequest(index, path: "api/reader/vowifi")
        api.complete(index, json: statusJSON(smsReady: false, callReady: false))
        await waitUntil { reader.voWiFiStatus?.smsReady == false }
        expect(reader.voWiFiStatus?.callReady == false, "manual refresh published degraded call state")
    }

    @MainActor static func disableWithdrawsReadinessAndRechecksRustStatus() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.disableVoWiFi()
        await waitForRequest(index, path: "api/reader/vowifi")
        expect(reader.voWiFiStatus == nil,
               "stop request must withdraw old ready display before backend teardown")
        let identityGap = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data("""
            {"readers":["Fixture Reader"],"selected":"Fixture Reader",
             "vowifi":\(statusJSON(smsReady: true))}
            """.utf8))
        reader.applySnapshot(identityGap, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.voWiFiActionInFlight && reader.voWiFiStatus == nil,
               "identity gap must withdraw readiness without abandoning an in-flight stop")
        api.complete(index, json: "{}")
        let statusIndex = await waitForPath("api/reader/vowifi", after: index + 1)
        api.complete(statusIndex, json: #"{"available":true,"tunnel_state":"disconnected","ims_state":"idle"}"#)
        await waitUntil { reader.voWiFiStatus?.tunnelState == "disconnected" }
        expect(!reader.voWiFiActionInFlight, "stop completed without leaving switch busy")
    }

    @MainActor static func newerAggregateStatusFencesOldDirectGET() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(index, path: "api/reader/vowifi")

        let newerJSON = readerSnapshotJSON.replacingOccurrences(
            of: statusJSON(smsReady: true), with: statusJSON(smsReady: false, callReady: false))
        let newer = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data(newerJSON.utf8))
        reader.applySnapshot(newer, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.voWiFiStatus?.smsReady == false, "new aggregate status is published")
        api.complete(index, json: statusJSON(smsReady: true))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus?.smsReady == false && reader.voWiFiStatus?.callReady == false,
               "late direct response cannot override a newer same-card aggregate status")
    }

    @MainActor static func sameReaderSameSIMReconnectFencesOldGeneration() async {
        let reader = readyStore()
        let oldIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(oldIndex, path: "api/reader/vowifi")
        let reconnected = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(
                readerName: "Fixture Reader", simID: "synthetic-sim",
                imsi: "synthetic-imsi", readerGeneration: 2).utf8))
        reader.applyInfo(reconnected)
        expect(reader.voWiFiStatus == nil,
               "same-name same-SIM reader reconnect must withdraw old readiness")
        api.complete(oldIndex, json: statusJSON(smsReady: true, readerGeneration: 1))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil,
               "old generation direct GET cannot republish ready after reconnect")

        let missingIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(missingIndex, path: "api/reader/vowifi")
        api.complete(missingIndex, json: statusJSON(smsReady: true, readerGeneration: nil))
        await waitUntil { !reader.voWiFiRefreshInFlight }
        expect(reader.voWiFiStatus == nil,
               "live status without generation fails closed when current generation is known")

        let currentIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(currentIndex, path: "api/reader/vowifi")
        api.complete(currentIndex, json: statusJSON(smsReady: true, readerGeneration: 2))
        await waitUntil { reader.voWiFiStatus?.smsReady == true }
    }

    @MainActor static func aggregateIdentityIssueFencesOldDirectGET() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(index, path: "api/reader/vowifi")

        let snapshot = try! JSONDecoder().decode(
            ReaderDeviceSnapshot.self, from: Data(readerSnapshotJSON.utf8))
        let issue = DeviceSnapshotIssue(
            endpoint: "/api/reader/info", code: "upstream_snapshot_failed",
            message: "synthetic identity read failure", retryable: true)
        reader.applySnapshot(snapshot, issues: [issue],
                             expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.voWiFiStatus == nil, "identity issue withdraws readiness")
        api.complete(index, json: statusJSON(smsReady: true))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil,
               "late direct response cannot restore ready after aggregate identity issue")
    }

    @MainActor static func readerMutationBlocksVoWiFiRequests() async {
        for disconnecting in [false, true] {
            let reader = readyStore()
            let index = api.requests.count
            if disconnecting { reader.disconnect() } else { reader.connect("Fixture Reader B") }
            await waitForRequest(index, path: disconnecting ? "api/reader/disconnect" : "api/reader/connect")
            reader.refreshVoWiFi(force: true)
            reader.enableVoWiFi()
            reader.disableVoWiFi()
            expect(api.requests.count == index + 1,
                   "VoWiFi GET and mutations must wait until reader connect/disconnect completes")
            expect(!reader.voWiFiRefreshInFlight && !reader.voWiFiActionInFlight,
                   "blocked VoWiFi commands must not alter operation flags")
            if disconnecting {
                api.complete(index, json: "{}")
            } else {
                api.complete(index, json: readerInfoJSON(
                    readerName: "Fixture Reader B", simID: "synthetic-sim", imsi: "synthetic-imsi"))
            }
            await waitUntil { !reader.actionInFlight }
        }
    }

    @MainActor static func fallbackIdentityPrecedesStatusDuringModuleHang() async {
        let reader = readyStore()
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        gateway.readerGeneration = { reader.generation }
        gateway.readerActionInFlight = { reader.actionInFlight }
        gateway.refreshReaderFallback = {
            reader.refreshAfterAggregateFailure(autoConnect: false)
        }

        let index = api.requests.count
        gateway.refresh()
        await waitForRequest(index, path: "api/v2/device-snapshot")
        api.fail(index)
        let healthIndex = await waitForPath("api/health", after: index + 1)
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil,
               "aggregate failure must withdraw old readiness before fallback I/O")
        let infoIndex = await waitForPath("api/reader/info", after: index + 1)
        expect(!api.requests.dropFirst(index + 1).contains { $0.path == "api/reader/vowifi" },
               "fallback must verify reader identity before requesting VoWiFi status")

        api.complete(infoIndex, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        let statusIndex = await waitForPath("api/reader/vowifi", after: index + 1)
        expect(reader.usimInfo?.iccid == "synthetic-sim-b",
               "same-reader eSIM switch must update identity before status becomes visible")
        api.complete(statusIndex, json: statusJSON(smsReady: true, simID: "synthetic-sim-b"))
        await waitUntil { reader.voWiFiStatus?.smsReady == true }
        expect(api.pendingCount == 1, "module health remains independent and pending")

        api.complete(healthIndex, json: #"{"ok":true}"#)
        let moduleStatusIndex = await waitForPath("api/status", after: healthIndex + 1)
        api.complete(moduleStatusIndex, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
    }

    @MainActor static func repeatedAggregateFailuresDoNotStarveIdentityRecovery() async {
        let reader = readyStore()
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        gateway.readerGeneration = { reader.generation }
        gateway.refreshReaderFallback = {
            reader.refreshAfterAggregateFailure(autoConnect: false)
        }

        let firstAggregate = api.requests.count
        gateway.refresh()
        await waitForRequest(firstAggregate, path: "api/v2/device-snapshot")
        api.fail(firstAggregate)
        let pendingInfo = await waitForPath("api/reader/info", after: firstAggregate + 1)
        let firstHealth = await waitForPath("api/health", after: firstAggregate + 1)
        expect(reader.fallbackRecoveryInFlight && reader.voWiFiStatus == nil,
               "first aggregate failure keeps one identity-bound recovery active")
        api.complete(firstHealth, json: #"{"ok":true}"#)
        let firstModuleStatus = await waitForPath("api/status", after: firstHealth + 1)
        api.complete(firstModuleStatus, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }

        // A periodic aggregate failure arrives before the first bounded
        // reader-info request has returned. It must neither cancel that
        // request nor start a second identity/status chain.
        let secondAggregate = api.requests.count
        for _ in 0..<1_000 where api.requests.count <= secondAggregate {
            gateway.refresh()
            await Task.yield()
        }
        await waitForRequest(secondAggregate, path: "api/v2/device-snapshot")
        api.fail(secondAggregate)
        let secondHealth = await waitForPath("api/health", after: secondAggregate + 1)
        for _ in 0..<30 { await Task.yield() }
        expect(reader.fallbackRecoveryInFlight && reader.voWiFiStatus == nil,
               "repeated aggregate failure preserves the first recovery and withdrawn readiness")
        expect(api.requests.dropFirst(pendingInfo + 1).filter { $0.path == "api/reader/info" }.isEmpty,
               "repeated aggregate failure must not restart the pending identity request")

        api.complete(pendingInfo, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        let recoveredStatus = await waitForPath("api/reader/vowifi", after: secondAggregate + 1)
        api.complete(recoveredStatus, json: statusJSON(smsReady: true, simID: "synthetic-sim-b"))
        await waitUntil { reader.voWiFiStatus?.smsReady == true && !reader.fallbackRecoveryInFlight }
        expect(reader.usimInfo?.iccid == "synthetic-sim-b",
               "first recovery finishes with status bound to the newly read identity")

        api.complete(secondHealth, json: #"{"ok":true}"#)
        let secondModuleStatus = await waitForPath("api/status", after: secondHealth + 1)
        api.complete(secondModuleStatus, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
    }

    @MainActor static func fallbackIdentityReplyCannotUndoAnotherProfileSwitch() async {
        let reader = readyStore()
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        gateway.readerGeneration = { reader.generation }
        gateway.refreshReaderFallback = { reader.refreshAfterAggregateFailure(autoConnect: false) }

        let index = api.requests.count
        gateway.refresh()
        await waitForRequest(index, path: "api/v2/device-snapshot")
        api.fail(index)
        let infoIndex = await waitForPath("api/reader/info", after: index + 1)
        let healthIndex = await waitForPath("api/health", after: index + 1)
        let newer = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-c").utf8))
        reader.applyInfo(newer)
        api.complete(infoIndex, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.usimInfo?.iccid == "synthetic-sim-c" && reader.voWiFiStatus == nil,
               "fallback identity reply cannot restore a profile changed during recovery")
        expect(!api.requests.dropFirst(index + 1).contains { $0.path == "api/reader/vowifi" },
               "stale identity reply cannot start a VoWiFi status read")

        api.complete(healthIndex, json: #"{"ok":true}"#)
        let moduleStatusIndex = await waitForPath("api/status", after: healthIndex + 1)
        api.complete(moduleStatusIndex, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
    }

    @MainActor static func fallbackStatusReplyCannotUndoAnotherProfileSwitch() async {
        let reader = readyStore()
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        gateway.readerGeneration = { reader.generation }
        gateway.refreshReaderFallback = { reader.refreshAfterAggregateFailure(autoConnect: false) }

        let index = api.requests.count
        gateway.refresh()
        await waitForRequest(index, path: "api/v2/device-snapshot")
        api.fail(index)
        let infoIndex = await waitForPath("api/reader/info", after: index + 1)
        let healthIndex = await waitForPath("api/health", after: index + 1)
        api.complete(infoIndex, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        let statusIndex = await waitForPath("api/reader/vowifi", after: index + 1)
        let newer = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-c").utf8))
        reader.applyInfo(newer)
        api.complete(statusIndex, json: statusJSON(smsReady: true, simID: "synthetic-sim-b"))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.usimInfo?.iccid == "synthetic-sim-c" && reader.voWiFiStatus == nil,
               "fallback VoWiFi reply cannot restore a profile changed during status read")

        api.complete(healthIndex, json: #"{"ok":true}"#)
        let moduleStatusIndex = await waitForPath("api/status", after: healthIndex + 1)
        api.complete(moduleStatusIndex, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
    }

    @MainActor static func fallbackRejectsUnseenProfileChangeBetweenInfoAndStatus() async {
        let reader = readyStore()
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        gateway.readerGeneration = { reader.generation }
        gateway.refreshReaderFallback = { reader.refreshAfterAggregateFailure(autoConnect: false) }

        let index = api.requests.count
        gateway.refresh()
        await waitForRequest(index, path: "api/v2/device-snapshot")
        api.fail(index)
        let infoIndex = await waitForPath("api/reader/info", after: index + 1)
        let healthIndex = await waitForPath("api/health", after: index + 1)
        api.complete(infoIndex, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        let statusIndex = await waitForPath("api/reader/vowifi", after: index + 1)
        expect(reader.usimInfo?.iccid == "synthetic-sim-b", "fallback cached profile B identity")

        // The card switched physically to C after the info response, before
        // the Go status response. No Swift profile-change callback occurred.
        api.complete(statusIndex, json: statusJSON(smsReady: true, simID: "synthetic-sim-c"))
        await waitUntil { !reader.listRefreshInFlight }
        expect(reader.voWiFiStatus == nil,
               "ready status for unseen profile C cannot display under profile B identity")

        api.complete(healthIndex, json: #"{"ok":true}"#)
        let moduleStatusIndex = await waitForPath("api/status", after: healthIndex + 1)
        api.complete(moduleStatusIndex, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
    }

    @MainActor static func failedAggregateFallsBackToDirectStatus() async {
        let reader = readyStore()
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        gateway.readerGeneration = { reader.generation }
        gateway.applyReaderSnapshot = { snapshot, issues, generation in
            reader.applySnapshot(snapshot, issues: issues,
                                 expectedGeneration: generation, autoConnect: false)
        }
        var fallbackInvocations = 0
        gateway.refreshReaderFallback = {
            fallbackInvocations += 1
            reader.refreshAfterAggregateFailure(autoConnect: false)
        }

        let index = api.requests.count
        gateway.refresh()
        await waitForRequest(index, path: "api/v2/device-snapshot")
        api.fail(index)

        // Module health remains suspended while the independent reader path
        // verifies card identity before reading IMS/SMS readiness.
        let infoIndex = await waitForPath("api/reader/info", after: index + 1)
        expect(reader.voWiFiStatus == nil, "aggregate failure withdrew old readiness immediately")
        api.complete(infoIndex, json: readerInfoJSON(
            readerName: "Fixture Reader", simID: "synthetic-sim", imsi: "synthetic-imsi"))
        let readerIndex = await waitForPath("api/reader/vowifi", after: index + 1)
        let healthIndex = await waitForPath("api/health", after: index + 1)
        api.complete(readerIndex, json: statusJSON(smsReady: false, callReady: false))
        await waitUntil { reader.voWiFiStatus?.smsReady == false }
        expect(reader.voWiFiStatus?.callReady == false, "fallback withdrew stale call readiness")
        expect(api.pendingCount == 1, "module health remains pending while reader has recovered its status")

        api.complete(healthIndex, json: #"{"ok":true}"#)
        let moduleStatusIndex = await waitForPath("api/status", after: healthIndex + 1)
        api.complete(moduleStatusIndex, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
        for _ in 0..<20 { await Task.yield() }
        expect(fallbackInvocations == 1,
               "aggregate failure must launch identity-bound reader fallback exactly once")

        // When the aggregate path recovers it becomes authoritative again;
        // the exceptional direct read must not pin the UI in a failed state.
        let recoveryIndex = api.requests.count
        for _ in 0..<1_000 where api.requests.count <= recoveryIndex {
            gateway.refresh()
            await Task.yield()
        }
        await waitForRequest(recoveryIndex, path: "api/v2/device-snapshot")
        let recovered = """
            {"schema_version":2,"instance_id":"fixture-rust","revision":1,
             "ok":true,"complete":true,"captured_at_ms":1,"source":"rust-go-shadow",
             "module":{},"reader":\(readerSnapshotJSON),"issues":[]}
            """
        api.complete(recoveryIndex, json: recovered)
        await waitUntil { reader.voWiFiStatus?.smsReady == true }
    }

    @MainActor static func repeatedDirectFailuresWithdrawOldReadiness() async {
        let reader = readyStore()
        for attempt in 1...2 {
            let index = api.requests.count
            reader.refreshVoWiFi(force: true)
            await waitForRequest(index, path: "api/reader/vowifi")
            api.fail(index)
            await waitUntil { !reader.voWiFiRefreshInFlight }
            if attempt == 1 {
                expect(reader.voWiFiStatus?.smsReady == true, "one brief local read failure is tolerated")
            }
        }
        expect(reader.voWiFiStatus == nil, "two failed status reads clear previous ready state")
    }

    @MainActor static func aggregateFailureDoesNotDisturbReaderAction() async {
        let gateway = DeviceSnapshotStore(backend: BackendProcess())
        var readerFallbacks = 0
        gateway.readerActionInFlight = { true }
        gateway.refreshReaderFallback = { readerFallbacks += 1 }

        var index = api.requests.count
        gateway.refresh()
        await waitForRequest(index, path: "api/v2/device-snapshot")
        api.fail(index)
        index += 1
        await waitForRequest(index, path: "api/health")
        api.complete(index, json: #"{"ok":true}"#)
        index += 1
        await waitForRequest(index, path: "api/status")
        api.complete(index, json: "{}")
        await waitUntil { gateway.state.readPath == "rust-compatible-fallback" }
        for _ in 0..<20 { await Task.yield() }
        expect(readerFallbacks == 0,
               "aggregate failure cannot start fallback during reader card action")
    }

    @MainActor static func lateDirectReplyCannotRestoreOldCard() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(index, path: "api/reader/vowifi")
        reader.clearRuntimeState()
        api.complete(index, json: statusJSON(smsReady: true))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil && !reader.connected,
               "late status from prior generation cannot revive disconnected card")
    }

    @MainActor static func lateDirectReplyCannotRestoreOldProfile() async {
        let reader = readyStore()
        let oldIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(oldIndex, path: "api/reader/vowifi")

        let newInfo = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-b").utf8))
        reader.applyInfo(newInfo)
        expect(reader.voWiFiStatus == nil, "profile switch immediately hides former readiness")
        expect(!reader.voWiFiRefreshInFlight, "profile switch releases old direct-read slot")

        let newIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(newIndex, path: "api/reader/vowifi")
        api.complete(oldIndex, json: statusJSON(smsReady: true))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil, "old profile reply cannot restore ready state")
        expect(reader.voWiFiRefreshInFlight, "old profile reply cannot release new read slot")

        api.complete(newIndex, json: statusJSON(
            smsReady: false, callReady: false, simID: "synthetic-sim-b"))
        await waitUntil { reader.voWiFiStatus?.smsReady == false }
        expect(reader.voWiFiStatus?.callReady == false, "new profile status remains authoritative")
    }

    @MainActor static func lateDirectReplyCannotRestoreOldSelectedReader() async {
        let reader = readyStore()
        let oldIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(oldIndex, path: "api/reader/vowifi")

        let changed = """
            {"readers":["Fixture Reader","Fixture Reader B"],"selected":"Fixture Reader B",
             "info":\(readerInfoJSON(readerName: "Fixture Reader B", simID: "synthetic-sim-b")),
             "vowifi":null}
            """
        let snapshot = try! JSONDecoder().decode(ReaderDeviceSnapshot.self, from: Data(changed.utf8))
        reader.applySnapshot(snapshot, issues: [], expectedGeneration: reader.generation, autoConnect: false)
        expect(reader.name == "Fixture Reader B" && reader.voWiFiStatus == nil,
               "selected reader change hides former readiness")
        expect(!reader.voWiFiRefreshInFlight, "selected reader change releases old direct-read slot")

        let newIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(newIndex, path: "api/reader/vowifi")
        api.fail(oldIndex)
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil, "old reader failure cannot publish an obsolete status")
        expect(reader.voWiFiRefreshInFlight, "old reader failure cannot release new read slot")

        api.complete(newIndex, json: statusJSON(
            smsReady: false, callReady: false,
            readerID: "Fixture Reader B", simID: "synthetic-sim-b"))
        await waitUntil { reader.voWiFiStatus?.smsReady == false }
    }

    @MainActor static func lateDirectReplyCannotRestoreOldReaderAfterListChange() async {
        let reader = readyStore()
        let oldIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(oldIndex, path: "api/reader/vowifi")

        api.readerListJSON = #"{"readers":["Fixture Reader","Fixture Reader B"],"selected":"Fixture Reader B"}"#
        reader.refreshList(autoConnect: false, refreshConnectedInfo: false)
        await waitUntil { reader.name == "Fixture Reader B" }
        api.readerListJSON = #"{"readers":["Fixture Reader"],"selected":"Fixture Reader"}"#
        expect(reader.voWiFiStatus == nil, "reader list change hides former readiness")
        expect(!reader.voWiFiRefreshInFlight, "reader list change releases old direct-read slot")

        let infoIndex = await waitForPath("api/reader/info", after: oldIndex + 1)
        api.complete(infoIndex, json: readerInfoJSON(readerName: "Fixture Reader B", simID: "synthetic-sim-b"))
        await waitUntil { reader.usimInfo?.iccid == "synthetic-sim-b" }

        let newIndex = api.requests.count
        reader.refreshVoWiFi(force: true)
        await waitForRequest(newIndex, path: "api/reader/vowifi")
        api.complete(oldIndex, json: statusJSON(smsReady: true))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.voWiFiStatus == nil, "old list-selected reader reply cannot restore ready state")
        expect(reader.voWiFiRefreshInFlight, "old reply cannot release new list-selected reader read slot")

        api.complete(newIndex, json: statusJSON(
            smsReady: false, callReady: false,
            readerID: "Fixture Reader B", simID: "synthetic-sim-b"))
        await waitUntil { reader.voWiFiStatus?.smsReady == false }
    }

    @MainActor static func switchSyncStatusCannotRestoreNewerProfile() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.synchronizeAfterProfileSwitch()
        await waitForRequest(index, path: "api/reader/refresh")
        api.complete(index, json: readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        let statusIndex = await waitForPath("api/reader/vowifi", after: index + 1)
        expect(reader.usimInfo?.iccid == "synthetic-sim-b", "switch sync refreshed first profile")

        let newer = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-c").utf8))
        reader.applyInfo(newer)
        api.complete(statusIndex, json: statusJSON(smsReady: true))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.usimInfo?.iccid == "synthetic-sim-c" && reader.voWiFiStatus == nil,
               "late switch-sync status cannot restore prior profile readiness")
    }

    @MainActor static func switchSyncRefreshCannotReapplyOldProfile() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.synchronizeAfterProfileSwitch()
        await waitForRequest(index, path: "api/reader/refresh")

        let newer = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-c").utf8))
        reader.applyInfo(newer)
        api.complete(index, json: readerInfoJSON(readerName: "Fixture Reader", simID: "synthetic-sim-b"))
        for _ in 0..<30 { await Task.yield() }
        expect(reader.usimInfo?.iccid == "synthetic-sim-c" && reader.voWiFiStatus == nil,
               "late switch-sync identity refresh cannot reapply prior profile")
        expect(api.requests.count == index + 1,
               "obsolete switch-sync identity refresh cannot launch a status read")
    }

    @MainActor static func connectingSameSIMReaderWithdrawsOldReadiness() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.connect("Fixture Reader B")
        await waitForRequest(index, path: "api/reader/connect")
        expect(reader.voWiFiStatus == nil,
               "connect start must withdraw old reader readiness before backend teardown")
        api.complete(index, json: readerInfoJSON(
            readerName: "Fixture Reader B", simID: "synthetic-sim", imsi: "synthetic-imsi"))
        await waitUntil { reader.name == "Fixture Reader B" && !reader.actionInFlight }
        expect(reader.voWiFiStatus == nil,
               "same SIM moved to a different reader must not retain prior reader session")
    }

    @MainActor static func sameSIMReaderIdentityChangeWithdrawsReadiness() {
        let reader = readyStore()
        let changed = try! JSONDecoder().decode(
            ReaderInfoResponse.self,
            from: Data(readerInfoJSON(
                readerName: "Fixture Reader B", simID: "synthetic-sim", imsi: "synthetic-imsi").utf8))
        reader.applyInfo(changed)
        expect(reader.name == "Fixture Reader B" && reader.voWiFiStatus == nil,
               "reader identity change must clear readiness even with unchanged SIM identifiers")
    }

    @MainActor static func disconnectingReaderWithdrawsOldReadiness() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.disconnect()
        await waitForRequest(index, path: "api/reader/disconnect")
        expect(reader.voWiFiStatus == nil,
               "disconnect start must withdraw old readiness before backend teardown")
        api.complete(index, json: "{}")
        await waitUntil { !reader.connected && !reader.actionInFlight }
        expect(reader.voWiFiStatus == nil, "disconnected reader cannot remain ready")
    }

    @MainActor static func failedReaderTransitionsRecheckStatus() async {
        for disconnecting in [false, true] {
            let reader = readyStore()
            let index = api.requests.count
            if disconnecting { reader.disconnect() } else { reader.connect("Fixture Reader B") }
            await waitForRequest(index, path: disconnecting ? "api/reader/disconnect" : "api/reader/connect")
            expect(reader.voWiFiStatus == nil, "operation in flight withdraws unverified readiness")
            api.fail(index)
            let statusIndex = await waitForPath("api/reader/vowifi", after: index + 1)
            api.complete(statusIndex, json: statusJSON(smsReady: true))
            await waitUntil { reader.voWiFiStatus?.smsReady == true }
            expect(reader.connected && !reader.actionInFlight,
                   "failed reader transition rechecks the unchanged backend session")
        }
    }

    @MainActor static func readerMutationsDoNotInterruptActiveVoWiFiAction() async {
        let reader = readyStore()
        let index = api.requests.count
        reader.enableVoWiFi()
        await waitForRequest(index, path: "api/reader/vowifi")
        reader.connect("Fixture Reader B")
        reader.disconnect()
        expect(api.requests.count == index + 1 && reader.voWiFiActionInFlight,
               "reader mutations cannot invalidate an active VoWiFi start operation")
        api.fail(index)
        await waitUntil { !reader.voWiFiActionInFlight }
    }

    @MainActor static func progressStagesFollowActualReadiness() {
        let view = ProgressPolicyFixture()
        expect(view.progressStageStatus(stepNum: 4, step: 5, isError: false,
                                        inFlight: false, imsRegistered: true, smsReady: false) == .done,
               "successful REGISTER remains complete while only SMS is unavailable")
        expect(view.progressStageStatus(stepNum: 5, step: 5, isError: false,
                                        inFlight: false, imsRegistered: true, smsReady: false) == .pending,
               "old step five must not paint an unavailable SMS stage green")
        expect(view.progressStageStatus(stepNum: 4, step: 5, isError: false,
                                        inFlight: false, imsRegistered: false, smsReady: false) == .pending,
               "withdrawn IMS registration must not remain complete")
        expect(view.progressStageStatus(stepNum: 5, step: 5, isError: false,
                                        inFlight: false, imsRegistered: false, smsReady: true) == .pending,
               "stale SMS flag cannot complete a stage after IMS registration is withdrawn")
        expect(view.progressStageStatus(stepNum: 5, step: 5, isError: false,
                                        inFlight: false, imsRegistered: true, smsReady: true) == .done,
               "real SMS readiness completes stage five")
        expect(view.progressStageStatus(stepNum: 4, step: 4, isError: false,
                                        inFlight: true, imsRegistered: false, smsReady: false) == .current,
               "IMS registration in progress remains amber")
        expect(view.progressStageStatus(stepNum: 5, step: 5, isError: false,
                                        inFlight: true, imsRegistered: true, smsReady: false) == .current,
               "SMS setup in progress remains amber")
        expect(view.progressStageStatus(stepNum: 5, step: 5, isError: true,
                                        inFlight: false, imsRegistered: false, smsReady: false) == .failed,
               "fatal failure remains red")
    }
}
