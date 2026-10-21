import SwiftUI

private enum SMSPageSource: String, CaseIterable, Identifiable {
    case devices = "模块短信"
    case multistandby = "N待短信"
    var id: String { rawValue }
}

/// 短信会话：左侧会话列表（按号码分组），右侧聊天界面
struct SMSView: View {
    @EnvironmentObject private var smsStore: SMSStore
    @EnvironmentObject private var attentionStore: AttentionStore
    @EnvironmentObject private var dashboardStore: DashboardStore
    @EnvironmentObject private var multiStandbyStore: MultiStandbyStore
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.workspaceViewport) private var viewport
    @State private var showCompose = false
    @State private var showClearOptions = false
    @State private var selectedItem: SMSItem?
    @State private var selectedPhone: String?
    @State private var deleteTarget: SMSDeleteTarget?
    @State private var deleting = false
    @State private var deleteError: String?
    @State private var searchText = ""
    @State private var pageSource: SMSPageSource = .devices

    private var allConversations: [Conversation] {
        Dictionary(grouping: smsStore.items) { $0.sender ?? "未知号码" }
            .map { Conversation(phone: $0.key, messages: $0.value.sorted { $0.timestamp < $1.timestamp }) }
            .sorted { $0.lastTimestamp > $1.lastTimestamp }
    }

    private var conversations: [Conversation] {
        let query = searchText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !query.isEmpty else { return allConversations }
        return allConversations.filter { conversation in
            conversation.phone.localizedCaseInsensitiveContains(query)
                || conversation.messages.contains {
                    ($0.content ?? "").localizedCaseInsensitiveContains(query)
                }
        }
    }

    private var selectedConversation: Conversation? {
        guard let selectedPhone else { return nil }
        return conversations.first { $0.phone == selectedPhone }
    }

    private var usesConversationColumns: Bool {
        viewport.contentWidth >= 230 + 340 + viewport.sectionSpacing
    }

    private var conversationLayout: AnyLayout {
        usesConversationColumns
            ? AnyLayout(HStackLayout(alignment: .top, spacing: viewport.sectionSpacing))
            : AnyLayout(VStackLayout(alignment: .leading, spacing: viewport.sectionSpacing))
    }

    private var conversationListWidth: CGFloat? {
        guard usesConversationColumns else { return nil }
        return min(300, max(230, viewport.contentWidth * 0.30))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            WorkspaceHero(
                title: "本机短信",
                subtitle: "集中查看模块短信与 N待号码的独立对话。",
                symbol: "message.fill",
                eyebrow: "MESSAGES")
            Picker("短信来源", selection: $pageSource) {
                ForEach(SMSPageSource.allCases) { source in
                    Text(source.rawValue).tag(source)
                }
            }
            .pickerStyle(.segmented)
            .frame(maxWidth: 420)
            if pageSource == .multistandby {
                MultiStandbyInboxView()
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if allConversations.isEmpty && !smsStore.busy {
                VStack(spacing: 16) {
                    ProletariusEmptyState(
                        title: "开始一段对话",
                        detail: "收到或发送的短信会按号码整理在这里。",
                        systemImage: "message",
                        actionTitle: "刷新",
                        action: { smsStore.refresh(force: true) })
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .workspaceCard()
            } else {
                // Reflow without replacing either view: resizing must not
                // discard the selected conversation or its in-progress draft.
                conversationLayout {
                    conversationList
                        .frame(width: conversationListWidth,
                               height: usesConversationColumns ? nil : min(190, viewport.height * 0.28))
                        .workspaceCard()
                    conversationDetail
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .workspaceCard()
                }
                .frame(maxHeight: .infinity)
            }
        }
        .padding(viewport.pageInset)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.18), value: selectedPhone)
        .navigationTitle("本机短信")
        .buttonStyle(.proletarius)
        .toolbar {
            ToolbarItemGroup(placement: .automatic) {
                toolbarItems
            }
            .compatibleSharedToolbarBackgroundHidden()
        }
        .onAppear {
            smsStore.viewingSMS = true
            let openingNWaitMessage = multiStandbyStore.selectedMessageID != nil || multiStandbyStore.shouldOpenInbox
            pageSource = openingNWaitMessage ? .multistandby : .devices
            if multiStandbyStore.shouldOpenInbox { multiStandbyStore.consumeSMSInboxRequest() }
            multiStandbyStore.viewingInbox = openingNWaitMessage
            multiStandbyStore.viewingPage = openingNWaitMessage
            attentionStore.markSMSViewed()
            smsStore.refresh()
            openPendingConversation()
        }
        .onDisappear {
            smsStore.viewingSMS = false
            multiStandbyStore.viewingInbox = false
            multiStandbyStore.viewingPage = false
        }
        .compatibleOnChange(of: pageSource) { source in
            let showingNWait = source == .multistandby
            multiStandbyStore.viewingInbox = showingNWait
            multiStandbyStore.viewingPage = showingNWait
            if showingNWait { multiStandbyStore.refresh() }
        }
        .compatibleOnChange(of: smsStore.pendingOpenSender) { sender in
            if sender != nil {
                openPendingConversation()
            }
        }
        .sheet(isPresented: $showCompose) { ComposeSMSView(onSent: { smsStore.refresh(force: true) }) }
        .sheet(item: $selectedItem) { item in
            SMSDetailView(item: item, onDelete: { deleted in
                if deleted {
                    selectedItem = nil
                    smsStore.refresh(force: true)
                }
            }, onReply: {
                selectedItem = nil
                selectedPhone = item.sender
                smsStore.refresh(force: true)
            })
        }
        .sheet(isPresented: $showClearOptions) {
            SMSClearView(
                readerMode: dashboardStore.status?.networkMode == "reader",
                onDone: { smsStore.refresh(force: true) })
        }
        .confirmationDialog(
            "确认删除？",
            isPresented: Binding(
                get: { deleteTarget != nil },
                set: { if !$0 { deleteTarget = nil } }),
            titleVisibility: .visible,
            presenting: deleteTarget
        ) { target in
            switch target {
            case .message(let item):
                Button("删除此短信", role: .destructive) {
                    performDelete(.message(item))
                }
                .disabled(deleting)
            case .conversation(let sender, let count):
                Button("删除全部 \(count) 条短信", role: .destructive) {
                    performDelete(.conversation(sender: sender, count: count))
                }
                .disabled(deleting)
            }
            Button("取消", role: .cancel) {}
        } message: { target in
            switch target {
            case .message:
                Text("这条短信将从实际存储位置删除，此操作不可撤销。")
            case .conversation(let sender, let count):
                Text("将删除与 \(sender) 的全部 \(count) 条短信，包括模块存储与本机归档。此操作不可撤销。")
            }
        }
        .alert(
            "删除失败",
            isPresented: Binding(
                get: { deleteError != nil },
                set: { if !$0 { deleteError = nil } })
        ) {
            Button("好", role: .cancel) { deleteError = nil }
        } message: {
            Text(deleteError ?? "未知错误")
        }
    }

    // MARK: - 工具栏

    @ViewBuilder
    private var toolbarItems: some View {
        if let error = smsStore.lastError, !error.isEmpty {
            Image(systemName: isModuleDisconnectedError(error) ? "usb.slash" : "exclamationmark.triangle")
                .foregroundStyle(.orange)
                .help(error)
                .accessibilityLabel(error)
        }
        Button {
            showCompose = true
        } label: {
            Label("新短信", systemImage: "square.and.pencil")
        }
        .labelStyle(.iconOnly)
        .buttonStyle(.proletariusToolbar)
        .foregroundStyle(WorkspaceColors.accent)
        .help("新短信")
        .keyboardShortcut("n", modifiers: .command)
        Menu {
            Button {
                smsStore.refresh(force: true)
            } label: {
                Label("刷新短信", systemImage: "arrow.clockwise")
            }
            .disabled(smsStore.busy)
            Divider()
            Button("清空短信…", role: .destructive) { showClearOptions = true }
        } label: {
            Image(systemName: "ellipsis")
                .font(.system(size: 15, weight: .semibold))
                .frame(width: 28, height: 28)
        }
        .menuStyle(.borderlessButton)
        .menuIndicator(.hidden)
        // Keep the overflow action consistent with the icon-only toolbar: no
        // capsule/plate is rendered, while the style still provides a 28pt
        // transparent hit target and pressed-state feedback.
        .buttonStyle(.proletariusToolbar)
        .fixedSize()
        .help("刷新或清理短信")
    }

    /// 点击通知后打开指定号码的会话
    private func openPendingConversation() {
        guard let sender = smsStore.pendingOpenSender else { return }
        selectedPhone = sender
        smsStore.consumeOpenRequest()
    }

    // MARK: - 会话列表

    private var conversationList: some View {
        VStack(spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Text("对话")
                    .font(.headline)
                Spacer()
                Text("\(smsStore.status?.count ?? smsStore.items.count) 条 · \(allConversations.count) 个会话")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 16)
            .padding(.top, viewport.isCompactHeight ? 12 : 18)
            .padding(.bottom, viewport.isCompactHeight ? 8 : 12)
            HStack(spacing: 8) {
                Image(systemName: "magnifyingglass")
                    .foregroundStyle(.secondary)
                TextField("搜索短信", text: $searchText)
                    .textFieldStyle(.plain)
                    .help("搜索号码或短信正文")
                if !searchText.isEmpty {
                    Button {
                        searchText = ""
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                            .foregroundStyle(.secondary)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("清除搜索")
                }
            }
            .padding(.horizontal, 12)
            .padding(.vertical, viewport.isCompactHeight ? 8 : 10)
            .background(ProletariusPalette.action.opacity(0.07), in: RoundedRectangle(cornerRadius: 10))
            .padding(.horizontal, 12)
            .padding(.bottom, 10)

            List(selection: $selectedPhone) {
                ForEach(conversations) { conversation in
                    ConversationRow(conversation: conversation)
                        .tag(conversation.phone)
                        .contextMenu {
                            if let sender = conversation.senderForDeletion {
                                Button("删除该号码的全部短信…", role: .destructive) {
                                    deleteTarget = .conversation(
                                        sender: sender,
                                        count: conversation.messages.count)
                                }
                            }
                        }
                }
            }
            .listStyle(.inset)
            .scrollIndicators(.visible)
            .scrollContentBackground(.hidden)
            .scrollBounceBehavior(.basedOnSize)
        }
        .frame(maxWidth: .infinity)
    }

    // MARK: - 会话详情

    @ViewBuilder
    private var conversationDetail: some View {
        if let conversation = selectedConversation {
            ConversationDetailView(
                conversation: conversation,
                onOpenMessage: { selectedItem = $0 },
                onDeleteMessage: { deleteTarget = .message($0) },
                onDeleteConversation: { sender, count in
                    deleteTarget = .conversation(sender: sender, count: count)
                },
                onSent: { smsStore.refresh(force: true) })
        } else {
            VStack(spacing: 14) {
                WorkspaceSymbol(systemName: "bubble.left.and.bubble.right", tint: ProletariusPalette.action, size: 48)
                Text("选择一个会话查看短信")
                    .font(.headline)
                Text("也可以从顶部创建新短信。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func performDelete(_ target: SMSDeleteTarget) {
        guard !deleting else { return }
        deleting = true
        deleteTarget = nil
        Task {
            do {
                switch target {
                case .message(let item):
                    _ = try await smsStore.deleteMessage(item)
                    if selectedItem?.id == item.id {
                        selectedItem = nil
                    }
                case .conversation(let sender, _):
                    _ = try await smsStore.deleteMessages(from: sender)
                    if selectedPhone == sender {
                        selectedPhone = nil
                    }
                }
                deleting = false
                smsStore.refresh(force: true)
            } catch {
                deleting = false
                deleteError = error.localizedDescription
            }
        }
    }
}

private enum SMSDeleteTarget: Identifiable {
    case message(SMSItem)
    case conversation(sender: String, count: Int)

    var id: String {
        switch self {
        case .message(let item): return "message|\(item.id)"
        case .conversation(let sender, _): return "conversation|\(sender)"
        }
    }
}
