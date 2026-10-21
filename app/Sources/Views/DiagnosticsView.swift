import SwiftUI
import UserNotifications

/// 调试与诊断：按功能分页组织（网络诊断 / AT 调试 / 通知调试），
/// 后续新增调试功能时新增一个 tab 及对应子视图即可，互不影响。
struct DiagnosticsView: View {
    enum DebugTab: String, CaseIterable, Identifiable {
        case network = "网络诊断"
        case at = "AT 调试"
        case notify = "通知调试"

        var id: String { rawValue }
    }

    @State private var tab: DebugTab = .network
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.workspaceViewport) private var viewport

    var body: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 20) {
                    diagnosticsHeading
                        .frame(minWidth: 360)
                    diagnosticsPicker.frame(width: 360)
                }
                VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                    diagnosticsHeading
                    diagnosticsPicker.frame(maxWidth: 480)
                }
            }
            .padding(.horizontal, viewport.pageInset)
            .padding(.top, viewport.pageInset)

            Group {
                switch tab {
                case .network: DiagnosticNetworkView()
                case .at: DiagnosticATView()
                case .notify: DiagnosticNotifyView()
                }
            }
            .id(tab)
            .transition(.opacity)
            .padding(.horizontal, viewport.pageInset)
            .padding(.bottom, viewport.pageInset)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.16), value: tab)
        .navigationTitle("调试与诊断")
        .buttonStyle(.proletarius)
        .tint(WorkspaceColors.accent)
    }

    @ViewBuilder
    private var diagnosticsHeading: some View {
        if viewport.isCompactHeight {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: "stethoscope", tint: WorkspaceColors.accent, size: 36)
                VStack(alignment: .leading, spacing: 4) {
                    Text("DEVELOPER TOOLS · 调试与诊断")
                        .font(.system(size: 17, weight: .semibold))
                    Text("采集网络信息、发送 AT 指令并检查通知。")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        } else {
            WorkspaceHero(title: "调试与诊断", subtitle: "采集网络信息、发送 AT 指令并检查通知。", symbol: "stethoscope", eyebrow: "DEVELOPER TOOLS")
        }
    }

    private var diagnosticsPicker: some View {
        Picker("诊断工具", selection: $tab) {
            ForEach(DebugTab.allCases) { item in
                Text(item.rawValue).tag(item)
            }
        }
        .labelsHidden()
        .pickerStyle(.segmented)
    }
}

// MARK: - 网络诊断

struct DiagnosticNetworkView: View {
    @EnvironmentObject private var backend: BackendProcess
    @Environment(\.workspaceViewport) private var viewport

    @State private var diagnostic: NetworkDiagnostic?
    @State private var diagnosticError: String?
    @State private var isLoading = false

    var body: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: "network", tint: WorkspaceColors.accent, size: 40)
                WorkspaceSectionHeader(title: "连接与网络", detail: "模块、系统接口与路由的当前信息。")
                if let diagnostic, let errors = diagnostic.errors, !errors.isEmpty {
                    Label("部分命令失败", systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
                Spacer()
                Button {
                    loadDiagnostic()
                } label: {
                    Label(isLoading ? "采集中" : "重新采集", systemImage: "arrow.clockwise")
                }
                .controlSize(.small)
                .disabled(isLoading)
                .help("重新采集诊断信息")
            }
            Divider()
            ScrollView {
                LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 10), count: viewport.columnCount(minimumWidth: 270, maximum: 3)), alignment: .leading, spacing: 10) {
                    if let diagnostic {
                        detailRows(for: diagnostic)
                    } else if let diagnosticError {
                        Label(isModuleDisconnectedError(diagnosticError) ? moduleDisconnectedFriendlyMessage : diagnosticError, systemImage: "exclamationmark.circle")
                            .font(.callout).foregroundStyle(.secondary)
                            .padding(.vertical, 16)
                    } else {
                        HStack(spacing: 8) {
                            ProgressView().controlSize(.small)
                            Text("读取诊断信息…").foregroundStyle(.secondary)
                        }
                    }
                }
                .font(.callout)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .scrollIndicators(.visible)
        }
        .padding(viewport.cardPadding)
        .workspaceCard()
        .onAppear { loadDiagnostic() }
    }

    private func detailRows(for diagnostic: NetworkDiagnostic) -> some View {
        Group {
            detailRow("USB 网卡模式", diagnostic.usbnetMode ?? "-")
            if let usbcfg = diagnostic.usbcfg {
                detailRow("USB 配置", usbcfg)
            }
            if let present = diagnostic.usbNetworkPresent {
                detailRow("USB 网络接口", present ? "存在" : "不存在")
            }
            if let route = diagnostic.defaultRoute {
                detailRow("默认出口", "\(route.interface ?? "-") → \(route.gateway ?? "-")")
            }
            ForEach(diagnostic.macInterfaces ?? [], id: \.name) { iface in
                detailRow("网络接口", "\(iface.name ?? "-")（\(iface.kind ?? "-")，\(iface.status ?? "-")）\(iface.ipv4 ?? "")")
            }
            ForEach(diagnostic.pdpContexts ?? [], id: \.id) { ctx in
                detailRow("PDP 上下文 \(ctx.id ?? 0)", "\(ctx.pdn ?? "-") \(ctx.apn ?? "-")")
            }
            if let addresses = diagnostic.pdpAddresses, !addresses.isEmpty {
                detailRow("PDP 地址", addresses.joined(separator: "、"))
            }
            if let errors = diagnostic.errors, !errors.isEmpty {
                ForEach(errors.sorted(by: { $0.key < $1.key }), id: \.key) { key, value in
                    detailRow("\(key) 错误", value)
                }
            }
        }
    }

    private func detailRow(_ key: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(key).font(.caption).foregroundStyle(.secondary)
            Text(value)
                .font(.system(size: 12, design: .monospaced))
                .copyableInformation(value)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(viewport.isCompactHeight ? 10 : 14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(WorkspaceColors.inset, in: RoundedRectangle(cornerRadius: 10))
    }

    private func loadDiagnostic() {
        guard !isLoading else { return }
        guard case .running = backend.state else {
            diagnostic = nil
            diagnosticError = "后端未运行，启动服务后可重新采集。"
            return
        }
        isLoading = true
        Task {
            do {
                let diag: NetworkDiagnostic = try await APIClient().get("api/network")
                await MainActor.run {
                    diagnostic = diag
                    diagnosticError = nil
                    isLoading = false
                }
            } catch {
                await MainActor.run {
                    diagnostic = nil
                    diagnosticError = error.localizedDescription
                    isLoading = false
                }
            }
        }
    }
}

// MARK: - AT 调试

struct DiagnosticATView: View {
    @State private var command = ""
    @State private var history: [ATEntry] = []
    @State private var atBusy = false
    @State private var atError: String?
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.workspaceViewport) private var viewport

    private let quickCommands = ["AT", "AT+CSQ", "AT+COPS?", "AT+CPIN?", "AT+CNUM", "AT+QCFG=\"usbnet\""]

    var body: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: "terminal", tint: WorkspaceColors.accent, size: 40)
                WorkspaceSectionHeader(title: "AT 控制台", detail: "选择快捷指令或直接输入，查看模块的原始响应。")
            }
            quickBar
            if let atError {
                Label {
                    Text(isModuleDisconnectedError(atError) ? moduleDisconnectedFriendlyMessage : atError)
                        .fixedSize(horizontal: false, vertical: true)
                        .textSelection(.enabled)
                } icon: {
                    Image(systemName: "exclamationmark.triangle")
                }
                .font(.caption)
                .foregroundStyle(.orange)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            Divider()
            outputArea
            inputBar
        }
        .padding(viewport.cardPadding)
        .workspaceCard()
    }

    private var quickBar: some View {
        ScrollView(.horizontal, showsIndicators: true) {
            HStack(spacing: 8) {
                ForEach(quickCommands, id: \.self) { cmd in
                    Button(cmd) {
                        command = cmd
                    }
                    .buttonStyle(.proletarius)
                    .controlSize(.small)
                }
            }
            .padding(.vertical, 2)
        }
        .accessibilityLabel("快捷指令")
    }

    private var outputArea: some View {
        ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    if history.isEmpty {
                        VStack(spacing: 8) {
                            WorkspaceSymbol(systemName: "terminal", tint: WorkspaceColors.accent, size: 54)
                            Text("输入 AT 指令并发送，响应将显示在这里")
                                .font(.callout).foregroundStyle(.secondary)
                        }
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, viewport.isCompactHeight ? 20 : 42)
                    } else {
                        ForEach(history) { entry in
                            ATEntryRow(entry: entry)
                                .id(entry.id)
                        }
                    }
                }
                .padding(16)
            }
            .scrollIndicators(.visible)
            .compatibleOnChange(of: history.count) { _ in
                if let last = history.last {
                    withAnimation(reduceMotion ? nil : .easeOut(duration: 0.18)) {
                        proxy.scrollTo(last.id, anchor: .bottom)
                    }
                }
            }
        }
        .frame(minHeight: viewport.isVeryShort ? 80 : 140)
        .background(WorkspaceColors.inset, in: RoundedRectangle(cornerRadius: 12))
    }

    private var inputBar: some View {
        HStack(spacing: 10) {
            TextField("例如：AT+CSQ", text: $command)
                .textFieldStyle(.workspace)
                .font(.monospaced(.body)())
                .onSubmit { sendAT() }
            Button {
                sendAT()
            } label: {
                if atBusy {
                    ProgressView().controlSize(.small)
                } else {
                    Label("发送", systemImage: "paperplane")
                }
            }
            .buttonStyle(.proletariusPrimary)
            .disabled(atBusy || command.isEmpty)
        }
        .padding(.top, 2)
    }

    private func sendAT() {
        let cmd = command.trimmingCharacters(in: .whitespacesAndNewlines)
        // Return in the text field bypasses the button's disabled state.
        // Keep one diagnostic command in flight regardless of input method.
        guard !cmd.isEmpty, !atBusy else { return }
        atBusy = true
        let client = APIClient()
        Task {
            do {
                let result: ATResult = try await client.send(
                    "api/at", body: ATRequest(command: cmd))
                await MainActor.run {
                    history.append(ATEntry(command: cmd, response: result.response ?? ""))
                    atBusy = false
                    atError = nil
                }
            } catch {
                await MainActor.run {
                    let errDesc = error.localizedDescription
                    let displayMsg = isModuleDisconnectedError(errDesc) ? moduleDisconnectedFriendlyMessage : "错误：\(errDesc)"
                    history.append(ATEntry(command: cmd, response: displayMsg, isError: true))
                    atBusy = false
                    atError = errDesc
                }
            }
        }
        command = ""
    }
}

// MARK: - 通知调试

struct DiagnosticNotifyView: View {
    @EnvironmentObject private var smsStore: SMSStore
    @EnvironmentObject private var store: DashboardStore
    @EnvironmentObject private var attentionStore: AttentionStore
    @Environment(\.workspaceViewport) private var viewport

    @State private var notifyAuth: String?
    @State private var notifyFeedback: String?
    /// 已授权但通知样式被系统设为"无"（静默，不弹横幅）
    @State private var notifyBannerOff = false

    var body: some View {
        ScrollView {
            LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: viewport.sectionSpacing, alignment: .top), count: viewport.columnCount(minimumWidth: 260, maximum: 2)), alignment: .leading, spacing: viewport.sectionSpacing) {
                VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                    HStack(spacing: 12) {
                        WorkspaceSymbol(systemName: "bell.badge", tint: WorkspaceColors.accent, size: 40)
                        WorkspaceSectionHeader(title: "通知测试", detail: "验证系统短信通知与应用来电提醒。")
                    }
                    HStack(spacing: 12) {
                        Text("通知权限").font(.callout).foregroundStyle(.secondary)
                        Spacer()
                        if let notifyAuth {
                            Text(notifyAuth)
                                .font(.callout.weight(.semibold))
                                .foregroundStyle(notifyAuth == "已开启" ? Color.green : Color.orange)
                        } else {
                            ProgressView().controlSize(.small)
                        }
                        if notifyBannerOff {
                            Button("打开通知设置") { openNotifySettings() }
                                .controlSize(.small)
                                .help("在系统设置中将通知样式改为横幅，通知才会弹出")
                        }
                    }
                    .padding(14)
                    .background(WorkspaceColors.inset, in: RoundedRectangle(cornerRadius: 12))

                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 220), spacing: 12)], spacing: 12) {
                        Button { simulateNotify() } label: {
                            Label("模拟短信通知", systemImage: "bell.badge")
                                .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.proletarius)
                        .help("直接发送一条测试通知，验证新短信系统通知是否正常工作")

                        Button { simulateCallNotify() } label: {
                            Label("模拟来电通知", systemImage: "phone.fill")
                                .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.proletarius)
                        .help("直接弹出一张自定义来电卡片，验证来电提醒是否正常工作")
                    }

                    if let notifyFeedback {
                        Text(notifyFeedback)
                            .font(.caption)
                            .foregroundStyle(notifyFeedback.contains("成功") || notifyFeedback.contains("已发送") ? Color.secondary : Color.red)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .padding(viewport.cardPadding)
                .workspaceCard()

                VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                    HStack(spacing: 12) {
                        WorkspaceSymbol(systemName: "number.circle", tint: WorkspaceColors.accent, size: 40)
                        WorkspaceSectionHeader(title: "未读计数调试", detail: "调整测试计数，不创建真实短信或通话。")
                        Button("计数清零") { attentionStore.clearForDebug() }
                            .controlSize(.small)
                            .disabled(!attentionStore.hasAttention)
                    }
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 220), spacing: 12)], spacing: 12) {
                        Button { attentionStore.incrementUnreadSMSForDebug() } label: {
                            HStack(spacing: 8) {
                                Text("增加未读短信")
                                Spacer(minLength: 0)
                                if attentionStore.unreadSMSCount > 0 {
                                    AttentionBadge(count: attentionStore.unreadSMSCount, accessibilityName: "未读短信")
                                }
                            }
                            .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.proletarius)
                        .help("将未读短信计数增加 1，不创建真实短信")

                        Button { attentionStore.incrementUnviewedCallForDebug() } label: {
                            HStack(spacing: 8) {
                                Text("增加未查看本机通话")
                                Spacer(minLength: 0)
                                if attentionStore.unviewedCallCount > 0 {
                                    AttentionBadge(count: attentionStore.unviewedCallCount, accessibilityName: "未查看本机通话")
                                }
                            }
                            .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.proletarius)
                        .help("将未查看本机通话计数增加 1，不创建真实本机通话记录")
                    }
                }
                .padding(viewport.cardPadding)
                .workspaceCard()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .onAppear { loadNotifyAuth() }
    }

    private func loadNotifyAuth() {
        Task {
            let settings = await UNUserNotificationCenter.current().notificationSettings()
            let text: String
            switch settings.authorizationStatus {
            case .authorized, .provisional, .ephemeral: text = "已开启"
            case .notDetermined: text = "未授权"
            case .denied: text = "已拒绝"
            @unknown default: text = "未知"
            }
            await MainActor.run {
                notifyAuth = text
                notifyBannerOff = settings.authorizationStatus == .authorized && settings.alertSetting == .disabled
            }
        }
    }

    private func openNotifySettings() {
        // 打开 macOS 系统设置的通知面板。
        if let url = URL(string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension") {
            NSWorkspace.shared.open(url)
        }
    }

    private func simulateNotify() {
        Task {
            notifyFeedback = "正在发送…"
            notifyFeedback = await smsStore.simulateNotification()
            loadNotifyAuth()
        }
    }

    private func simulateCallNotify() {
        notifyFeedback = "已弹出模拟来电卡片（30 秒后自动收起）"
        IncomingCallCard.shared.show(store: store, preview: true)
    }
}

// MARK: - AT 记录

struct ATEntry: Identifiable {
    let id = UUID()
    let command: String
    let response: String
    var isError: Bool = false
}

struct ATEntryRow: View {
    let entry: ATEntry

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(entry.command)
                .font(.monospaced(.body)())
                .textSelection(.enabled)
            Text(entry.response.isEmpty ? "(无响应)" : entry.response)
                .font(.monospaced(.body)())
                .foregroundStyle(entry.isError ? Color.red : Color(nsColor: .secondaryLabelColor))
                .textSelection(.enabled)
        }
        .padding(.vertical, 6)
        .padding(.horizontal, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(entry.isError ? Color.red.opacity(0.08) : WorkspaceColors.accent.opacity(0.05)))
        .padding(.vertical, 3)
    }
}
