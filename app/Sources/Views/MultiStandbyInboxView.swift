import SwiftUI

/// N待号码的独立消息收件箱，统一放在短信页面中。
struct MultiStandbyInboxView: View {
    @EnvironmentObject private var store: MultiStandbyStore
    @Environment(\.workspaceViewport) private var viewport
    @AppStorage(AppPreferenceKeys.privacyMaskEnabled) private var privacyMaskEnabled = true
    @State private var selectedPeer = ""
    @State private var recipient = ""
    @State private var draft = ""
    @State private var deleting: MultiStandbyMessage?

    private var profiles: [MultiStandbyProfile] {
        store.profiles.sorted { lhs, rhs in
            if lhs.isReady != rhs.isReady { return lhs.isReady }
            return lhs.title.localizedStandardCompare(rhs.title) == .orderedAscending
        }
    }

    private var peers: [String] {
        let latest = Dictionary(grouping: store.messages, by: \.peer).compactMap { peer, messages in
            messages.map(\.date).max().map { (peer, $0, messages.count) }
        }
        return latest.sorted { $0.1 > $1.1 }.map { $0.0 }
    }

    private var conversation: [MultiStandbyMessage] {
        store.messages.filter { $0.peer == selectedPeer }.sorted { $0.date < $1.date }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            inboxToolbar
            HStack(spacing: 12) {
                conversationSidebar
                    .frame(width: min(248, max(205, viewport.contentWidth * 0.28)))
                conversationDetail
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .frame(minHeight: 360)
        }
        .onAppear {
            store.viewingInbox = true
            store.viewingPage = true
        }
        .onDisappear {
            store.viewingInbox = false
            store.viewingPage = false
        }
        .task {
            await store.refreshProfiles()
            if !profiles.contains(where: { $0.profileID == store.selectedProfileID }),
               let first = profiles.first {
                store.selectProfile(first.profileID)
            } else {
                await store.refreshMessages()
            }
            openRequestedMessageIfAvailable()
        }
        .compatibleOnChange(of: store.selectedProfileID) { _ in
            selectedPeer = ""
            recipient = ""
        }
        .compatibleOnChange(of: store.messages) { _ in
            openRequestedMessageIfAvailable()
        }
        .alert("删除这条短信？", isPresented: Binding(
            get: { deleting != nil }, set: { if !$0 { deleting = nil } })) {
            Button("取消", role: .cancel) { deleting = nil }
            Button("删除", role: .destructive) {
                if let message = deleting { Task { await store.delete(message) } }
                deleting = nil
            }
        } message: {
            Text("只删除当前 N待号码的这条归档记录。")
        }
    }

    private var inboxToolbar: some View {
        HStack(spacing: 10) {
            Image(systemName: "simcard.2.fill")
                .foregroundStyle(WorkspaceColors.accent)
            Picker("号码", selection: Binding(
                get: { store.selectedProfileID },
                set: { store.selectProfile($0) })) {
                if profiles.isEmpty { Text("暂无 N待号码").tag("") }
                ForEach(profiles) { profile in
                    Text(profile.label(masked: privacyMaskEnabled)).tag(profile.profileID)
                }
            }
            .frame(maxWidth: 360)
            if let profile = store.selectedProfile {
                Label(profile.isReady ? "短信就绪" : "未就绪",
                      systemImage: profile.isReady ? "checkmark.circle.fill" : "circle")
                    .font(.caption.weight(.medium))
                    .foregroundStyle(profile.isReady ? .green : .secondary)
            }
            Spacer(minLength: 0)
            Toggle("新短信通知", isOn: $store.notificationsEnabled)
                .toggleStyle(.checkbox)
                .font(.caption)
            Button { Task { await store.refreshProfiles(); await store.refreshMessages() } } label: {
                Image(systemName: "arrow.clockwise")
            }
            .buttonStyle(.proletariusToolbar)
            .help("刷新 N待短信")
            .disabled(!store.isServiceRunning || store.refreshing)
        }
        .padding(.horizontal, 12).padding(.vertical, 9)
        .workspaceCard()
    }

    private var conversationSidebar: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Label("对话", systemImage: "bubble.left.and.bubble.right.fill")
                    .font(.headline)
                Spacer()
                Text("\(peers.count)").font(.caption.monospacedDigit()).foregroundStyle(.secondary)
            }
            Divider()
            if peers.isEmpty {
                ContentUnavailableView("暂无对话", systemImage: "message", description: Text("所选 N待号码收到或发送短信后会显示在这里。"))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    VStack(spacing: 5) {
                        ForEach(peers, id: \.self) { peer in
                            peerRow(peer)
                        }
                    }
                }
                .scrollIndicators(.hidden)
            }
        }
        .padding(12)
        .frame(maxHeight: .infinity, alignment: .topLeading)
        .workspaceCard()
    }

    private func peerRow(_ peer: String) -> some View {
        let latest = store.messages.filter { $0.peer == peer }.max { $0.date < $1.date }
        return Button {
            selectedPeer = peer
            recipient = peer
        } label: {
            HStack(spacing: 9) {
                Image(systemName: "person.crop.circle.fill")
                    .font(.system(size: 23)).foregroundStyle(WorkspaceColors.accent.opacity(0.8))
                VStack(alignment: .leading, spacing: 4) {
                    Text(PrivacyMask.value(peer, enabled: privacyMaskEnabled))
                        .font(.system(size: 13, weight: .semibold)).lineLimit(1)
                    Text(latest.map { privacyMaskEnabled ? "内容已隐藏" : $0.body } ?? "")
                        .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer(minLength: 0)
            }
            .padding(9)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(selectedPeer == peer ? WorkspaceColors.accent.opacity(0.11) : Color.clear,
                        in: RoundedRectangle(cornerRadius: 10))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private var conversationDetail: some View {
        VStack(alignment: .leading, spacing: 10) {
            if selectedPeer.isEmpty {
                ContentUnavailableView("选择一段对话", systemImage: "bubble.left.and.bubble.right",
                                       description: Text("从左侧选择联系人，或直接填写收件人开始发送。"))
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                HStack(spacing: 9) {
                    Image(systemName: "person.crop.circle.fill")
                        .font(.system(size: 25)).foregroundStyle(WorkspaceColors.accent)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(PrivacyMask.value(selectedPeer, enabled: privacyMaskEnabled))
                            .font(.headline)
                        Text("\(conversation.count) 条短信 · \(store.selectedProfile?.label(masked: privacyMaskEnabled) ?? "N待号码")")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                }
                Divider()
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(spacing: 10) {
                            ForEach(conversation) { message in messageRow(message) }
                        }
                        .padding(.vertical, 8)
                    }
                    .onChange(of: conversation.last?.id) { id in
                        if let id { withAnimation(.easeOut(duration: 0.15)) { proxy.scrollTo(id, anchor: .bottom) } }
                    }
                    .onAppear {
                        if let id = conversation.last?.id { proxy.scrollTo(id, anchor: .bottom) }
                    }
                }
                if store.messageCursor != nil {
                    Button("加载更早的短信") { Task { await store.refreshMessages(loadMore: true) } }
                        .font(.caption).frame(maxWidth: .infinity)
                }
            }
            Divider()
            HStack(spacing: 8) {
                TextField("收件人号码", text: $recipient)
                    .textFieldStyle(.workspace).frame(maxWidth: 180)
                TextField("输入短信内容", text: $draft)
                    .textFieldStyle(.workspace)
                    .onSubmit(send)
                Button(store.sending ? "提交中" : "发送") { send() }
                    .buttonStyle(.proletariusPrimary)
                    .disabled(!store.canSend || recipient.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                              || draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
            if let notice = store.sendNotice, !notice.isEmpty {
                Label(notice, systemImage: "checkmark.circle")
                    .font(.caption).foregroundStyle(.secondary)
            } else if !store.canSend {
                Text("选择短信就绪的 N待号码后即可发送。")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if let error = store.actionError, !error.isEmpty {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.caption).foregroundStyle(.orange).lineLimit(2)
            }
        }
        .padding(12)
        .frame(maxHeight: .infinity, alignment: .topLeading)
        .workspaceCard()
    }

    private func messageRow(_ message: MultiStandbyMessage) -> some View {
        HStack {
            if message.isOutgoing { Spacer(minLength: 56) }
            VStack(alignment: message.isOutgoing ? .trailing : .leading, spacing: 5) {
                Text(privacyMaskEnabled ? "内容已隐藏" : message.body)
                    .font(.callout).textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: message.isOutgoing ? .trailing : .leading)
                HStack(spacing: 7) {
                    Text(message.date.formatted(date: .abbreviated, time: .shortened))
                    Text(MultiStandbyStore.messageState(message.state))
                    Button { deleting = message } label: { Image(systemName: "trash") }
                        .buttonStyle(.plain).help("删除短信")
                        .disabled(!store.isServiceRunning)
                }
                .font(.caption2).foregroundStyle(.secondary)
            }
            .padding(.horizontal, 12).padding(.vertical, 9)
            .frame(maxWidth: 460, alignment: message.isOutgoing ? .trailing : .leading)
            .background(message.isOutgoing ? WorkspaceColors.accent.opacity(0.12) : Color.secondary.opacity(0.07),
                        in: RoundedRectangle(cornerRadius: 12))
            if !message.isOutgoing { Spacer(minLength: 56) }
        }
        .id(message.id)
    }

    private func send() {
        let submittedBody = draft
        Task {
            if await store.send(phone: recipient, body: submittedBody), draft == submittedBody {
                draft = ""
            }
        }
    }

    private func openRequestedMessageIfAvailable() {
        guard let messageID = store.selectedMessageID,
              let message = store.messages.first(where: { $0.messageID == messageID }) else { return }
        selectedPeer = message.peer
        recipient = message.peer
        store.consumeOpenMessageRequest()
    }
}
