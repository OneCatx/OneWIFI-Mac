import AppKit
import Combine
import Foundation
import UserNotifications

/// Owned for the lifetime of the application, independently of the selected
/// page. No legacy SMS device selection or SMSStore state is read or changed.
@MainActor
final class MultiStandbyStore: ObservableObject {
    static let shared = MultiStandbyStore(backend: BackendProcess.shared)
    typealias NotificationSink = (MultiStandbyMessage, Bool) async throws -> Void

    @Published private(set) var cards: [MultiStandbyCard] = []
    @Published private(set) var profiles: [MultiStandbyProfile] = []
    @Published private(set) var messages: [MultiStandbyMessage] = []
    @Published private(set) var selectedCardID = ""
    @Published private(set) var selectedProfileID = ""
    @Published private(set) var selectedMessageID: String?
    @Published private(set) var isServiceRunning = false
    @Published private(set) var refreshing = false
    @Published private(set) var controlling = false
    @Published private(set) var sending = false
    @Published private(set) var lastError: String?
    @Published private(set) var actionError: String?
    @Published private(set) var sendNotice: String?
    @Published private(set) var operation: MultiStandbyOperation?
    @Published private(set) var messageCursor: String?
    @Published var viewingPage = false
    @Published var viewingInbox = false
    @Published private(set) var shouldOpenInbox = false
    @Published var notificationsEnabled: Bool {
        didSet { defaults.set(notificationsEnabled, forKey: Self.notificationsKey) }
    }

    private static let cursorKey = "switch.multistandby.notificationCursor"
    private static let seededKey = "switch.multistandby.notificationSeeded"
    private static let notifiedKey = "switch.multistandby.notifiedMessages"
    private static let notificationsKey = "switch.multistandby.notificationsEnabled"
    private let api: any MultiStandbyAPI
    private let defaults: UserDefaults
    private let notificationSink: NotificationSink
    private var cancellables = Set<AnyCancellable>()
    private var timer: Timer?
    private var pollTask: Task<Void, Never>?
    private var lifecycle: UInt64 = 0
    private var profileRequest: UInt64 = 0
    private var messageRequest: UInt64 = 0
    private var profileRevision: UInt64 = 0
    private var messageRevision: UInt64 = 0
    private var hasLoadedMessages = false
    private var bootID: String?
    private var changesCursor: String?
    private var notificationsSeeded: Bool
    private var notifiedIDs: [String]
    private struct SendKey: Hashable {
        let profileID: String
        let phone: String
        let body: String
    }
    private var pendingSends: [SendKey: MultiStandbySendRequest] = [:]
    private var changesInFlight = false

    init(backend: BackendProcess? = nil, api: (any MultiStandbyAPI)? = nil,
         defaults: UserDefaults = .standard, notificationSink: NotificationSink? = nil) {
        self.api = api ?? MultiStandbyHTTPAPI()
        self.defaults = defaults
        self.notificationSink = notificationSink ?? Self.postNotification
        changesCursor = defaults.string(forKey: Self.cursorKey)
        notificationsSeeded = defaults.bool(forKey: Self.seededKey)
        notifiedIDs = defaults.stringArray(forKey: Self.notifiedKey) ?? []
        notificationsEnabled = defaults.object(forKey: Self.notificationsKey) as? Bool ?? true
        backend?.$state
            .receive(on: RunLoop.main)
            .sink { [weak self] state in self?.setServiceRunning(state == .running) }
            .store(in: &cancellables)
    }

    deinit { timer?.invalidate(); pollTask?.cancel() }

    var selectedProfile: MultiStandbyProfile? { profiles.first { $0.profileID == selectedProfileID } }
    var onlineCount: Int { isServiceRunning ? profiles.filter(\.isReady).count : 0 }
    var canSend: Bool { isServiceRunning && selectedProfile?.isReady == true && !sending }
    var selectedCardAvailable: Bool { cards.contains { $0.cardID == selectedCardID && $0.available } }

    func setServiceRunning(_ running: Bool, schedulePolling: Bool = true) {
        guard isServiceRunning != running else { return }
        isServiceRunning = running
        lifecycle &+= 1
        profileRequest &+= 1
        messageRequest &+= 1
        pollTask?.cancel()
        pollTask = nil
        refreshing = false
        timer?.invalidate()
        timer = nil
        if running && schedulePolling {
            timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
                Task { @MainActor [weak self] in self?.refresh() }
            }
            refresh()
        }
    }

    func refresh() {
        guard isServiceRunning, pollTask == nil else { return }
        pollTask = Task { [weak self] in
            guard let self else { return }
            let epoch = self.lifecycle
            await self.refreshNow()
            if self.lifecycle == epoch { self.pollTask = nil }
        }
    }

    func refreshNow() async {
        guard isServiceRunning, !refreshing else { return }
        refreshing = true
        let epoch = lifecycle
        defer { if epoch == lifecycle { refreshing = false } }
        lastError = nil
        // Inventory can perform card I/O. Background notifications only need
        // durable snapshots and changes, so leave the legacy reader untouched
        // when this page is not in use.
        if viewingPage {
            do {
                let result = try await api.cards()
                guard current(epoch) else { return }
                cards = result.cards
                if let detail = result.detail, !detail.isEmpty { lastError = detail }
            } catch {
                guard current(epoch) else { return }
                lastError = userFacingErrorMessage(error)
            }
        }
        await refreshProfiles()
        guard current(epoch) else { return }
        if viewingInbox, !selectedProfileID.isEmpty { await refreshMessages() }
        guard current(epoch) else { return }
        if let operation, !operation.isFinished {
            do {
                let result = try await api.operation(id: operation.operationID)
                guard current(epoch), self.operation?.operationID == result.operationID else { return }
                self.operation = result
            } catch {
                if current(epoch) { lastError = userFacingErrorMessage(error) }
            }
        }
        await pollChanges()
    }

    func selectCard(_ id: String) {
        guard selectedCardID != id else { return }
        selectedCardID = id
        selectedProfileID = ""
        selectedMessageID = nil
        profiles = []
        messages = []
        messageCursor = nil
        profileRevision = 0
        messageRevision = 0
        hasLoadedMessages = false
        profileRequest &+= 1
        messageRequest &+= 1
        Task { await refreshProfiles() }
    }

    func selectProfile(_ id: String) {
        guard selectedProfileID != id else { return }
        selectedProfileID = id
        selectedMessageID = nil
        messages = []
        messageCursor = nil
        messageRevision = 0
        hasLoadedMessages = false
        messageRequest &+= 1
        sendNotice = nil
        // An uncertain intent remains bound to its original profile/payload.
        Task { await refreshMessages() }
    }

    func requestOpenMessage(profileID: String, messageID: String) {
        selectCard("")
        selectProfile(profileID)
        selectedMessageID = messageID
        Task {
            await refreshProfiles()
            await refreshMessages()
            let epoch = lifecycle
            let change = MultiStandbyChange(revision: 0, profileID: profileID, messageID: messageID,
                                           direction: nil, kind: nil, state: nil, deleted: nil)
            if let message = try? await findMessage(change), current(epoch), selectedProfileID == profileID {
                mergeMessages([message])
            }
        }
    }

    func consumeOpenMessageRequest() {
        selectedMessageID = nil
    }

    func requestSMSInbox() {
        shouldOpenInbox = true
    }

    func consumeSMSInboxRequest() {
        shouldOpenInbox = false
    }

    func refreshProfiles() async {
        guard isServiceRunning else { return }
        let epoch = lifecycle
        let card = selectedCardID
        profileRequest &+= 1
        let request = profileRequest
        do {
            let result = try await api.profiles(cardID: card)
            guard current(epoch), request == profileRequest, card == selectedCardID else { return }
            if bootID == result.bootID && result.revision < profileRevision { return }
            bootID = result.bootID
            profileRevision = result.revision
            profiles = result.profiles.filter { card.isEmpty || $0.cardID == card }
            // Preserve the selected ID even when unavailable; never fall back
            // to a different sender because a status response removed it.
        } catch {
            if current(epoch), request == profileRequest { lastError = userFacingErrorMessage(error) }
        }
    }

    func refreshMessages(loadMore: Bool = false) async {
        guard isServiceRunning, !selectedProfileID.isEmpty else { return }
        let profile = selectedProfileID
        let epoch = lifecycle
        messageRequest &+= 1
        let request = messageRequest
        let cursor = loadMore ? messageCursor : nil
        if loadMore && cursor == nil { return }
        do {
            let result = try await api.messages(profileID: profile, cursor: cursor)
            guard current(epoch), request == messageRequest, profile == selectedProfileID,
                  result.revision >= messageRevision else { return }
            messageRevision = result.revision
            let incoming = result.messages.filter { $0.profileID == profile }
            mergeMessages(incoming)
            if loadMore || !hasLoadedMessages {
                let next = result.nextCursor?.value
                messageCursor = next?.isEmpty == false && next != cursor ? next : nil
            }
            hasLoadedMessages = true
        } catch {
            if current(epoch), request == messageRequest { lastError = userFacingErrorMessage(error) }
        }
    }

    func control(start: Bool, profileID: String? = nil) async {
        guard isServiceRunning, !controlling else { return }
        let card: String
        if let profileID, let profile = profiles.first(where: { $0.profileID == profileID }) {
            card = profile.cardID
        } else if profileID != nil { return }
        else { card = selectedCardID }
        guard !card.isEmpty else { actionError = "请先选择要操作的号卡"; return }
        if start, !cards.contains(where: { $0.cardID == card && $0.available }) {
            actionError = "号卡当前不可用，请检查连接或占用状态"
            return
        }
        controlling = true
        actionError = nil
        let epoch = lifecycle
        defer { controlling = false }
        do {
            let accepted = try await api.control(start: start, request: MultiStandbyControlRequest(
                cardID: card, profileIDs: profileID.map { [$0] }, all: profileID == nil ? true : nil,
                clientRequestID: UUID().uuidString))
            guard current(epoch) else { return }
            operation = MultiStandbyOperation(operationID: accepted.operationID,
                state: accepted.accepted ? "pending" : "failed", detail: accepted.accepted ? "操作已提交，正在读取逐号状态" : "操作未被接受")
            await refreshProfiles()
        } catch {
            if current(epoch) { actionError = userFacingErrorMessage(error) }
        }
    }

    /// Returns true only after durable acceptance. A transport failure keeps
    /// exactly the same intent for an explicit retry of that profile/payload.
    func send(phone: String, body: String) async -> Bool {
        let phone = phone.trimmingCharacters(in: .whitespacesAndNewlines)
        guard canSend, !phone.isEmpty, !body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            actionError = "请选择短信就绪的号码，并填写收件人和内容"
            return false
        }
        let profileID = selectedProfileID
        let key = SendKey(profileID: profileID, phone: phone, body: body)
        let request: MultiStandbySendRequest
        if let pendingSend = pendingSends[key] { request = pendingSend }
        else {
            request = MultiStandbySendRequest(profileID: profileID, phone: phone, body: body,
                                             clientRequestID: UUID().uuidString)
        }
        pendingSends[key] = request
        sending = true
        actionError = nil
        let epoch = lifecycle
        defer { sending = false }
        do {
            let result = try await api.send(request)
            guard current(epoch) else { return false }
            if pendingSends[key] == request { pendingSends.removeValue(forKey: key) }
            if selectedProfileID == profileID {
                sendNotice = "已保存发送请求 · " + Self.messageState(result.state)
                await refreshMessages()
            }
            return true
        } catch {
            if current(epoch) {
                actionError = "发送结果待确认：\(userFacingErrorMessage(error))。请先刷新记录；再次提交相同号码和内容会复用本次请求，不会切换发送号码。"
            }
            return false
        }
    }

    func delete(_ message: MultiStandbyMessage) async {
        guard isServiceRunning, message.profileID == selectedProfileID else { return }
        let profile = message.profileID
        do {
            try await api.delete(messageID: message.messageID, profileID: profile)
            if selectedProfileID == profile {
                messageRequest &+= 1
                messages.removeAll { $0.id == message.id }
                await refreshMessages()
            }
        } catch { actionError = userFacingErrorMessage(error) }
    }

    func pollChanges() async {
        guard isServiceRunning, !changesInFlight else { return }
        changesInFlight = true
        defer { changesInFlight = false }
        let epoch = lifecycle
        do {
            // Bounded batches keep a large archive from starving status polls.
            for _ in 0..<8 {
                let previous = changesCursor
                let page = try await api.changes(cursor: previous)
                guard current(epoch) else { return }
                if notificationsSeeded {
                    for change in page.changes {
                        let identity = change.profileID + ":" + change.messageID
                        if change.deleted == true {
                            if selectedProfileID == change.profileID {
                                messageRequest &+= 1
                                messages.removeAll { $0.id == identity }
                            }
                            continue
                        }
                        guard change.kind == nil || change.kind == "message" else { continue }
                        let shouldNotify = !notifiedIDs.contains(identity)
                            && !["out", "outbound"].contains(change.direction ?? "")
                            && (change.state == nil || change.state == "received")
                        guard shouldNotify || selectedProfileID == change.profileID else { continue }
                        guard let message = try await findMessage(change), current(epoch) else { continue }
                        if selectedProfileID == message.profileID { mergeMessages([message]) }
                        guard shouldNotify && message.isIncoming else { continue }
                        let visible = viewingInbox && selectedProfileID == message.profileID && NSApp.isActive
                        if notificationsEnabled && !visible {
                            let masked = defaults.object(forKey: AppPreferenceKeys.privacyMaskEnabled) as? Bool ?? true
                            try await notificationSink(message, masked)
                            guard current(epoch) else { return }
                        }
                        notifiedIDs.append(identity)
                        notifiedIDs = Array(notifiedIDs.suffix(2048))
                        defaults.set(notifiedIDs, forKey: Self.notifiedKey)
                    }
                }
                guard current(epoch) else { return }
                if let next = page.nextCursor?.value, !next.isEmpty { changesCursor = next }
                defaults.set(changesCursor, forKey: Self.cursorKey)
                if page.changes.isEmpty || changesCursor == previous {
                    notificationsSeeded = true
                    defaults.set(true, forKey: Self.seededKey)
                    return
                }
            }
        } catch {
            if current(epoch) { lastError = "多待短信同步：" + userFacingErrorMessage(error) }
        }
    }

    private func findMessage(_ change: MultiStandbyChange) async throws -> MultiStandbyMessage? {
        // A change revision is a durable archive position, so a new arrival
        // does not require scanning years of earlier messages. Historical
        // notification clicks have no revision and follow pages to exhaustion.
        var cursor: String? = change.revision > 0 ? String(change.revision - 1) : nil
        let epoch = lifecycle
        while current(epoch) {
            let page = try await api.messages(profileID: change.profileID, cursor: cursor)
            guard current(epoch) else { throw CancellationError() }
            if let message = page.messages.first(where: { $0.profileID == change.profileID && $0.messageID == change.messageID }) {
                return message
            }
            guard let next = page.nextCursor?.value, !next.isEmpty, next != cursor else { return nil }
            cursor = next
        }
        throw CancellationError()
    }

    private func current(_ epoch: UInt64) -> Bool { isServiceRunning && lifecycle == epoch && !Task.isCancelled }

    private func mergeMessages(_ incoming: [MultiStandbyMessage]) {
        var unique: [String: MultiStandbyMessage] = [:]
        for message in messages + incoming where message.profileID == selectedProfileID { unique[message.id] = message }
        messages = unique.values.sorted {
            $0.createdAtMS == $1.createdAtMS ? $0.messageID < $1.messageID : $0.createdAtMS < $1.createdAtMS
        }
    }

    private static func postNotification(_ message: MultiStandbyMessage, masked: Bool) async throws {
        let center = UNUserNotificationCenter.current()
        var settings = await center.notificationSettings()
        if settings.authorizationStatus == .notDetermined {
            guard try await center.requestAuthorization(options: [.alert, .sound]) else { return }
            settings = await center.notificationSettings()
        }
        guard settings.authorizationStatus == .authorized || settings.authorizationStatus == .provisional else { return }
        let content = UNMutableNotificationContent()
        content.title = "N待 · 新短信"
        content.body = masked ? "内容已隐藏，请打开 OneWIFI 查看" : message.peer + "：" + message.body
        content.sound = .default
        content.userInfo = ["source": "multistandby", "profile_id": message.profileID, "message_id": message.messageID]
        try await center.add(UNNotificationRequest(identifier: "switch.multistandby." + message.id, content: content, trigger: nil))
    }

    static func messageState(_ state: String) -> String {
        switch state {
        case "received": return "已接收"
        case "queued": return "等待发送"
        case "submitting": return "正在提交"
        case "accepted": return "运营商已接受"
        case "delivered": return "已送达"
        case "failed": return "发送失败"
        case "unknown": return "结果待确认"
        case "cancelled": return "已取消"
        default: return state
        }
    }
}
