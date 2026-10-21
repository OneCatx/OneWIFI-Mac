import SwiftUI

struct MultiStandbyView: View {
    @EnvironmentObject private var store: MultiStandbyStore
    @Environment(\.workspaceViewport) private var viewport
    @AppStorage(AppPreferenceKeys.privacyMaskEnabled) private var privacyMaskEnabled = true
    @State private var currentPage = 0

    let onOpenSMS: () -> Void

    private var orderedProfiles: [MultiStandbyProfile] {
        store.profiles.sorted { lhs, rhs in
            let left = profilePriority(lhs)
            let right = profilePriority(rhs)
            if left != right { return left < right }
            return lhs.title.localizedStandardCompare(rhs.title) == .orderedAscending
        }
    }

    var body: some View {
        GeometryReader { geometry in
            let layout = pageLayout(for: geometry.size)
            let visibleProfiles = pageProfiles(for: layout)

            VStack(alignment: .leading, spacing: layout.spacing) {
                header
                safetyNotice
                controls(compact: layout.compactControls)

                if let banner = statusBanner {
                    statusBannerView(banner)
                }

                profileSectionHeader(layout)

                if orderedProfiles.isEmpty {
                    ContentUnavailableView {
                        Label(store.refreshing ? "正在读取号码" : "暂无号码", systemImage: "simcard")
                    } description: {
                        Text(store.refreshing ? "正在同步 eSIM Profile 状态。" : "连接号卡后点击刷新。")
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    LazyVGrid(columns: layout.columns, alignment: .leading, spacing: layout.spacing) {
                        ForEach(visibleProfiles) { profile in
                            profileCard(profile, height: layout.cardHeight)
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .topLeading)
                    Spacer(minLength: 0)
                }
            }
            .padding(.horizontal, layout.pageInset)
            .padding(.vertical, layout.verticalInset)
            .frame(maxWidth: 1240, maxHeight: .infinity, alignment: .topLeading)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        }
        .onAppear { store.viewingPage = true; store.refresh() }
        .onDisappear { store.viewingPage = false }
        .onChange(of: store.selectedCardID) { _, _ in currentPage = 0 }
        .onChange(of: orderedProfiles.count) { _, _ in currentPage = 0 }
    }

    private var header: some View {
        HStack(spacing: 10) {
            NWaitMobiusMark()
            VStack(alignment: .leading, spacing: 2) {
                Text("N待").font(.system(size: 21, weight: .semibold))
                Text("VoWiFi 短信线路 · 独立收发，不接入语音")
                    .font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer(minLength: 8)
            HStack(alignment: .firstTextBaseline, spacing: 4) {
                Text("\(store.onlineCount)")
                    .font(.system(size: 20, weight: .semibold, design: .rounded).monospacedDigit())
                    .foregroundStyle(WorkspaceColors.accent)
                Text("/ \(store.profiles.count) 在线")
                    .font(.system(size: 11, weight: .medium)).foregroundStyle(.secondary)
            }
            .padding(.horizontal, 10).padding(.vertical, 6)
            .background(WorkspaceColors.accent.opacity(0.08), in: Capsule())
        }
        .frame(height: 40)
    }

    private var safetyNotice: some View {
        HStack(spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
                .font(.system(size: 12, weight: .semibold)).foregroundStyle(.orange)
            Text("安全提醒：请备份 eSIM 码号与恢复文件；Profile 丢失或实体卡芯片损坏，可能导致号码无法恢复。")
                .font(.system(size: 10, weight: .medium)).foregroundStyle(.primary)
                .lineLimit(1).minimumScaleFactor(0.78)
                .help("请妥善保存运营商提供的 eSIM 激活码和恢复文件；在没有确认备份可用前，不要删除或重置 Profile。实体卡芯片损坏也可能导致号码无法恢复。N待可监测 VoWiFi 掉线和 IMS 注册租期，当前无法读取运营商套餐的实际到期日。")
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 10).padding(.vertical, 6)
        .frame(height: 30)
        .workspaceCard(tint: .orange)
    }

    @ViewBuilder
    private func controls(compact: Bool) -> some View {
        VStack(spacing: compact ? 7 : 0) {
            HStack(spacing: 8) {
                Picker("号卡", selection: Binding(get: { store.selectedCardID }, set: store.selectCard)) {
                    Text("全部号卡").tag("")
                    ForEach(store.cards) { card in
                        Text("\(card.reader) · \(card.profileCount) 个号码").tag(card.cardID)
                    }
                }
                .frame(maxWidth: compact ? .infinity : 360)

                if compact {
                    Spacer(minLength: 0)
                    smsButton
                    refreshButton
                } else {
                    Spacer(minLength: 4)
                    startAllButton
                    stopAllButton
                    smsButton
                    refreshButton
                }
            }

            if compact {
                HStack(spacing: 8) {
                    startAllButton
                    stopAllButton
                    Spacer(minLength: 0)
                    Label("短信专用", systemImage: "message.badge.filled.fill")
                        .font(.system(size: 10, weight: .medium)).foregroundStyle(.secondary)
                        .lineLimit(1)
                }
            }
        }
        .controlSize(.small)
        .padding(.horizontal, 10)
        .padding(.vertical, compact ? 8 : 9)
        .workspaceCard()
    }

    private var startAllButton: some View {
        Button { Task { await store.control(start: true) } } label: {
            Label("启动全部", systemImage: "power")
        }
        .buttonStyle(.proletariusPrimary)
        .disabled(!store.isServiceRunning || !store.selectedCardAvailable || store.controlling)
        .help("启动所选号卡的全部 N待 线路")
    }

    private var stopAllButton: some View {
        Button { Task { await store.control(start: false) } } label: {
            Label("停止全部", systemImage: "stop.fill")
        }
        .buttonStyle(.proletarius)
        .disabled(!store.isServiceRunning || store.selectedCardID.isEmpty || store.controlling)
        .help("停止所选号卡的全部 N待 线路")
    }

    private var smsButton: some View {
        Button {
            store.requestSMSInbox()
            onOpenSMS()
        } label: {
            Image(systemName: "bubble.left.and.bubble.right.fill")
        }
        .buttonStyle(.proletarius)
        .disabled(store.profiles.isEmpty)
        .help("打开短信收发页面")
        .accessibilityLabel("打开短信")
    }

    private var refreshButton: some View {
        Button { store.refresh() } label: {
            Image(systemName: "arrow.clockwise")
        }
        .buttonStyle(.proletarius)
        .disabled(!store.isServiceRunning || store.refreshing)
        .help("刷新号卡和线路状态")
        .accessibilityLabel("刷新")
    }

    private func profileSectionHeader(_ layout: PageLayout) -> some View {
        HStack(spacing: 8) {
            Text("线路").font(.system(size: 15, weight: .semibold))
            Text(orderedProfiles.isEmpty ? "暂无 Profile" : "\(orderedProfiles.count) 条")
                .font(.system(size: 11)).foregroundStyle(.secondary)
            if recoveringCount > 0 {
                Label("恢复/掉线 \(recoveringCount)", systemImage: "arrow.clockwise.circle.fill")
                    .font(.system(size: 10, weight: .medium)).foregroundStyle(.orange)
                    .lineLimit(1)
                    .help("掉线或 IMS 注册租期需要刷新时，N待会重建对应 Profile 的 VoWiFi 会话。当前无法读取运营商套餐的实际到期日。")
            }
            Spacer(minLength: 4)
            if layout.pageCount > 1 {
                Text("\(layout.pageIndex + 1) / \(layout.pageCount)")
                    .font(.system(size: 11, weight: .medium, design: .rounded).monospacedDigit())
                    .foregroundStyle(.secondary)
                Button {
                    currentPage = max(0, layout.pageIndex - 1)
                } label: {
                    Image(systemName: "chevron.left")
                }
                .buttonStyle(.proletarius)
                .controlSize(.small)
                .disabled(layout.pageIndex == 0)
                .help("上一页线路")
                .accessibilityLabel("上一页线路")

                Button {
                    currentPage = min(layout.pageCount - 1, layout.pageIndex + 1)
                } label: {
                    Image(systemName: "chevron.right")
                }
                .buttonStyle(.proletarius)
                .controlSize(.small)
                .disabled(layout.pageIndex >= layout.pageCount - 1)
                .help("下一页线路")
                .accessibilityLabel("下一页线路")
            }
        }
        .frame(height: 30)
    }

    private var statusBanner: StatusBanner? {
        if !store.isServiceRunning {
            return StatusBanner(title: "本机服务未就绪", detail: "服务恢复后会自动更新线路状态。", symbol: "pause.circle", color: .orange)
        } else if let message = store.actionError ?? store.lastError {
            return StatusBanner(title: "状态同步需要关注", detail: message, symbol: "exclamationmark.triangle", color: .orange)
        } else if let operation = store.operation, !operation.isFinished {
            return StatusBanner(title: operation.detail ?? operation.state, detail: nil, symbol: "arrow.triangle.2.circlepath", color: WorkspaceColors.accent, showsProgress: true)
        } else if store.cards.isEmpty {
            return StatusBanner(title: "尚未发现可用号卡", detail: "连接读卡器后点击刷新。", symbol: "simcard", color: .secondary)
        }
        return nil
    }

    private func statusBannerView(_ banner: StatusBanner) -> some View {
        HStack(spacing: 8) {
            if banner.showsProgress { ProgressView().controlSize(.small) }
            Image(systemName: banner.symbol).foregroundStyle(banner.color)
            Text(banner.title).font(.system(size: 11, weight: .medium)).lineLimit(1)
            if let detail = banner.detail {
                Text(detail).font(.system(size: 10)).foregroundStyle(.secondary)
                    .lineLimit(1).truncationMode(.tail).help(detail)
            }
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 10).padding(.vertical, 7)
        .frame(height: 32)
        .workspaceCard(tint: banner.color)
    }

    private func profileCard(_ profile: MultiStandbyProfile, height: CGFloat) -> some View {
        let selected = store.selectedProfileID == profile.profileID
        let error = profile.lastError.flatMap { $0.isEmpty ? nil : $0 }
        return VStack(alignment: .leading, spacing: 5) {
            Button { store.selectProfile(profile.profileID) } label: {
                HStack(spacing: 7) {
                    Image(systemName: "simcard.fill")
                        .font(.system(size: 13, weight: .medium))
                        .foregroundStyle(selected ? WorkspaceColors.accent : Color.secondary)
                        .frame(width: 27, height: 27)
                        .background(WorkspaceColors.accent.opacity(selected ? 0.12 : 0.07), in: RoundedRectangle(cornerRadius: 8))
                    VStack(alignment: .leading, spacing: 2) {
                        Text(profile.label(masked: privacyMaskEnabled))
                            .font(.system(size: 12, weight: .semibold))
                            .lineLimit(1).truncationMode(.middle)
                        Text(error ?? (profile.physicalEnabled == true ? "实体卡当前启用" : "eSIM Profile"))
                            .font(.system(size: 9)).foregroundStyle(error == nil ? Color.secondary : .orange)
                            .lineLimit(1).truncationMode(.tail).help(error ?? "")
                    }
                    Spacer(minLength: 3)
                    readinessBadge(profile)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("选择 \(profile.label(masked: privacyMaskEnabled))")

            HStack(spacing: 5) {
                statusChip("鉴权", profile.authState)
                statusChip("隧道", profile.tunnelState)
                statusChip("IMS", profile.imsState)
                Spacer(minLength: 0)
            }

            HStack(spacing: 6) {
                Button {
                    Task { await store.control(start: true, profileID: profile.profileID) }
                } label: { Label(profile.isStopped ? "启动" : "重试", systemImage: "play.fill") }
                    .buttonStyle(.proletariusPrimary)
                    .disabled(!store.isServiceRunning || store.controlling || profile.isReady)
                Button {
                    Task { await store.control(start: false, profileID: profile.profileID) }
                } label: { Label("停止", systemImage: "stop.fill") }
                    .buttonStyle(.proletarius)
                    .disabled(!store.isServiceRunning || store.controlling || profile.isStopped)
                Spacer(minLength: 0)
            }
        }
        .controlSize(.small)
        .padding(9)
        .frame(maxWidth: .infinity, alignment: .leading)
        .frame(height: height, alignment: .center)
        .workspaceCard(tint: selected ? WorkspaceColors.accent : nil)
    }

    private func readinessBadge(_ profile: MultiStandbyProfile) -> some View {
        let color: Color = profile.isReady ? .green : (profile.lastError?.isEmpty == false ? .orange : .secondary)
        let title: String
        let symbol: String
        if profile.isReady {
            title = "在线"; symbol = "checkmark.circle.fill"
        } else if profile.isStopped {
            title = "已停止"; symbol = "circle"
        } else if profile.health == "offline" {
            title = "掉线"; symbol = "wifi.exclamationmark"
        } else if isRegistrationRenewing(profile) {
            title = "续期中"; symbol = "arrow.clockwise.circle.fill"
        } else if isRecovering(profile) {
            title = "恢复中"; symbol = "arrow.triangle.2.circlepath"
        } else {
            title = "连接中"; symbol = "arrow.triangle.2.circlepath"
        }
        return Label(title, systemImage: symbol)
            .font(.system(size: 9, weight: .medium))
            .foregroundStyle(color)
            .padding(.horizontal, 6).padding(.vertical, 4)
            .background(color.opacity(0.09), in: Capsule())
            .help(profile.lastError ?? "")
    }

    private func statusChip(_ title: String, _ state: String?) -> some View {
        let value = stateTitle(state)
        let color: Color = value == "就绪" ? .green : (value == "失败" ? .orange : .secondary)
        return HStack(spacing: 3) {
            Circle().fill(color).frame(width: 4, height: 4)
            Text(title)
            Text(value).foregroundStyle(color)
        }
        .font(.system(size: 9, weight: .medium))
        .padding(.horizontal, 5).padding(.vertical, 3)
        .background(Color.secondary.opacity(0.055), in: Capsule())
        .lineLimit(1).minimumScaleFactor(0.8)
    }

    private func pageLayout(for size: CGSize) -> PageLayout {
        let inset: CGFloat = size.height < 580 ? 12 : 16
        let gap: CGFloat = 9
        let contentWidth = max(0, size.width - inset * 2)
        let minimumCardWidth: CGFloat = 224
        let columnCount = max(1, min(5, Int((contentWidth + gap) / (minimumCardWidth + gap))))
        let compactControls = contentWidth < 760
        let hasBanner = statusBanner != nil
        let cardHeight: CGFloat = size.width < 560 ? 120 : 116
        let controlsHeight: CGFloat = compactControls ? 90 : 50
        let bannerHeight: CGFloat = hasBanner ? 38 : 0
        let verticalInset: CGFloat = size.height < 580 ? 8 : 10
        let interItemSpacing: CGFloat = 8
        let sectionHeight: CGFloat = 30
        let edgeInsetsHeight = verticalInset * 2
        let headerStackHeight: CGFloat = 40 + 30 + sectionHeight
        let optionalStatusHeight = bannerHeight
        let controlStackHeight = controlsHeight
        let gapCount: CGFloat = hasBanner ? 5 : 4
        let fixedHeight: CGFloat = edgeInsetsHeight + headerStackHeight + optionalStatusHeight
            + controlStackHeight + gapCount * interItemSpacing
        let availableGridHeight = max(cardHeight, size.height - fixedHeight)
        let rows = max(1, Int((availableGridHeight + gap) / (cardHeight + gap)))
        let pageSize = max(1, columnCount * rows)
        let pageCount = max(1, (orderedProfiles.count + pageSize - 1) / pageSize)
        let pageIndex = min(currentPage, pageCount - 1)
        return PageLayout(
            columns: Array(repeating: GridItem(.flexible(), spacing: gap, alignment: .top), count: columnCount),
            pageSize: pageSize,
            pageCount: pageCount,
            pageIndex: pageIndex,
            cardHeight: cardHeight,
            pageInset: inset,
            verticalInset: verticalInset,
            spacing: interItemSpacing,
            compactControls: compactControls
        )
    }

    private func pageProfiles(for layout: PageLayout) -> [MultiStandbyProfile] {
        let start = layout.pageIndex * layout.pageSize
        return Array(orderedProfiles.dropFirst(start).prefix(layout.pageSize))
    }

    private var recoveringCount: Int {
        orderedProfiles.filter { profile in
            !profile.isReady && !profile.isStopped
                && (profile.health == "offline" || isRecovering(profile) || isRegistrationRenewing(profile))
        }.count
    }

    private func isRegistrationRenewing(_ profile: MultiStandbyProfile) -> Bool {
        guard let error = profile.lastError else { return false }
        return error.contains("注册有效期") || error.contains("有效期即将结束")
    }

    private func isRecovering(_ profile: MultiStandbyProfile) -> Bool {
        ["degraded", "backoff"].contains(profile.health ?? "")
            || ["backoff", "authenticating"].contains(profile.authState ?? "")
    }

    private func profilePriority(_ profile: MultiStandbyProfile) -> Int {
        if profile.isReady { return 0 }
        if ["queued", "waiting", "connecting", "registering", "authenticating"].contains(profile.health ?? "") { return 1 }
        if profile.lastError?.isEmpty == false { return 2 }
        if profile.physicalEnabled == true { return 3 }
        return 4
    }

    private func stateTitle(_ state: String?) -> String {
        switch state {
        case "idle", "stopped", "disconnected": return "未连接"
        case "queued", "waiting": return "排队中"
        case "connecting", "registering", "authenticating": return "连接中"
        case "backoff", "degraded": return "恢复中"
        case "offline": return "掉线"
        case "verified", "authenticated", "connected", "registered", "ready": return "就绪"
        case "failed", "error": return "失败"
        case "unknown", nil: return "待确认"
        default: return state ?? "待确认"
        }
    }
}

private struct PageLayout {
    let columns: [GridItem]
    let pageSize: Int
    let pageCount: Int
    let pageIndex: Int
    let cardHeight: CGFloat
    let pageInset: CGFloat
    let verticalInset: CGFloat
    let spacing: CGFloat
    let compactControls: Bool
}

private struct StatusBanner {
    let title: String
    let detail: String?
    let symbol: String
    let color: Color
    var showsProgress = false
}

private struct NWaitMobiusMark: View {
    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: 13, style: .continuous)
                .fill(LinearGradient(
                    colors: [WorkspaceColors.accent.opacity(0.18), Color.purple.opacity(0.11), Color.cyan.opacity(0.08)],
                    startPoint: .topLeading, endPoint: .bottomTrailing))
            Canvas { context, size in
                var loop = Path()
                let steps = 144
                for index in 0...steps {
                    let angle = Double(index) / Double(steps) * 2 * Double.pi
                    let point = CGPoint(
                        x: size.width / 2 + size.width * 0.34 * CGFloat(sin(angle)),
                        y: size.height / 2 + size.height * 0.29 * CGFloat(sin(2 * angle)))
                    if index == 0 { loop.move(to: point) } else { loop.addLine(to: point) }
                }
                loop.closeSubpath()
                context.stroke(loop, with: .color(WorkspaceColors.accent.opacity(0.16)),
                               style: StrokeStyle(lineWidth: 6, lineCap: .round, lineJoin: .round))
                context.stroke(loop, with: .linearGradient(
                    Gradient(colors: [Color.cyan, WorkspaceColors.accent, Color.purple.opacity(0.9), WorkspaceColors.accent]),
                    startPoint: CGPoint(x: 2, y: size.height), endPoint: CGPoint(x: size.width - 2, y: 0)),
                    style: StrokeStyle(lineWidth: 3.4, lineCap: .round, lineJoin: .round))
            }
            .padding(5)
        }
        .frame(width: 40, height: 40)
        .overlay(RoundedRectangle(cornerRadius: 13, style: .continuous)
            .strokeBorder(WorkspaceColors.accent.opacity(0.18), lineWidth: 0.8))
        .shadow(color: WorkspaceColors.accent.opacity(0.12), radius: 5, y: 2)
        .accessibilityLabel("莫比乌斯环")
    }
}
