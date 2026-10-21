import AppKit
import SwiftUI

/// Remote Calls control plane: live list + history + dial/answer/hangup (+ optional DTMF).
/// Channel: Gateway `/api/devices/{id}/calls*` via `GatewayHTTPClient` + Bearer.
/// Not local UDS CallView / WebRTC media.
struct RemoteCallsView: View {
    @ObservedObject private var session = GatewaySessionStore.shared
    @ObservedObject private var store = RemoteCallsStore.shared
    @ObservedObject private var devices = RemoteDashboardStore.shared
    @Environment(\.workspaceViewport) private var viewport
    @Environment(\.openSettings) private var openSettings
    @AppStorage(AppPreferenceKeys.privacyMaskEnabled) private var privacyMaskEnabled = true

    private var selectedDeviceLabel: String {
        guard let device = devices.devices.first(where: { $0.id == store.selectedDeviceId }) else {
            return "未选择设备"
        }
        return device.name.isEmpty ? device.id : device.name
    }

    var body: some View {
        Group {
            switch session.phase {
            case .booting:
                statusPane(
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
                await devices.refresh()
                store.ensureDeviceSelection(from: devices.devices)
                if !store.selectedDeviceId.isEmpty {
                    await store.refreshAll()
                }
            }
        }
        .compatibleOnChange(of: session.isSignedIn) { signedIn in
            if signedIn {
                Task {
                    await devices.refresh()
                    store.ensureDeviceSelection(from: devices.devices)
                    if !store.selectedDeviceId.isEmpty {
                        await store.refreshAll()
                    }
                }
            } else {
                store.resetForSignOut()
            }
        }
        .compatibleOnChange(of: store.selectedDeviceId) { _ in
            Task {
                guard session.isSignedIn, !store.selectedDeviceId.isEmpty else { return }
                await store.refreshAll()
            }
        }
        .compatibleOnChange(of: devices.devices.count) { _ in
            store.ensureDeviceSelection(from: devices.devices)
        }
    }

    private var signedOutPane: some View {
        VStack(spacing: 16) {
            Image(systemName: "phone.badge.waveform")
                .font(.system(size: 44))
                .foregroundStyle(.tertiary)
            Text("尚未登录远端网关")
                .font(.title3)
            Text("在「设置 → 网关」登录后，可查看与控制远端通话。本页不走本机通话 / 音频桥，也不含 WebRTC 媒体。")
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
        ScrollView {
            VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                header
                if let actionError = store.actionError, !actionError.isEmpty {
                    Text(actionError)
                        .font(.callout)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
                dialCard
                liveCallsSection
                historySection
            }
            .padding(viewport.pageInset)
            .frame(maxWidth: 1160)
            .frame(maxWidth: .infinity)
        }
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                Button {
                    Task { await store.refreshAll() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
                .labelStyle(.iconOnly)
                .buttonStyle(.proletariusToolbar)
                .help("刷新远端通话与记录")
                .keyboardShortcut("r", modifiers: .command)
                .disabled(store.selectedDeviceId.isEmpty || store.actionBusy)
            }
            .compatibleSharedToolbarBackgroundHidden()
        }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 12) {
            WorkspaceSymbol(systemName: "phone.badge.waveform", tint: ProletariusPalette.calls, size: 36)
            VStack(alignment: .leading, spacing: 4) {
                Text("REMOTE · 远端通话")
                    .font(.system(size: 18, weight: .semibold))
                Text(session.signedInUsername.map { "网关已登录：\($0)。控制面 /api/devices/{id}/calls*（无 WebRTC）。" }
                    ?? "控制面 /api/devices/{id}/calls*（无 WebRTC）。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 0)
            devicePicker
        }
    }

    private var devicePicker: some View {
        Picker("设备", selection: $store.selectedDeviceId) {
            if devices.devices.isEmpty {
                Text("暂无设备").tag("")
            } else {
                ForEach(devices.devices) { device in
                    Text(device.name.isEmpty ? device.id : device.name).tag(device.id)
                }
            }
        }
        .labelsHidden()
        .frame(maxWidth: 200)
        .disabled(devices.devices.isEmpty)
    }

    private var dialCard: some View {
        VStack(alignment: .leading, spacing: 12) {
            WorkspaceSectionHeader(
                title: "拨号",
                detail: store.selectedDeviceId.isEmpty
                    ? "先选择远端设备。"
                    : "经 \(selectedDeviceLabel) 发起远端拨号（HTTP 202）。"
            )
            HStack(spacing: 10) {
                TextField("号码", text: $store.dialNumber)
                    .textFieldStyle(.roundedBorder)
                    .disabled(store.actionBusy || store.selectedDeviceId.isEmpty)
                Button("拨打") {
                    Task { await store.dial() }
                }
                .buttonStyle(.borderedProminent)
                .disabled(store.actionBusy || store.selectedDeviceId.isEmpty
                    || store.dialNumber.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    @ViewBuilder
    private var liveCallsSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            WorkspaceSectionHeader(
                title: "当前通话",
                detail: "GET …/calls · answer / hangup / dtmf"
            )
            switch store.liveState {
            case .idle where store.selectedDeviceId.isEmpty:
                emptyHint("请选择远端设备以加载通话。")
            case .idle, .loading:
                statusInline(title: "正在加载远端通话…")
            case .failed(let message):
                failedInline(message) {
                    Task { await store.refreshLive() }
                }
            case .empty:
                emptyHint("当前无进行中的远端通话。")
            case .loaded:
                LazyVStack(alignment: .leading, spacing: 10) {
                    ForEach(store.liveCalls) { call in
                        liveCallRow(call)
                    }
                }
            }
        }
    }

    private func liveCallRow(_ call: RemoteCall) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline, spacing: 10) {
                Image(systemName: call.isIncoming ? "phone.arrow.down.left" : "phone.arrow.up.right")
                    .foregroundStyle(ProletariusPalette.calls)
                VStack(alignment: .leading, spacing: 2) {
                    Text(PrivacyMask.value(
                        call.number.isEmpty ? "未知号码" : call.number,
                        enabled: privacyMaskEnabled
                    ))
                    .font(.headline)
                    Text("\(call.direction) · \(call.state)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer(minLength: 0)
                Text(call.startedAt, style: .time)
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            HStack(spacing: 8) {
                if call.canAnswer {
                    Button("接听") {
                        Task { await store.answer(callId: call.id) }
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(store.actionBusy)
                }
                if call.canHangup {
                    Button("挂断") {
                        Task { await store.hangup(callId: call.id) }
                    }
                    .buttonStyle(.bordered)
                    .disabled(store.actionBusy)
                }
            }
            if call.canSendDTMF {
                HStack(spacing: 8) {
                    TextField("DTMF", text: $store.dtmfDigits)
                        .textFieldStyle(.roundedBorder)
                        .frame(maxWidth: 160)
                        .disabled(store.actionBusy)
                    Button("发送") {
                        Task { await store.sendDTMF(callId: call.id) }
                    }
                    .disabled(store.actionBusy
                        || store.dtmfDigits.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    @ViewBuilder
    private var historySection: some View {
        VStack(alignment: .leading, spacing: 10) {
            WorkspaceSectionHeader(
                title: "通话记录",
                detail: "GET …/calls/history（号码尾号）"
            )
            switch store.historyState {
            case .idle where store.selectedDeviceId.isEmpty:
                emptyHint("请选择远端设备以加载记录。")
            case .idle, .loading:
                statusInline(title: "正在加载远端通话记录…")
            case .failed(let message):
                failedInline(message) {
                    Task { await store.refreshHistory() }
                }
            case .empty:
                emptyHint("暂无远端通话记录。")
            case .loaded:
                LazyVStack(alignment: .leading, spacing: 8) {
                    ForEach(store.history) { item in
                        historyRow(item)
                    }
                }
            }
        }
    }

    private func historyRow(_ item: RemoteCallHistoryItem) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            Image(systemName: item.isIncoming ? "phone.arrow.down.left" : "phone.arrow.up.right")
                .foregroundStyle(.secondary)
                .frame(width: 18)
            VStack(alignment: .leading, spacing: 2) {
                Text(PrivacyMask.value(
                    item.numberTail.isEmpty ? "未知" : item.numberTail,
                    enabled: privacyMaskEnabled
                ))
                .font(.body.weight(.medium))
                Text("\(item.direction) · \(item.state) · \(item.path) · \(item.durationSeconds)s")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
            Text(item.startedAt, style: .date)
                .font(.caption)
                .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 6)
        .padding(.horizontal, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func emptyHint(_ text: String) -> some View {
        Text(text)
            .font(.callout)
            .foregroundStyle(.secondary)
            .padding(viewport.cardPadding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .workspaceCard()
    }

    private func statusInline(title: String) -> some View {
        HStack(spacing: 10) {
            ProgressView()
                .controlSize(.small)
            Text(title)
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func failedInline(_ message: String, retry: @escaping () -> Void) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(message)
                .font(.callout)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
            Button("重试", action: retry)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func statusPane(symbol: String, title: String, detail: String) -> some View {
        VStack(spacing: 14) {
            Image(systemName: symbol)
                .font(.system(size: 40))
                .foregroundStyle(.tertiary)
            Text(title)
                .font(.title3)
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
