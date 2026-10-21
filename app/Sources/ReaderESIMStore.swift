import Combine
import Foundation

/// 读卡器 eSIM Profile 管理 Store
///
/// - 所有 API 调用走 `/api/reader/esim/*`（PC/SC 读卡器通道）
/// - Profile 切换后不重启模块、不监控模块 ICCID 变化
/// - 复用 ESIMOverview/ProfileItem/ESIMOperationSnapshot 等模型（后端响应格式相同）
@MainActor
final class ReaderESIMStore: ObservableObject {
    struct ReaderContext: Equatable {
        let name: String?
        let connected: Bool
        let hasESIM: Bool
    }

    enum LoadPhase: Equatable {
        case idle
        case loading
        case loaded
        case failed(String)
    }

    enum Action: Equatable {
        case switching(String)
        case deleting(String)
        case renaming(String)
        case downloading
    }

    @Published private(set) var overview: ESIMOverview?
    @Published private(set) var loadPhase: LoadPhase = .idle
    @Published private(set) var isRefreshing = false
    @Published private(set) var action: Action?
    @Published private(set) var operation: ESIMOperationSnapshot?
    @Published private(set) var profileNotes: [String: ProfileNote] = [:]
    @Published private(set) var notifications: [ESIMNotificationItem] = []
    @Published private(set) var isLoadingNotifications = false
    @Published private(set) var retryingNotificationID: String?
    @Published private(set) var notificationError: String?
    @Published var banner: ESIMBanner?

    private var loadTask: Task<Void, Never>?
    private var notesTask: Task<Void, Never>?
    private var actionTask: Task<Void, Never>?
    private var notificationTask: Task<Void, Never>?
    private var retryNotificationTask: Task<Void, Never>?
    private var operationPollTask: Task<Void, Never>?
    private var terminalRefreshTask: Task<Void, Never>?
    private var loadGeneration = 0
    private var sessionGeneration: UInt64 = 0
    private var lastHandledOperationID: String?
    private var synchronizedSwitchOperationID: String?
    private var unconfirmedSwitchICCID: String?
    private var readerContext: ReaderContext?
    private var loadErrorBannerID: UUID?

    deinit {
        loadTask?.cancel()
        notesTask?.cancel()
        actionTask?.cancel()
        notificationTask?.cancel()
        retryNotificationTask?.cancel()
        operationPollTask?.cancel()
        terminalRefreshTask?.cancel()
    }

    var capabilities: ESIMCapabilities? { overview?.capabilities }
    var isPhysicalSIM: Bool { overview?.cardType == "physical_sim" }
    var isInitialLoading: Bool { loadPhase == .loading && overview == nil }
    var isBusy: Bool {
        isRefreshing || action != nil || operation?.isActive == true ||
            isLoadingNotifications || retryingNotificationID != nil
    }

    var profileGroups: [(group: EUICCProfiles, profiles: [ESIMProfileContext])] {
        (overview?.profiles ?? []).map { group in
            let eid = group.eid ?? ""
            let aid = group.aidHex ?? ""
            return (group, (group.profiles ?? []).map {
                ESIMProfileContext(eid: eid, aidHex: aid, profile: $0)
            })
        }
    }

    var profileCount: Int {
        profileGroups.reduce(0) { $0 + $1.profiles.count }
    }

    func savedPhoneNumber(for iccid: String) -> String? {
        let target = normalizedICCID(iccid)
        guard !target.isEmpty else { return nil }
        return usableSIMPhoneNumber(profileNotes.first(where: { normalizedICCID($0.key) == target })?.value.phone)
    }

    private func normalizedICCID(_ value: String) -> String {
        value.filter(\.isNumber)
    }

    // MARK: - API 路径前缀（读卡器 eSIM 专用）

    private let apiPrefix = "api/reader/esim"

    // MARK: - 加载概览

    /// A USB/BLE selection can change without either readiness flag turning
    /// false. Invalidate the old reader before starting any new overview work.
    /// Repeated status polls with the same context do not re-read the eUICC.
    @discardableResult
    func synchronizeReader(_ context: ReaderContext) -> Bool {
        let previous = readerContext
        guard previous != context else { return false }
        let invalidated = previous.map {
            $0.name != context.name || ($0.connected && !context.connected)
        } ?? false
        if invalidated { clearForReaderDisconnect() }
        readerContext = context
        let hasSelectedReader = !(context.name?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ?? true)
        if context.connected && context.hasESIM && hasSelectedReader &&
            overview == nil && loadTask == nil && action == nil && operation?.isActive != true {
            load()
        }
        return invalidated
    }

    func load(forceRefresh: Bool = false) {
        loadTask?.cancel()
        loadGeneration &+= 1
        let generation = loadGeneration
        if forceRefresh {
            isRefreshing = true
        } else if overview == nil {
            loadPhase = .loading
        }

        loadTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.loadGeneration {
                    self.isRefreshing = false
                    self.loadTask = nil
                }
            }
            do {
                let path = forceRefresh ? "\(apiPrefix)?refresh=true" : apiPrefix
                let fetched: ESIMOverview
                do {
                    fetched = try await APIClient(timeoutInterval: 45).get(path)
                } catch {
                    // A CCID reader may need a brief moment after its PC/SC
                    // power cycle. Retry once before showing a false failure.
                    guard forceRefresh, !Task.isCancelled else { throw error }
                    try await Task.sleep(nanoseconds: 800_000_000)
                    fetched = try await APIClient(timeoutInterval: 45).get(path)
                }
                guard !Task.isCancelled, generation == loadGeneration else { return }
                overview = fetched
                publishActiveProfileOperator(from: fetched)
                loadProfileNotes(generation: generation)
                operation = fetched.operation
                loadPhase = .loaded
                if let loadErrorBannerID, banner?.id == loadErrorBannerID {
                    banner = nil
                }
                loadErrorBannerID = nil
                if let activeOperation = fetched.operation, activeOperation.isActive {
                    trackOperation(activeOperation)
                }
            } catch {
                guard !Task.isCancelled, generation == loadGeneration else { return }
                if overview == nil {
                    loadPhase = .failed(error.localizedDescription)
                } else {
                    let loadError = ESIMBanner(
                        tone: .error,
                        message: forceRefresh ? "刷新读卡器卡片信息失败" : "更新读卡器卡片信息失败",
                        detail: error.localizedDescription)
                    banner = loadError
                    loadErrorBannerID = loadError.id
                }
            }
        }
    }

    private func loadProfileNotes(generation: Int) {
        notesTask?.cancel()
        notesTask = Task { [weak self] in
            guard let self else { return }
            let result: NotesResponse? = try? await APIClient(timeoutInterval: 8).get("api/esim/notes")
            guard !Task.isCancelled, generation == loadGeneration else { return }
            profileNotes = result?.notes ?? [:]
            notesTask = nil
        }
    }

    func refresh() {
        guard capabilities?.canRefresh == true, !isBusy else { return }
        banner = nil
        load(forceRefresh: true)
    }

    /// Invalidates every reader-backed eSIM snapshot after removal or a
    /// USB/BLE reader selection change. User-entered settings live elsewhere
    /// and are intentionally not touched.
    func clearForReaderDisconnect() {
        loadTask?.cancel()
        notesTask?.cancel()
        actionTask?.cancel()
        notificationTask?.cancel()
        retryNotificationTask?.cancel()
        operationPollTask?.cancel()
        terminalRefreshTask?.cancel()
        loadTask = nil
        notesTask = nil
        actionTask = nil
        notificationTask = nil
        retryNotificationTask = nil
        operationPollTask = nil
        terminalRefreshTask = nil
        loadGeneration &+= 1
        sessionGeneration &+= 1
        overview = nil
        profileNotes = [:]
        loadPhase = .idle
        isRefreshing = false
        action = nil
        operation = nil
        banner = nil
        loadErrorBannerID = nil
        lastHandledOperationID = nil
        synchronizedSwitchOperationID = nil
        unconfirmedSwitchICCID = nil
        notifications = []
        isLoadingNotifications = false
        retryingNotificationID = nil
        notificationError = nil
    }

    // MARK: - 运营商 Profile 通知

    func loadNotifications() {
        guard !isLoadingNotifications, retryingNotificationID == nil, action == nil else { return }
        let generation = sessionGeneration
        isLoadingNotifications = true
        notificationError = nil
        notificationTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.sessionGeneration {
                    self.isLoadingNotifications = false
                    self.notificationTask = nil
                }
            }
            do {
                let response: ESIMNotificationListResponse = try await APIClient(timeoutInterval: 45).get(
                    "\(apiPrefix)/notifications")
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                notifications = response.notifications
            } catch {
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                notificationError = error.localizedDescription
            }
        }
    }

    func retryNotification(_ item: ESIMNotificationItem) {
        guard !isLoadingNotifications, retryingNotificationID == nil, action == nil, item.canRetry else { return }
        let generation = sessionGeneration
        retryingNotificationID = item.id
        notificationError = nil
        retryNotificationTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.sessionGeneration {
                    self.retryingNotificationID = nil
                    self.retryNotificationTask = nil
                }
            }
            do {
                let _: MessageResponse = try await APIClient(timeoutInterval: 45).send(
                    "\(apiPrefix)/notifications/retry",
                    body: ESIMNotificationRetryRequest(
                        sequenceNumber: item.sequenceNumber,
                        aid: item.aidHex))
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                notifications.removeAll { $0.id == item.id }
                banner = ESIMBanner(
                    tone: .success,
                    message: "运营商通知已确认",
                    detail: nil)
            } catch {
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                notificationError = error.localizedDescription
            }
        }
    }

    // MARK: - Profile 切换

    /// A completed operation may still own a delayed overview refresh. A new
    /// user mutation supersedes that read (and any older metadata load), not
    /// the cached list itself. Cancelled replies remain fenced by loadGeneration.
    private func beginProfileAction(_ next: Action) {
        terminalRefreshTask?.cancel()
        terminalRefreshTask = nil
        operationPollTask?.cancel()
        operationPollTask = nil
        loadTask?.cancel()
        loadTask = nil
        notesTask?.cancel()
        notesTask = nil
        loadGeneration &+= 1
        isRefreshing = false
        operation = nil
        action = next
        banner = nil
    }

    func switchProfile(_ context: ESIMProfileContext) {
        guard capabilities?.canSwitch == true, !isBusy, !context.profile.isEnabled else { return }
        let generation = sessionGeneration
        beginProfileAction(.switching(context.id))
        actionTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.sessionGeneration { self.actionTask = nil }
            }
            do {
                let result: ESIMSwitchResult = try await APIClient(timeoutInterval: 130).send(
                    "\(apiPrefix)/switch",
                    body: ESIMSwitchRequest(iccid: context.profile.iccid, aid: context.aidHex))
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                action = nil
                // An accepted switch may still report the old USIM identity.
                // Profile enabled-state alone does not prove activation on the
                // reader; only publish the requested operator after that read.
                let target = normalizedICCID(context.profile.iccid)
                let actual = normalizedICCID(result.readerInfo?.usimInfo?.iccid ?? "")
                let identityConfirmed = !target.isEmpty && actual == target
                unconfirmedSwitchICCID = identityConfirmed ? nil : target
                if identityConfirmed {
                    let carrierName = context.profile.serviceProviderName ?? context.profile.name
                    DashboardStore.shared?.updateReaderProfileOperatorName(carrierName)
                }
                DashboardStore.shared?.synchronizeReaderAfterProfileSwitch(result.readerInfo)
                if let op = result.operation {
                    synchronizedSwitchOperationID = op.id
                    trackOperation(op)
                } else {
                    banner = ESIMBanner(
                        tone: identityConfirmed ? .success : .warning,
                        message: identityConfirmed ? "Profile 切换完成" : "Profile 切换已受理，激活结果待确认",
                        detail: nil)
                    load(forceRefresh: true)
                }
            } catch {
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                action = nil
                if operationOutcomeIsUnknown(error) {
                    // A lost write response cannot prove either cancellation
                    // or USIM activation. Retain the same identity gate used
                    // for an explicitly accepted, unconfirmed switch.
                    unconfirmedSwitchICCID = normalizedICCID(context.profile.iccid)
                }
                banner = ESIMBanner(tone: .error,
                                    message: operationOutcomeIsUnknown(error) ? "切换 Profile 结果待确认" : "切换 Profile 失败",
                                    detail: error.localizedDescription)
            }
        }
    }

    // MARK: - Profile 删除

    func deleteProfile(_ context: ESIMProfileContext) {
        guard capabilities?.canDelete == true, !isBusy, !context.profile.isEnabled else { return }
        let generation = sessionGeneration
        beginProfileAction(.deleting(context.id))
        actionTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.sessionGeneration {
                    self.action = nil
                    self.actionTask = nil
                }
            }
            do {
                let result: ESIMProfileResult = try await APIClient(timeoutInterval: 90).send(
                    "\(apiPrefix)/profile", method: "DELETE",
                    body: ESIMDeleteRequest(iccid: context.profile.iccid, aid: context.aidHex))
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                if let warning = result.warning, !warning.isEmpty {
                    banner = ESIMBanner(tone: .warning, message: "Profile 已删除", detail: warning)
                } else {
                    banner = ESIMBanner(tone: .success, message: "Profile 已删除", detail: nil)
                }
                load(forceRefresh: true)
            } catch {
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                banner = ESIMBanner(tone: .error,
                                    message: operationOutcomeIsUnknown(error) ? "删除 Profile 结果待确认" : "删除 Profile 失败",
                                    detail: error.localizedDescription)
            }
        }
    }

    // MARK: - Profile 重命名

    func renameProfile(_ context: ESIMProfileContext, newName: String) {
        guard capabilities?.canRename == true, !isBusy else { return }
        let generation = sessionGeneration
        beginProfileAction(.renaming(context.id))
        actionTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.sessionGeneration {
                    self.action = nil
                    self.actionTask = nil
                }
            }
            do {
                let _: MessageResponse = try await APIClient(timeoutInterval: 30).send(
                    "\(apiPrefix)/profile", method: "PATCH",
                    body: ESIMRenameRequest(iccid: context.profile.iccid, aid: context.aidHex, name: newName))
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                banner = ESIMBanner(tone: .success, message: "Profile 名称修改成功", detail: nil)
                load(forceRefresh: true)
            } catch {
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                banner = ESIMBanner(tone: .error,
                                    message: operationOutcomeIsUnknown(error) ? "修改名称结果待确认" : "修改名称失败",
                                    detail: error.localizedDescription)
            }
        }
    }

    // MARK: - Profile 下载

    func startDownload(_ request: ESIMDownloadRequest) {
        guard capabilities?.canDownload == true, !isBusy else { return }
        let generation = sessionGeneration
        beginProfileAction(.downloading)
        actionTask = Task { [weak self] in
            guard let self else { return }
            defer {
                if generation == self.sessionGeneration { self.actionTask = nil }
            }
            do {
                let started: ESIMOperationSnapshot = try await APIClient().send(
                    "\(apiPrefix)/download", body: request)
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                trackOperation(started)
            } catch {
                guard !Task.isCancelled, generation == sessionGeneration else { return }
                action = nil
                banner = ESIMBanner(tone: .error,
                                    message: operationOutcomeIsUnknown(error) ? "下载 Profile 结果待确认" : "无法开始下载 Profile",
                                    detail: error.localizedDescription)
            }
        }
    }

    func confirmDownload(operationID: String, confirm: Bool) {
        guard operation?.id == operationID, operation?.awaitsDownloadConfirmation == true else { return }
        let generation = sessionGeneration
        Task { [weak self] in
            guard let self else { return }
            do {
                let updated: ESIMOperationSnapshot = try await APIClient(timeoutInterval: 10).send(
                    "\(apiPrefix)/download/confirm",
                    body: ESIMDownloadConfirmationRequest(operationID: operationID, confirm: confirm))
                guard generation == sessionGeneration, updated.id == operationID else { return }
                operation = updated
            } catch {
                guard generation == sessionGeneration else { return }
                banner = ESIMBanner(tone: .error, message: "无法提交下载确认", detail: error.localizedDescription)
            }
        }
    }

    // MARK: - 操作跟踪

    func dismissBanner() {
        banner = nil
    }

    func isProfileActionRunning(_ context: ESIMProfileContext) -> Bool {
        switch action {
        case .switching(let id), .deleting(let id), .renaming(let id): return id == context.id
        default: return false
        }
    }

    func reloadAfterMetadataChange() {
        load()
    }

    // MARK: - 异步操作轮询

    private func trackOperation(_ snapshot: ESIMOperationSnapshot) {
        let generation = sessionGeneration
        operation = snapshot
        if snapshot.kind == "download_profile" && snapshot.isActive {
            action = .downloading
        }
        operationPollTask?.cancel()
        if !snapshot.isActive {
            handleTerminalOperation(snapshot)
            return
        }

        operationPollTask = Task { [weak self] in
            guard let self else { return }
            var consecutiveFailures = 0
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 1_000_000_000)
                guard !Task.isCancelled else { return }
                do {
                    let response: ESIMOperationResponse = try await APIClient(timeoutInterval: 10).get("\(apiPrefix)/operation")
                    guard !Task.isCancelled, generation == sessionGeneration else { return }
                    consecutiveFailures = 0
                    guard let current = response.operation else {
                        operationPollTask = nil
                        operation = nil
                        action = nil
                        banner = ESIMBanner(
                            tone: .warning,
                            message: "无法继续跟踪 eSIM 操作",
                            detail: "后端操作状态已重置，请刷新卡片确认最终结果")
                        return
                    }
                    guard current.id == snapshot.id else {
                        trackOperation(current)
                        return
                    }
                    operation = current
                    if !current.isActive {
                        operationPollTask = nil
                        handleTerminalOperation(current)
                        return
                    }
                } catch {
                    guard !Task.isCancelled, generation == sessionGeneration else { return }
                    consecutiveFailures += 1
                    if consecutiveFailures == 8 {
                        banner = ESIMBanner(
                            tone: .warning,
                            message: "仍在等待 eSIM 操作状态",
                            detail: "请稍后刷新卡片确认")
                    }
                    if consecutiveFailures >= 30 {
                        operationPollTask = nil
                        operation = nil
                        action = nil
                        banner = ESIMBanner(
                            tone: .error,
                            message: "eSIM 操作状态读取超时",
                            detail: "已停止后台轮询，请确认本机服务后刷新卡片状态")
                        return
                    }
                }
            }
        }
    }

    private func handleTerminalOperation(_ snapshot: ESIMOperationSnapshot) {
        action = nil
        guard lastHandledOperationID != snapshot.id else { return }
        lastHandledOperationID = snapshot.id

        if snapshot.isFailed {
            banner = ESIMBanner(
                tone: .error,
                message: operationTitle(snapshot.kind, success: false),
                detail: snapshot.error ?? "操作未完成")
            return
        }
        if snapshot.hasWarning {
            banner = ESIMBanner(
                tone: .warning,
                message: snapshot.message ?? operationTitle(snapshot.kind, success: true),
                detail: snapshot.result?.warning)
        } else if snapshot.isCancelled {
            banner = ESIMBanner(tone: .info, message: snapshot.message ?? "已取消下载", detail: nil)
        } else {
            banner = ESIMBanner(
                tone: .success,
                message: snapshot.message ?? operationTitle(snapshot.kind, success: true),
                detail: nil)
        }

        let delay = max(0, snapshot.refreshAfterSeconds ?? 0)
        terminalRefreshTask?.cancel()
        let generation = sessionGeneration
        terminalRefreshTask = Task { [weak self] in
            if delay > 0 {
                try? await Task.sleep(nanoseconds: UInt64(delay) * 1_000_000_000)
            }
            guard let self, !Task.isCancelled, generation == self.sessionGeneration,
                  self.operation?.id == snapshot.id, self.action == nil else { return }
            defer { self.terminalRefreshTask = nil }
            if snapshot.kind == "switch_profile" {
                self.operation = nil
                let alreadySynchronized = self.synchronizedSwitchOperationID == snapshot.id
                if !alreadySynchronized {
                    DashboardStore.shared?.synchronizeReaderAfterProfileSwitch()
                }
                self.load(forceRefresh: alreadySynchronized)
            } else {
                self.load(forceRefresh: true)
            }
        }
    }

    private func publishActiveProfileOperator(from overview: ESIMOverview) {
        let active = (overview.profiles ?? [])
            .flatMap { $0.profiles ?? [] }
            .first(where: { $0.isEnabled })
        if let pending = unconfirmedSwitchICCID {
            let actual = normalizedICCID(DashboardStore.shared?.readerUSIMInfo?.iccid ?? "")
            guard !pending.isEmpty, actual == pending,
                  normalizedICCID(active?.iccid ?? "") == pending else { return }
            unconfirmedSwitchICCID = nil
        }
        let name = active?.serviceProviderName ?? active?.name
        DashboardStore.shared?.updateReaderProfileOperatorName(name)
    }

    private func operationTitle(_ kind: String, success: Bool) -> String {
        switch kind {
        case "download_profile": return success ? "Profile 下载完成" : "Profile 下载失败"
        case "switch_profile": return success ? "Profile 切换完成" : "Profile 切换失败"
        default: return success ? "eSIM 操作完成" : "eSIM 操作失败"
        }
    }
}
