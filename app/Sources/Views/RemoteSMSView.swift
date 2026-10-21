import AppKit
import SwiftUI

/// Remote SMS: conversation list + thread detail + send.
/// Channel: Gateway `/api/sms/*` via `GatewayHTTPClient` + Bearer. Not local UDS SMS.
struct RemoteSMSView: View {
    @ObservedObject private var session = GatewaySessionStore.shared
    @ObservedObject private var store = RemoteSMSStore.shared
    @ObservedObject private var devices = RemoteDashboardStore.shared
    @Environment(\.workspaceViewport) private var viewport
    @Environment(\.openSettings) private var openSettings
    @AppStorage(AppPreferenceKeys.privacyMaskEnabled) private var privacyMaskEnabled = true
    @State private var showCompose = false
    @State private var searchText = ""

    private var filteredContacts: [RemoteSMSContact] {
        let query = searchText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !query.isEmpty else { return store.contacts }
        return store.contacts.filter { contact in
            contact.title.localizedCaseInsensitiveContains(query)
                || contact.peer.localizedCaseInsensitiveContains(query)
                || contact.lastMessage.localizedCaseInsensitiveContains(query)
                || contact.deviceName.localizedCaseInsensitiveContains(query)
        }
    }

    private var selectedContact: RemoteSMSContact? {
        guard let key = store.selectedKey else { return nil }
        return store.contacts.first { RemoteSMSThreadKey(contact: $0) == key }
    }

    private var usesColumns: Bool {
        viewport.contentWidth >= 230 + 340 + viewport.sectionSpacing
    }

    private var listWidth: CGFloat? {
        guard usesColumns else { return nil }
        return min(320, max(230, viewport.contentWidth * 0.32))
    }

    var body: some View {
        Group {
            switch session.phase {
            case .booting:
                remoteSMSStatusPane(
                    symbol: "arrow.triangle.2.circlepath",
                    title: "正在恢复网关会话…",
                    detail: "请稍候。"
                )
            case .signedOut, .failed:
                signedOutPane
            case .signedIn:
                signedInContent
            }
        }
        .task {
            await session.bootstrap()
            if session.isSignedIn {
                async let contacts: Void = store.refreshContacts()
                async let dash: Void = devices.refresh()
                _ = await (contacts, dash)
            }
        }
        .compatibleOnChange(of: session.isSignedIn) { signedIn in
            if signedIn {
                Task {
                    async let contacts: Void = store.refreshContacts()
                    async let dash: Void = devices.refresh()
                    _ = await (contacts, dash)
                }
            } else {
                store.resetForSignOut()
            }
        }
        .compatibleOnChange(of: store.deviceFilter) { _ in
            Task { await store.refreshContacts() }
        }
        .sheet(isPresented: $showCompose) {
            RemoteSMSComposeSheet(
                devices: devices.devices,
                initialDeviceId: store.selectedSendDeviceId,
                onCancel: { showCompose = false },
                onSend: { phone, message, deviceId in
                    let ok = await store.send(phone: phone, message: message, deviceId: deviceId)
                    if ok { showCompose = false }
                    return ok
                }
            )
        }
    }

    private var signedOutPane: some View {
        VStack(spacing: 16) {
            Image(systemName: "message")
                .font(.system(size: 44))
                .foregroundStyle(.tertiary)
            Text("尚未登录远端网关")
                .font(.title3)
            Text("在「设置 → 网关」登录后，可查看与发送远端短信。本页不走本机模块短信通道。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 420)
                .fixedSize(horizontal: false, vertical: true)
            if let message = session.statusMessage, !message.isEmpty {
                Text(message)
                    .font(.callout)
                    .foregroundStyle(.orange)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 420)
            }
            Button("打开设置 · 网关") {
                openGatewaySettings()
            }
            .buttonStyle(.borderedProminent)
            .keyboardShortcut(.defaultAction)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(24)
    }

    @ViewBuilder
    private var signedInContent: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            header
            if store.contacts.isEmpty {
                switch store.listState {
                case .idle, .loading:
                    remoteSMSStatusPane(
                        symbol: "arrow.clockwise",
                        title: "正在加载远端短信…",
                        detail: session.signedInUsername.map { "已登录：\($0)" } ?? ""
                    )
                case .failed(let message):
                    failedPane(message)
                case .empty:
                    conversationLayout
                case .loaded:
                    conversationLayout
                }
            } else {
                conversationLayout
            }
        }
        .padding(viewport.pageInset)
        .frame(maxWidth: 1160, maxHeight: .infinity, alignment: .topLeading)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                Button {
                    showCompose = true
                } label: {
                    Label("新远端短信", systemImage: "square.and.pencil")
                }
                .labelStyle(.iconOnly)
                .buttonStyle(.proletariusToolbar)
                .help("撰写远端短信")

                Button {
                    Task { await store.refreshContacts() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
                .labelStyle(.iconOnly)
                .buttonStyle(.proletariusToolbar)
                .help("刷新远端短信会话")
                .keyboardShortcut("r", modifiers: .command)
            }
            .compatibleSharedToolbarBackgroundHidden()
        }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 12) {
            WorkspaceSymbol(systemName: "message", tint: ProletariusPalette.messages, size: 36)
            VStack(alignment: .leading, spacing: 4) {
                Text("REMOTE · 远端短信")
                    .font(.system(size: 18, weight: .semibold))
                Text(session.signedInUsername.map { "网关已登录：\($0)。会话来自远端 /api/sms/*。" }
                    ?? "会话来自远端 /api/sms/*。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
            deviceFilterPicker
        }
    }

    private var deviceFilterPicker: some View {
        Picker("设备", selection: $store.deviceFilter) {
            Text("全部设备").tag("all")
            ForEach(devices.devices) { device in
                Text(device.name.isEmpty ? device.id : device.name).tag(device.id)
            }
        }
        .labelsHidden()
        .frame(maxWidth: 180)
    }

    private var conversationLayout: some View {
        let layout = usesColumns
            ? AnyLayout(HStackLayout(alignment: .top, spacing: viewport.sectionSpacing))
            : AnyLayout(VStackLayout(alignment: .leading, spacing: viewport.sectionSpacing))
        return layout {
            contactList
                .frame(width: listWidth, alignment: .topLeading)
                .frame(maxWidth: usesColumns ? listWidth : .infinity, maxHeight: .infinity, alignment: .topLeading)
            threadPane
                .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
    }

    private var contactList: some View {
        VStack(alignment: .leading, spacing: 10) {
            TextField("搜索远端会话", text: $searchText)
                .textFieldStyle(.roundedBorder)
            if filteredContacts.isEmpty {
                emptyContactsCard
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 8) {
                        ForEach(filteredContacts) { contact in
                            Button {
                                Task { await store.selectContact(contact) }
                            } label: {
                                contactRow(contact)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
            }
        }
    }

    private func contactRow(_ contact: RemoteSMSContact) -> some View {
        let selected = store.selectedKey == RemoteSMSThreadKey(contact: contact)
        return HStack(alignment: .top, spacing: 10) {
            WorkspaceSymbol(systemName: "bubble.left.fill", tint: ProletariusPalette.messages, size: 24)
                .padding(.top, 2)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 8) {
                    Text(PrivacyMask.value(contact.title, enabled: privacyMaskEnabled))
                        .font(.system(size: 13, weight: .semibold))
                        .lineLimit(1)
                    if contact.unreadCount > 0 {
                        Text("\(contact.unreadCount)")
                            .font(.caption2.weight(.semibold))
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(WorkspaceColors.accent.opacity(0.18))
                            .clipShape(Capsule())
                    }
                }
                Text(privacyMaskEnabled ? "内容已隐藏" : contact.lastMessage)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                if !contact.deviceName.isEmpty || !contact.deviceId.isEmpty {
                    Text(contact.deviceName.isEmpty ? contact.deviceId : contact.deviceName)
                        .font(.caption2)
                        .foregroundStyle(.tertiary)
                        .lineLimit(1)
                }
            }
            Spacer(minLength: 0)
            if let ts = contact.lastTimestamp {
                Text(ts.formatted(date: .omitted, time: .shortened))
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .background(selected ? WorkspaceColors.accent.opacity(0.10) : Color.clear)
        .workspaceCard()
    }

    private var emptyContactsCard: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("还没有远端短信会话")
                .font(.headline)
            Text("登录网关后若仍为空，说明远端尚未产生短信记录。可点击右上角撰写新远端短信。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    @ViewBuilder
    private var threadPane: some View {
        if store.selectedKey == nil {
            VStack(spacing: 10) {
                Image(systemName: "text.bubble")
                    .font(.system(size: 32))
                    .foregroundStyle(.tertiary)
                Text("选择一个远端会话")
                    .font(.title3)
                Text("左侧列出远端联系人；发送走网关 /api/sms/send。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .padding(24)
            .workspaceCard()
        } else {
            RemoteSMSThreadPane(
                contact: selectedContact,
                key: store.selectedKey!,
                messages: store.messages,
                state: store.threadState,
                sendBusy: store.sendBusy,
                sendError: store.sendError,
                devices: devices.devices,
                selectedSendDeviceId: $store.selectedSendDeviceId,
                onReload: { Task { await store.reloadThread() } },
                onSend: { text, deviceId in
                    let peer = store.selectedKey?.peer ?? selectedContact?.peer ?? ""
                    return await store.send(phone: peer, message: text, deviceId: deviceId)
                }
            )
        }
    }

    private func failedPane(_ message: String) -> some View {
        VStack(spacing: 14) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 40))
                .foregroundStyle(.orange)
            Text("远端短信加载失败")
                .font(.title3)
            Text(message)
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 420)
            HStack(spacing: 12) {
                Button("重试") {
                    Task { await store.refreshContacts() }
                }
                .buttonStyle(.borderedProminent)
                Button("打开设置 · 网关") {
                    openGatewaySettings()
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(24)
    }

    private func remoteSMSStatusPane(symbol: String, title: String, detail: String) -> some View {
        VStack(spacing: 12) {
            Image(systemName: symbol)
                .font(.system(size: 36))
                .foregroundStyle(.tertiary)
            Text(title).font(.title3)
            if !detail.isEmpty {
                Text(detail)
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(24)
    }

    private func openGatewaySettings() {
        UserDefaults.standard.set(
            SettingsDeepLink.gatewayTab,
            forKey: AppPreferenceKeys.settingsPreferredTab
        )
        openSettings()
    }
}

private struct RemoteSMSThreadPane: View {
    @Environment(\.workspaceViewport) private var viewport
    @AppStorage(AppPreferenceKeys.privacyMaskEnabled) private var privacyMaskEnabled = true

    let contact: RemoteSMSContact?
    let key: RemoteSMSThreadKey
    let messages: [RemoteSMSMessage]
    let state: RemoteSMSStore.LoadState
    let sendBusy: Bool
    let sendError: String?
    let devices: [RemoteDeviceOverview]
    @Binding var selectedSendDeviceId: String
    let onReload: () -> Void
    let onSend: (String, String) async -> Bool

    @State private var draft = ""

    private var title: String {
        if let contact { return contact.title }
        return key.peer
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                WorkspaceSymbol(
                    systemName: "bubble.left.and.bubble.right.fill",
                    tint: ProletariusPalette.messages,
                    size: 28
                )
                VStack(alignment: .leading, spacing: 4) {
                    Text(PrivacyMask.value(title, enabled: privacyMaskEnabled))
                        .font(.headline)
                    Text("\(messages.count) 条远端短信")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                Button("刷新", action: onReload)
                    .buttonStyle(.bordered)
            }
            .padding(viewport.cardPadding)

            Divider()

            Group {
                switch state {
                case .idle, .loading:
                    ProgressView("加载会话…")
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                case .failed(let message):
                    VStack(spacing: 10) {
                        Text("会话加载失败").font(.headline)
                        Text(message).font(.callout).foregroundStyle(.secondary)
                        Button("重试", action: onReload)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .padding(24)
                case .empty:
                    Text("此远端会话暂无消息")
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                case .loaded:
                    ScrollViewReader { proxy in
                        ScrollView {
                            LazyVStack(alignment: .leading, spacing: 12) {
                                ForEach(messages) { message in
                                    remoteBubble(message)
                                        .id(message.id)
                                }
                            }
                            .padding(viewport.cardPadding)
                        }
                        .onAppear {
                            if let last = messages.last {
                                proxy.scrollTo(last.id, anchor: .bottom)
                            }
                        }
                        .compatibleOnChange(of: messages.count) { _ in
                            if let last = messages.last {
                                withAnimation {
                                    proxy.scrollTo(last.id, anchor: .bottom)
                                }
                            }
                        }
                    }
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)

            Divider()

            VStack(alignment: .leading, spacing: 8) {
                if !devices.isEmpty {
                    Picker("发送设备", selection: $selectedSendDeviceId) {
                        ForEach(devices) { device in
                            Text(device.name.isEmpty ? device.id : device.name).tag(device.id)
                        }
                    }
                    .labelsHidden()
                }
                HStack(alignment: .bottom, spacing: 10) {
                    TextField("输入远端短信内容", text: $draft, axis: .vertical)
                        .lineLimit(1...5)
                        .textFieldStyle(.roundedBorder)
                    Button {
                        Task {
                            let deviceId = selectedSendDeviceId.isEmpty
                                ? (contact?.deviceId ?? key.deviceId)
                                : selectedSendDeviceId
                            let ok = await onSend(draft, deviceId)
                            if ok { draft = "" }
                        }
                    } label: {
                        if sendBusy {
                            ProgressView()
                        } else {
                            Text("发送")
                        }
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(sendBusy || draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
                if let sendError, !sendError.isEmpty {
                    Text(sendError)
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
            .padding(viewport.cardPadding)
        }
        .workspaceCard()
        .onAppear {
            if selectedSendDeviceId.isEmpty {
                selectedSendDeviceId = contact?.deviceId ?? key.deviceId
            }
            if selectedSendDeviceId.isEmpty, let first = devices.first {
                selectedSendDeviceId = first.id
            }
        }
    }

    private func remoteBubble(_ message: RemoteSMSMessage) -> some View {
        HStack {
            if message.isOutgoing { Spacer(minLength: 40) }
            VStack(alignment: message.isOutgoing ? .trailing : .leading, spacing: 4) {
                Text(privacyMaskEnabled ? "内容已隐藏" : message.body)
                    .font(.callout)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    .background(message.isOutgoing ? WorkspaceColors.accent.opacity(0.18) : Color.secondary.opacity(0.12))
                    .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
                Text(message.timestamp.formatted(date: .abbreviated, time: .shortened))
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            if !message.isOutgoing { Spacer(minLength: 40) }
        }
    }
}

private struct RemoteSMSComposeSheet: View {
    let devices: [RemoteDeviceOverview]
    let initialDeviceId: String
    let onCancel: () -> Void
    let onSend: (String, String, String) async -> Bool

    @State private var phone = ""
    @State private var message = ""
    @State private var deviceId: String = ""
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                TextField("收件号码", text: $phone)
                TextField("短信内容", text: $message, axis: .vertical)
                    .lineLimit(3...8)
                Picker("发送设备", selection: $deviceId) {
                    ForEach(devices) { device in
                        Text(device.name.isEmpty ? device.id : device.name).tag(device.id)
                    }
                }
                if let error, !error.isEmpty {
                    Text(error).foregroundStyle(.orange)
                }
            }
            .padding(20)
            .frame(minWidth: 420, minHeight: 260)
            .navigationTitle("新远端短信")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("取消", action: onCancel)
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button(busy ? "发送中…" : "发送") {
                        Task {
                            busy = true
                            defer { busy = false }
                            let ok = await onSend(phone, message, deviceId)
                            if !ok {
                                error = RemoteSMSStore.shared.sendError
                            }
                        }
                    }
                    .disabled(busy)
                }
            }
            .onAppear {
                deviceId = initialDeviceId
                if deviceId.isEmpty, let first = devices.first {
                    deviceId = first.id
                }
            }
        }
    }
}
