import Combine
import Foundation

/// Owns the complete reader identity and Wi-Fi Calling lifecycle.
///
/// A monotonically increasing generation rejects replies that belong to an
/// older reader or eSIM profile. User-entered proxy and ePDG settings survive
/// disconnects; every value derived from the physical card is cleared as one
/// unit when the reader disappears.
@MainActor
final class ReaderRuntimeStore: ObservableObject {
    @Published private(set) var state = ReaderRuntimeState.initial
    var revision: UInt64 { state.revision }
    var readers: [String] { state.readers }
    var listLoaded: Bool { state.listLoaded }
    var connected: Bool { state.connected }
    var name: String? { state.name }
    var usimInfo: USIMInfo? { state.usimInfo }
    /// Presentation may retain the selected reader's capability skeleton,
    /// but no SIM-dependent service is ready until a fresh identity exists.
    var identityPending: Bool { connected && usimInfo == nil }
    var profileOperatorName: String? { state.profileOperatorName }
    var epdgFQDN: String? { state.epdgFQDN }
    var carrierConfigError: String? { state.carrierConfigError }
    var hasESIM: Bool { state.hasESIM }
    var cardType: String? { state.cardType }
    var error: String? { state.error }
    var actionInFlight: Bool { state.actionInFlight }
    var voWiFiStatus: ReaderVoWiFiStatus? { state.voWiFiStatus }
    var voWiFiLoaded: Bool { state.voWiFiLoaded }
    var voWiFiActionInFlight: Bool { state.voWiFiActionInFlight }
    @Published var proxyIP: String = UserDefaults.standard.string(forKey: "readerVoWiFiProxyIP") ?? "" {
        didSet { UserDefaults.standard.set(proxyIP, forKey: "readerVoWiFiProxyIP") }
    }
    @Published var proxyPort: String = UserDefaults.standard.string(forKey: "readerVoWiFiProxyPort") ?? "" {
        didSet { UserDefaults.standard.set(proxyPort, forKey: "readerVoWiFiProxyPort") }
    }
    @Published var customEPDG: String = UserDefaults.standard.string(forKey: "readerVoWiFiEPDG") ?? "" {
        didSet { UserDefaults.standard.set(customEPDG, forKey: "readerVoWiFiEPDG") }
    }
    @Published var customEPDGPort: String = UserDefaults.standard.string(forKey: "readerVoWiFiEPDGPort") ?? "" {
        didSet { UserDefaults.standard.set(customEPDGPort, forKey: "readerVoWiFiEPDGPort") }
    }
    @Published var proxyCheckResult: ProxyCheckResult?
    @Published var proxyChecking = false
    @Published var epdgError: String?

    private(set) var generation = 0
    private(set) var readerSessionGeneration: UInt64?
    private var requiresReaderGeneration = false
    private(set) var listRefreshInFlight = false
    private(set) var fallbackRecoveryInFlight = false
    private(set) var voWiFiRefreshInFlight = false
    private(set) var lastVoWiFiHealthRefreshAt: Date?
    // A single status GET can fail while the local backend is restarting.
    // Do not immediately turn a healthy card red, but never leave a stale
    // ready state visible after repeated failures either.
    private var consecutiveVoWiFiStatusReadFailures = 0

    var onToast: ((String, Bool, String?) -> Void)?

    private let backend: BackendProcess
    private var voWiFiPollTask: Task<Void, Never>?
    private var proxyCheckTask: Task<Void, Never>?
    private var listRequestRevision: UInt64 = 0
    private var voWiFiOperationRevision: UInt64 = 0
    private var voWiFiStatusRevision: UInt64 = 0
    private var proxyCheckRevision: UInt64 = 0

    init(backend: BackendProcess) {
        self.backend = backend
    }

    var epdgPortValue: Int? {
        let value = customEPDGPort.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty, let port = Int(value), (1...65_535).contains(port) else { return nil }
        return port
    }

    var proxy: String? {
        var host = proxyIP.trimmingCharacters(in: .whitespacesAndNewlines)
        let portText = proxyPort.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !host.isEmpty,
              !host.contains(where: { $0.isWhitespace || $0.isNewline }),
              !host.contains("/"),
              !host.contains("://"),
              let port = Int(portText),
              (1...65_535).contains(port) else { return nil }
        if host.contains(":"), !(host.hasPrefix("[") && host.hasSuffix("]")) {
            host = "[\(host)]"
        }
        return "socks5://\(host):\(port)"
    }

    /// Empty proxy fields let the native transport selector first inspect a
    /// usable macOS SOCKS/Surge System Proxy. If none can be verified, it uses
    /// the current network's legal direct/fake-IP route. Once either field is
    /// populated, both must form a valid endpoint so a typo cannot silently
    /// change the requested transport.
    var proxyConfigurationError: String? {
        let host = proxyIP.trimmingCharacters(in: .whitespacesAndNewlines)
        let port = proxyPort.trimmingCharacters(in: .whitespacesAndNewlines)
        if host.isEmpty && port.isEmpty { return nil }
        guard proxy != nil else {
            return "请同时填写合法的代理 IP/主机名和 1–65535 端口，或将两项清空后自动检测系统 SOCKS / Surge"
        }
        return nil
    }

    var operatorDisplayName: String {
        if let value = usimInfo?.operatorName?.trimmingCharacters(in: .whitespacesAndNewlines),
           !value.isEmpty {
            return value
        }
        if let profileOperatorName, !profileOperatorName.isEmpty { return profileOperatorName }
        return usimInfo?.operatorDisplayName ?? "未知运营商"
    }

    func markVoWiFiHealthRefresh() {
        lastVoWiFiHealthRefreshAt = Date()
    }

    func reset() {
        listRequestRevision &+= 1
        listRefreshInFlight = false
        fallbackRecoveryInFlight = false
        proxyCheckRevision &+= 1
        voWiFiPollTask?.cancel()
        voWiFiPollTask = nil
        proxyCheckTask?.cancel()
        proxyCheckTask = nil
        proxyCheckResult = nil
        proxyChecking = false
        epdgError = nil
        consecutiveVoWiFiStatusReadFailures = 0
        clearRuntimeState(clearList: true)
    }

    func applySnapshot(
        _ snapshot: ReaderDeviceSnapshot,
        issues: [DeviceSnapshotIssue],
        expectedGeneration: Int,
        autoConnect: Bool
    ) {
        guard !actionInFlight, expectedGeneration == generation else { return }
        let voWiFiIssue = issues.first { $0.endpoint == "/api/reader/vowifi" }
        if let issue = issues.first(where: {
            $0.endpoint == "/api/reader/list"
                || $0.endpoint == "/api/v2/reader-discovery"
                || $0.endpoint == "/api/reader/info"
        }) {
            invalidatePendingVoWiFiStatusReads()
            commit { state in
                state.listLoaded = true
                // The aggregate snapshot could not verify reader identity.
                // Do not leave a previously ready IMS/VoWiFi state visible;
                // the start/stop operation itself must keep running so its
                // completion path can reconcile the real backend state.
                Self.clearVoWiFiSnapshotFields(&state)
                state.error = "读取读卡器快照失败：\(issue.message)"
            }
            return
        }

        guard let selected = snapshot.selected else {
            if connected, name != nil {
                // Native discovery can miss one scan while Go still owns the
                // selected card. Verify the logical selection through Go's
                // read-only list before clearing it or auto-selecting another
                // reader. The confirmed hotplug monitor retires a real loss.
                refreshAfterAggregateFailure(autoConnect: false)
                return
            }
            clearRuntimeState(
                keepingReaders: snapshot.readers,
                listLoaded: true)
            if autoConnect, let first = snapshot.preferredAutoConnectReader { connect(first) }
            return
        }

        let selectionChanged = name != selected
        if let info = snapshot.info {
            applyInfo(
                info,
                readers: snapshot.readers,
                listLoaded: true,
                clearVoWiFi: selectionChanged)
        } else {
            let retainCapabilities = connected && !selectionChanged
            if selectionChanged {
                invalidateVoWiFiOperationForCardTransition()
            } else {
                invalidatePendingVoWiFiStatusReads()
            }
            commit { state in
                Self.clearCardIdentityFields(&state, preservingCapabilities: retainCapabilities)
                if selectionChanged {
                    Self.clearVoWiFiFields(&state)
                } else {
                    // Keep ownership of an in-flight stop/start, never its
                    // old ready snapshot while SIM identity is unavailable.
                    Self.clearVoWiFiSnapshotFields(&state)
                }
                state.readers = snapshot.readers
                state.listLoaded = true
                state.name = selected
                state.connected = true
                state.error = "读卡器已选择，但身份快照尚未就绪"
            }
        }
        if let status = snapshot.vowifi, !voWiFiActionInFlight, usimInfo != nil {
            publishVoWiFiStatus(status)
        } else if voWiFiIssue != nil, !voWiFiActionInFlight {
            invalidatePendingVoWiFiStatusReads()
            commit { state in
                state.voWiFiStatus = nil
                state.voWiFiLoaded = true
            }
        }
    }

    func refreshList(
        autoConnect: Bool = true,
        refreshConnectedInfo: Bool = true,
        refreshVoWiFiAfterIdentity: Bool = false
    ) {
        guard case .running = backend.state, !actionInFlight, !listRefreshInFlight else { return }
        let expectedGeneration = generation
        listRequestRevision &+= 1
        let requestRevision = listRequestRevision
        let initialOperationRevision = voWiFiOperationRevision
        let initialStatusRevision = voWiFiStatusRevision
        listRefreshInFlight = true
        Task {
            var expectedOperationRevision = initialOperationRevision
            var expectedStatusRevision = initialStatusRevision
            defer {
                if requestRevision == listRequestRevision {
                    listRefreshInFlight = false
                    if refreshVoWiFiAfterIdentity { fallbackRecoveryInFlight = false }
                }
            }
            do {
                let response: ReaderListResponse = try await APIClient(timeoutInterval: 10)
                    .get("api/reader/list")
                guard expectedGeneration == generation,
                      requestRevision == listRequestRevision,
                      !actionInFlight,
                      (!refreshVoWiFiAfterIdentity || (
                        expectedOperationRevision == voWiFiOperationRevision &&
                        expectedStatusRevision == voWiFiStatusRevision)) else { return }
                if let selected = response.selected {
                    let selectionChanged = name != selected
                    if selectionChanged {
                        // A list-only reader transition must fence pending
                        // direct VoWiFi reads just like an aggregate snapshot
                        // or profile identity transition does.
                        invalidateVoWiFiOperationForCardTransition()
                        consecutiveVoWiFiStatusReadFailures = 0
                        expectedOperationRevision = voWiFiOperationRevision
                        expectedStatusRevision = voWiFiStatusRevision
                    }
                    commit { state in
                        if selectionChanged { Self.clearDerivedFields(&state) }
                        state.readers = response.readers
                        state.listLoaded = true
                        state.error = nil
                        state.name = selected
                        state.connected = true
                    }
                    if refreshVoWiFiAfterIdentity {
                        // A failed aggregate cannot safely publish VoWiFi
                        // status against the prior profile. Read the current
                        // card identity first, even when reader name is the
                        // same, then read status without waiting for module
                        // health/status compatibility calls.
                        let info: ReaderInfoResponse = try await APIClient(timeoutInterval: 10)
                            .get("api/reader/info")
                        guard expectedGeneration == generation,
                              requestRevision == listRequestRevision,
                              !actionInFlight,
                              expectedOperationRevision == voWiFiOperationRevision,
                              expectedStatusRevision == voWiFiStatusRevision else { return }
                        applyInfo(info, readers: response.readers, listLoaded: true,
                                  clearVoWiFi: selectionChanged)
                        guard expectedGeneration == generation, connected else { return }
                        expectedOperationRevision = voWiFiOperationRevision
                        expectedStatusRevision = voWiFiStatusRevision

                        let status: ReaderVoWiFiStatus = try await APIClient(timeoutInterval: 10)
                            .get("api/reader/vowifi")
                        guard expectedGeneration == generation,
                              requestRevision == listRequestRevision,
                              !actionInFlight,
                              expectedOperationRevision == voWiFiOperationRevision,
                              expectedStatusRevision == voWiFiStatusRevision else { return }
                        publishDirectVoWiFiStatus(status)
                    } else if refreshConnectedInfo || selectionChanged {
                        refreshInfo()
                    }
                } else {
                    clearRuntimeState(
                        keepingReaders: response.readers,
                        listLoaded: true)
                }
                if response.selected == nil, autoConnect, let first = response.readers.first {
                    connect(first)
                }
            } catch {
                guard expectedGeneration == generation,
                      requestRevision == listRequestRevision,
                      (!refreshVoWiFiAfterIdentity || (
                        expectedOperationRevision == voWiFiOperationRevision &&
                        expectedStatusRevision == voWiFiStatusRevision)) else { return }
                commit { state in
                    state.listLoaded = true
                    state.error = refreshVoWiFiAfterIdentity
                        ? "回退读取读卡器身份与 VoWiFi 状态失败：\(error.localizedDescription)"
                        : "枚举读卡器失败：\(error.localizedDescription)"
                }
            }
        }
    }

    /// Rust aggregate failure recovery is independent from module reads.
    /// Withdraw stale readiness immediately; the bounded list→info→status
    /// sequence is the only path allowed to republish it.
    func refreshAfterAggregateFailure(autoConnect: Bool = true) {
        guard case .running = backend.state, !actionInFlight else { return }
        if fallbackRecoveryInFlight && voWiFiStatus == nil {
            // Another failed aggregate must not restart a bounded recovery
            // faster than list→info→status can finish. The first attempt has
            // already withdrawn readiness and remains authoritative.
            return
        }
        invalidatePendingVoWiFiStatusReads()
        commit { state in Self.clearVoWiFiSnapshotFields(&state) }
        // A manual list read might already be pending. Supersede it so its
        // reply cannot race with this identity-bound recovery sequence.
        listRequestRevision &+= 1
        listRefreshInFlight = false
        fallbackRecoveryInFlight = true
        refreshList(
            autoConnect: autoConnect,
            refreshConnectedInfo: true,
            refreshVoWiFiAfterIdentity: true)
    }

    func connect(_ reader: String) {
        guard !actionInFlight else { return }
        guard !voWiFiActionInFlight else {
            toast("Wi-Fi Calling 操作正在进行，请稍后切换读卡器", false, "读卡器")
            return
        }
        generation &+= 1
        let expectedGeneration = generation
        invalidateVoWiFiOperationForCardTransition()
        consecutiveVoWiFiStatusReadFailures = 0
        commit { state in
            state.actionInFlight = true
            state.error = nil
            Self.clearVoWiFiSnapshotFields(&state)
        }
        Task {
            defer { if expectedGeneration == generation && actionInFlight { setActionInFlight(false) } }
            do {
                let response: ReaderInfoResponse = try await APIClient(timeoutInterval: 30)
                    .send("api/reader/connect", body: ReaderConnectRequest(reader: reader))
                guard expectedGeneration == generation else { return }
                applyInfo(response)
                toast("读卡器已连接", true, "读卡器")
            } catch {
                guard expectedGeneration == generation else { return }
                setError("连接读卡器失败：\(error.localizedDescription)")
                toast("连接失败：\(error.localizedDescription)", false, "读卡器")
                setActionInFlight(false)
                refreshVoWiFi(force: true)
            }
        }
    }

    func disconnect() {
        guard !actionInFlight else { return }
        guard !voWiFiActionInFlight else {
            toast("Wi-Fi Calling 操作正在进行，请稍后断开读卡器", false, "读卡器")
            return
        }
        generation &+= 1
        let expectedGeneration = generation
        invalidateVoWiFiOperationForCardTransition()
        consecutiveVoWiFiStatusReadFailures = 0
        commit { state in
            state.actionInFlight = true
            Self.clearVoWiFiSnapshotFields(&state)
        }
        Task {
            defer { if expectedGeneration == generation && actionInFlight { setActionInFlight(false) } }
            do {
                let _: EmptyResponse = try await APIClient(timeoutInterval: 15)
                    .send("api/reader/disconnect", as: EmptyResponse.self)
                guard expectedGeneration == generation else { return }
                clearRuntimeState()
                toast("读卡器已断开", true, "读卡器")
            } catch {
                guard expectedGeneration == generation else { return }
                setError("断开读卡器失败：\(error.localizedDescription)")
                toast("断开失败：\(error.localizedDescription)", false, "读卡器")
                setActionInFlight(false)
                refreshVoWiFi(force: true)
            }
        }
    }

    func refreshInfo() {
        guard !actionInFlight else { return }
        let expectedGeneration = generation
        setActionInFlight(true)
        Task {
            defer { if expectedGeneration == generation { setActionInFlight(false) } }
            do {
                let response: ReaderInfoResponse = try await APIClient(timeoutInterval: 30)
                    .get("api/reader/info")
                guard expectedGeneration == generation else { return }
                applyInfo(response)
            } catch {
                guard expectedGeneration == generation else { return }
                setError("获取读卡器信息失败：\(error.localizedDescription)")
            }
        }
    }

    func refreshSIM() {
        guard !actionInFlight else { return }
        let expectedGeneration = generation
        setActionInFlight(true)
        Task {
            defer { if expectedGeneration == generation { setActionInFlight(false) } }
            do {
                let response: ReaderInfoResponse = try await APIClient(timeoutInterval: 30)
                    .send("api/reader/refresh")
                guard expectedGeneration == generation else { return }
                applyInfo(response)
                toast("SIM 卡信息已刷新", true, "读卡器")
            } catch {
                guard expectedGeneration == generation else { return }
                setError("刷新 SIM 失败：\(error.localizedDescription)")
                toast("刷新失败：\(error.localizedDescription)", false, "读卡器")
            }
        }
    }

    func applyInfo(
        _ response: ReaderInfoResponse,
        readers: [String]? = nil,
        listLoaded: Bool? = nil,
        clearVoWiFi: Bool = false
    ) {
        guard response.status.connected else {
            clearRuntimeState(
                keepingReaders: readers ?? self.readers,
                listLoaded: listLoaded ?? self.listLoaded)
            return
        }
        let carrier = response.usimInfo?.operatorName?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        let readerChanged = name != response.status.readerName
        // A same-reader USIM refresh can briefly have no capability result.
        // Retain only the known card-type skeleton, not the old SIM identity.
        // Explicit removal/type or a newly read SIM always replaces it, so a
        // physical SIM inserted into the same reader cannot inherit eUICC UI.
        let retainCapabilities = connected && !readerChanged && response.status.hasCard
            && response.usimInfo == nil
            && (response.status.cardType?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ?? true)
        let profileChanged = usimInfo?.iccid != response.usimInfo?.iccid
            || usimInfo?.imsi != response.usimInfo?.imsi
        let nextReaderGeneration = response.status.readerGeneration.flatMap { $0 == 0 ? nil : $0 }
        let readerGenerationChanged = readerSessionGeneration != nextReaderGeneration
        if clearVoWiFi || readerChanged || profileChanged || readerGenerationChanged {
            invalidateVoWiFiOperationForCardTransition()
            consecutiveVoWiFiStatusReadFailures = 0
        }
        readerSessionGeneration = nextReaderGeneration
        if nextReaderGeneration != nil { requiresReaderGeneration = true }
        commit { state in
            if let readers { state.readers = readers }
            if let listLoaded { state.listLoaded = listLoaded }
            state.connected = true
            state.name = response.status.readerName
            state.hasESIM = response.status.hasESIM || (retainCapabilities && state.hasESIM)
            state.cardType = retainCapabilities ? state.cardType : response.status.cardType
            state.usimInfo = response.usimInfo
            state.profileOperatorName = carrier.isEmpty ? nil : carrier
            state.epdgFQDN = response.epdgFqdn
            state.carrierConfigError = response.carrierConfigError
            state.error = nil
            if clearVoWiFi || readerChanged || profileChanged || readerGenerationChanged {
                state.voWiFiStatus = nil
                state.voWiFiLoaded = false
                state.voWiFiActionInFlight = false
            }
        }
    }

    func clearRuntimeState(
        clearList: Bool = false,
        keepingReaders: [String]? = nil,
        listLoaded: Bool? = nil
    ) {
        let targetReaders = clearList ? [] : (keepingReaders ?? state.readers)
        let targetListLoaded = clearList ? false : (listLoaded ?? state.listLoaded)
        let hadRuntimeState = connected || name != nil || usimInfo != nil ||
            profileOperatorName != nil || epdgFQDN != nil || hasESIM || cardType != nil ||
            actionInFlight || voWiFiStatus != nil || voWiFiLoaded ||
            voWiFiActionInFlight || voWiFiRefreshInFlight
        let listChanged = state.readers != targetReaders || state.listLoaded != targetListLoaded
        guard hadRuntimeState || clearList || listChanged else { return }
        if hadRuntimeState || clearList { generation &+= 1 }
        invalidateVoWiFiOperationForCardTransition()
        readerSessionGeneration = nil
        requiresReaderGeneration = false
        consecutiveVoWiFiStatusReadFailures = 0
        lastVoWiFiHealthRefreshAt = nil
        epdgError = nil
        commit { state in
            Self.clearDerivedFields(&state)
            state.readers = targetReaders
            state.listLoaded = targetListLoaded
        }
    }

    func updateProfileOperatorName(_ value: String?) {
        let normalized = value?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        commit { $0.profileOperatorName = normalized.isEmpty ? nil : normalized }
    }

    func synchronizeAfterProfileSwitch(_ snapshot: ReaderInfoResponse? = nil) {
        generation &+= 1
        let expectedGeneration = generation
        invalidateVoWiFiOperationForCardTransition()
        if let snapshot {
            applyInfo(snapshot, clearVoWiFi: true)
        } else {
            // Activation is being confirmed. Withdraw every field derived
            // from the old profile before I/O while retaining the same
            // physical reader's known capability for a stable page skeleton.
            commit { state in
                Self.clearCardIdentityFields(&state, preservingCapabilities: true)
                Self.clearVoWiFiFields(&state)
            }
        }
        let initialOperationRevision = voWiFiOperationRevision
        let initialStatusRevision = voWiFiStatusRevision
        Task {
            var expectedOperationRevision = initialOperationRevision
            var expectedStatusRevision = initialStatusRevision
            do {
                guard expectedGeneration == generation,
                      expectedOperationRevision == voWiFiOperationRevision,
                      expectedStatusRevision == voWiFiStatusRevision else { return }
                if snapshot == nil {
                    let info: ReaderInfoResponse = try await APIClient(timeoutInterval: 45)
                        .send("api/reader/refresh")
                    guard expectedGeneration == generation,
                          expectedOperationRevision == voWiFiOperationRevision,
                          expectedStatusRevision == voWiFiStatusRevision else { return }
                    applyInfo(info)
                    guard expectedGeneration == generation else { return }
                    // Applying the freshly read card may itself invalidate
                    // the prior VoWiFi operation revision. Bind the next GET
                    // to that new card identity, not to the old revision.
                    expectedOperationRevision = voWiFiOperationRevision
                    expectedStatusRevision = voWiFiStatusRevision
                }
                let status: ReaderVoWiFiStatus = try await APIClient(timeoutInterval: 15)
                    .get("api/reader/vowifi")
                guard expectedGeneration == generation,
                      expectedOperationRevision == voWiFiOperationRevision,
                      expectedStatusRevision == voWiFiStatusRevision else { return }
                publishDirectVoWiFiStatus(status)
            } catch {
                guard expectedGeneration == generation,
                      expectedOperationRevision == voWiFiOperationRevision,
                      expectedStatusRevision == voWiFiStatusRevision else { return }
                commit { state in
                    state.voWiFiLoaded = true
                    state.error = "切换后同步卡片信息失败：\(error.localizedDescription)"
                }
            }
        }
    }

    func refreshVoWiFi(force: Bool = false) {
        guard case .running = backend.state, !actionInFlight, !fallbackRecoveryInFlight else { return }
        if backend.usesRustGateway && !force && !voWiFiActionInFlight { return }
        guard !voWiFiRefreshInFlight else { return }
        let expectedGeneration = generation
        let expectedOperationRevision = voWiFiOperationRevision
        let expectedStatusRevision = voWiFiStatusRevision
        voWiFiRefreshInFlight = true
        Task {
            defer {
                if expectedGeneration == generation,
                   expectedOperationRevision == voWiFiOperationRevision,
                   expectedStatusRevision == voWiFiStatusRevision {
                    voWiFiRefreshInFlight = false
                }
            }
            do {
                let status: ReaderVoWiFiStatus = try await APIClient(timeoutInterval: 10)
                    .get("api/reader/vowifi")
                guard expectedGeneration == generation,
                      expectedOperationRevision == voWiFiOperationRevision,
                      expectedStatusRevision == voWiFiStatusRevision else { return }
                publishDirectVoWiFiStatus(status)
            } catch {
                guard expectedGeneration == generation,
                      expectedOperationRevision == voWiFiOperationRevision,
                      expectedStatusRevision == voWiFiStatusRevision else { return }
                recordVoWiFiStatusReadFailure()
            }
        }
    }

    func enableVoWiFi() {
        guard !actionInFlight else { return }
        guard !voWiFiActionInFlight else { return }
        if let proxyConfigurationError {
            toast(proxyConfigurationError, false, "Wi-Fi Calling")
            return
        }
        let expectedGeneration = generation
        voWiFiOperationRevision &+= 1
        invalidatePendingVoWiFiStatusReads()
        let operationRevision = voWiFiOperationRevision
        let epdg = customEPDG.trimmingCharacters(in: .whitespacesAndNewlines)
        epdgError = nil
        commit { $0.voWiFiActionInFlight = true }
        Task {
            do {
                let request = ReaderVoWiFiRequest(
                    enable: true,
                    proxy: proxy,
                    epdg: epdg.isEmpty ? nil : epdg,
                    epdgPort: epdgPortValue)
                _ = try await APIClient(timeoutInterval: 15)
                    .send("api/reader/vowifi", body: request, as: EmptyResponse.self)
                guard expectedGeneration == generation,
                      operationRevision == voWiFiOperationRevision else { return }
                toast("正在建立 Wi-Fi Calling 隧道...", true, "Wi-Fi Calling")
                pollVoWiFi(
                    expectedGeneration: expectedGeneration,
                    operationRevision: operationRevision)
            } catch {
                guard expectedGeneration == generation,
                      operationRevision == voWiFiOperationRevision else { return }
                commit { $0.voWiFiActionInFlight = false }
                toast("启用失败：\(error.localizedDescription)", false, "Wi-Fi Calling")
            }
        }
    }

    func disableVoWiFi() {
        guard !actionInFlight else { return }
        voWiFiPollTask?.cancel()
        voWiFiPollTask = nil
        voWiFiOperationRevision &+= 1
        invalidatePendingVoWiFiStatusReads()
        let operationRevision = voWiFiOperationRevision
        commit { state in
            state.voWiFiActionInFlight = true
            Self.clearVoWiFiSnapshotFields(&state)
        }
        let expectedGeneration = generation
        Task {
            do {
                _ = try await APIClient(timeoutInterval: 15).send(
                    "api/reader/vowifi",
                    body: ReaderVoWiFiRequest(enable: false, proxy: nil),
                    as: EmptyResponse.self)
                guard expectedGeneration == generation,
                      operationRevision == voWiFiOperationRevision else { return }
                toast("Wi-Fi Calling 已关闭", true, "Wi-Fi Calling")
            } catch {
                guard expectedGeneration == generation,
                      operationRevision == voWiFiOperationRevision else { return }
                toast("关闭失败：\(error.localizedDescription)", false, "Wi-Fi Calling")
            }
            commit { $0.voWiFiActionInFlight = false }
            refreshVoWiFi(force: true)
        }
    }

    private func pollVoWiFi(expectedGeneration: Int, operationRevision: UInt64) {
        voWiFiPollTask?.cancel()
        voWiFiPollTask = Task {
            try? await Task.sleep(for: .milliseconds(1_500))
            for _ in 0..<60 where !Task.isCancelled {
                do {
                    let expectedStatusRevision = voWiFiStatusRevision
                    let status: ReaderVoWiFiStatus = try await APIClient(timeoutInterval: 10)
                        .get("api/reader/vowifi")
                    guard expectedGeneration == generation,
                          operationRevision == voWiFiOperationRevision else { return }
                    guard expectedStatusRevision == voWiFiStatusRevision else { continue }
                    guard publishDirectVoWiFiStatus(status) else {
                        try? await Task.sleep(for: .milliseconds(1_500))
                        continue
                    }
                    if status.tunnelState == "connected", status.imsState == "registered" {
                        commit { $0.voWiFiActionInFlight = false }
                        let ready = status.smsReady == true || status.callReady == true
                        let message = status.smsReady == true && status.callReady == true
                            ? "Wi-Fi Calling 已就绪，SMS 与通话可用"
                            : status.callReady == true
                                ? "IMS 与通话已就绪，SMS 通道尚未就绪"
                                : "IMS 已注册，业务安全通道尚未就绪"
                        toast(message, ready, "Wi-Fi Calling")
                        return
                    }
                    if status.tunnelState == "error" || status.imsState == "failed" {
                        commit { $0.voWiFiActionInFlight = false }
                        toast(status.message ?? "IMS 或隧道建立失败", false, "Wi-Fi Calling")
                        return
                    }
                } catch {
                    // A short read failure is retried within the bounded startup window.
                }
                try? await Task.sleep(for: .milliseconds(1_500))
            }
            guard expectedGeneration == generation,
                  operationRevision == voWiFiOperationRevision else { return }
            commit { $0.voWiFiActionInFlight = false }
            toast("Wi-Fi Calling 启动超时，请检查网络和代理设置", false, "Wi-Fi Calling")
            refreshVoWiFi()
        }
    }

    func checkProxy() {
        proxyCheckTask?.cancel()
        proxyCheckRevision &+= 1
        let requestRevision = proxyCheckRevision
        guard let proxy else {
            proxyChecking = false
            let host = proxyIP.trimmingCharacters(in: .whitespacesAndNewlines)
            let port = proxyPort.trimmingCharacters(in: .whitespacesAndNewlines)
            let usesAutomaticSystemProxy = host.isEmpty && port.isEmpty
            proxyCheckResult = ProxyCheckResult(
                ok: usesAutomaticSystemProxy,
                probeMessage: usesAutomaticSystemProxy ? "未手动填写前置代理；启动时将自动检测 macOS SOCKS / Surge" : nil,
                countryCode: nil, country: nil,
                exitIP: nil, latencyMs: nil, udpReady: nil, transport: nil,
                udpMessage: usesAutomaticSystemProxy ? "HTTP/PAC 不会被当作 UDP 代理；未发现可验证 SOCKS 时才按当前网络选路" : nil,
                error: usesAutomaticSystemProxy ? nil : proxyConfigurationError)
            return
        }
        proxyChecking = true
        proxyCheckTask = Task {
            defer {
                if requestRevision == proxyCheckRevision { proxyChecking = false }
            }
            do {
                let custom = customEPDG.trimmingCharacters(in: .whitespacesAndNewlines)
                let effectiveEPDG = custom.isEmpty ? (voWiFiStatus?.epdgFqdn ?? epdgFQDN) : custom
                let request = ProxyCheckRequest(proxy: proxy, epdg: effectiveEPDG)
                let result: ProxyCheckResult = try await APIClient(timeoutInterval: 12)
                    .send("api/reader/vowifi/proxy-check", body: request)
                guard requestRevision == proxyCheckRevision else { return }
                proxyCheckResult = result
            } catch {
                guard requestRevision == proxyCheckRevision else { return }
                proxyCheckResult = ProxyCheckResult(
                    ok: false, probeMessage: nil, countryCode: nil, country: nil,
                    exitIP: nil, latencyMs: nil, udpReady: nil, transport: nil,
                    udpMessage: nil, error: "检测失败：\(error.localizedDescription)")
            }
        }
    }

    func saveEPDGConfig() {
        let epdg = customEPDG.trimmingCharacters(in: .whitespacesAndNewlines)
        let port = customEPDGPort.trimmingCharacters(in: .whitespacesAndNewlines)
        if !port.isEmpty && epdgPortValue == nil {
            epdgError = "端口无效（1-65535）"
            return
        }
        if !epdg.isEmpty && !Self.isValidEPDG(epdg) {
            epdgError = "请输入合法 IP 或域名"
            return
        }
        epdgError = nil
        toast(epdg.isEmpty ? "已使用运营商模板自动选择 ePDG" : "自定义 ePDG 配置已保存", true, "Wi-Fi Calling")
    }

    private func commit(_ update: (inout ReaderRuntimeState) -> Void) {
        var next = state
        update(&next)
        next.revision = state.revision &+ 1
        state = next
    }

    private func setActionInFlight(_ value: Bool) {
        commit { $0.actionInFlight = value }
    }

    private func setError(_ message: String?) {
        commit { $0.error = message }
    }

    private func invalidateVoWiFiOperationForCardTransition() {
        voWiFiPollTask?.cancel()
        voWiFiPollTask = nil
        voWiFiOperationRevision &+= 1
        invalidatePendingVoWiFiStatusReads()
        lastVoWiFiHealthRefreshAt = nil
    }

    private func invalidatePendingVoWiFiStatusReads() {
        voWiFiStatusRevision &+= 1
        voWiFiRefreshInFlight = false
    }

    private func publishVoWiFiStatus(_ status: ReaderVoWiFiStatus) {
        invalidatePendingVoWiFiStatusReads()
        consecutiveVoWiFiStatusReadFailures = 0
        commit { state in
            state.voWiFiStatus = status
            state.voWiFiLoaded = true
        }
    }

    @discardableResult
    private func publishDirectVoWiFiStatus(_ status: ReaderVoWiFiStatus) -> Bool {
        if status.tunnelState == "disconnected",
           (status.imsState == nil || status.imsState == "idle"),
           status.smsReady != true,
           status.callReady != true {
            // A keyless idle result may always withdraw readiness.
            publishVoWiFiStatus(status)
            return true
        }
        let currentReader = name?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        let iccid = usimInfo?.iccid.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        let imsi = usimInfo?.imsi.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        let currentSIM = iccid.isEmpty ? imsi : iccid
        guard connected, !currentReader.isEmpty, !currentSIM.isEmpty,
              status.readerID?.trimmingCharacters(in: .whitespacesAndNewlines) == currentReader,
              status.simID?.trimmingCharacters(in: .whitespacesAndNewlines) == currentSIM,
              (!requiresReaderGeneration || (
                readerSessionGeneration != nil &&
                status.readerGeneration == readerSessionGeneration)) else {
            invalidatePendingVoWiFiStatusReads()
            commit { state in
                Self.clearVoWiFiSnapshotFields(&state)
                state.voWiFiLoaded = true
                state.error = "VoWiFi 状态与当前读卡器或 SIM 不匹配，已拒绝显示就绪"
            }
            return false
        }
        publishVoWiFiStatus(status)
        return true
    }

    private static func clearCardDerivedFields(_ state: inout ReaderRuntimeState) {
        clearCardIdentityFields(&state)
        clearVoWiFiFields(&state)
    }

    private static func clearCardIdentityFields(
        _ state: inout ReaderRuntimeState,
        preservingCapabilities: Bool = false
    ) {
        state.usimInfo = nil
        state.profileOperatorName = nil
        state.epdgFQDN = nil
        state.carrierConfigError = nil
        if !preservingCapabilities {
            state.hasESIM = false
            state.cardType = nil
        }
        state.error = nil
    }

    private static func clearVoWiFiFields(_ state: inout ReaderRuntimeState) {
        state.voWiFiStatus = nil
        state.voWiFiLoaded = false
        state.voWiFiActionInFlight = false
    }

    /// A direct status request has no trustworthy replacement value on
    /// failure. Two consecutive failures are enough to make the previous
    /// ready status stale, while one failure remains tolerant of a brief
    /// backend restart or transient local socket error.
    private func recordVoWiFiStatusReadFailure() {
        consecutiveVoWiFiStatusReadFailures &+= 1
        if consecutiveVoWiFiStatusReadFailures >= 2, !voWiFiActionInFlight {
            invalidatePendingVoWiFiStatusReads()
        }
        commit { state in
            state.voWiFiLoaded = true
            guard consecutiveVoWiFiStatusReadFailures >= 2,
                  !state.voWiFiActionInFlight else { return }
            state.voWiFiStatus = nil
        }
    }

    /// Clears only data that came from a failed aggregate snapshot. In-flight
    /// enable/disable operations own their own completion and must not be
    /// cancelled or visually reset by an unrelated reader snapshot failure.
    private static func clearVoWiFiSnapshotFields(_ state: inout ReaderRuntimeState) {
        state.voWiFiStatus = nil
        state.voWiFiLoaded = false
    }

    private static func clearDerivedFields(_ state: inout ReaderRuntimeState) {
        state.connected = false
        state.name = nil
        state.actionInFlight = false
        clearCardDerivedFields(&state)
    }

    private static func isValidEPDG(_ value: String) -> Bool {
        let unwrapped = value.trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        let ipv4Parts = unwrapped.split(separator: ".", omittingEmptySubsequences: false)
        let isIPv4 = ipv4Parts.count == 4 && ipv4Parts.allSatisfy {
            guard let value = Int($0) else { return false }
            return (0...255).contains(value)
        }
        let isIPv6 = unwrapped.contains(":") && unwrapped.allSatisfy {
            $0.isHexDigit || $0 == ":" || $0 == "."
        }
        let labels = unwrapped.split(separator: ".", omittingEmptySubsequences: false)
        let isFQDN = labels.count > 1 && labels.allSatisfy { label in
            !label.isEmpty && label.count <= 63 && label.first != "-" && label.last != "-"
                && label.allSatisfy { $0.isLetter || $0.isNumber || $0 == "-" }
        }
        return isIPv4 || isIPv6 || isFQDN
    }

    private func toast(_ message: String, _ success: Bool, _ title: String?) {
        onToast?(message, success, title)
    }

    private struct EmptyResponse: Decodable {}
}
