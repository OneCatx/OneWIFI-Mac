import AppKit
import SwiftUI

/// Remote gateway dashboard: host card + device list + overview drill-down.
/// Read-only; SMS/Calls belong to MAC-P4.
struct RemoteDashboardView: View {
    @ObservedObject private var session = GatewaySessionStore.shared
    @ObservedObject private var store = RemoteDashboardStore.shared
    @Environment(\.workspaceViewport) private var viewport
    @Environment(\.openSettings) private var openSettings
    @State private var path = NavigationPath()

    var body: some View {
        NavigationStack(path: $path) {
            Group {
                switch session.phase {
                case .booting:
                    remoteStatusPane(
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
            .navigationDestination(for: String.self) { deviceId in
                RemoteDeviceOverviewView(deviceId: deviceId)
            }
        }
        .task {
            await session.bootstrap()
            if session.isSignedIn {
                await store.refresh()
            }
        }
        .compatibleOnChange(of: session.isSignedIn) { signedIn in
            if signedIn {
                Task { await store.refresh() }
            } else {
                store.clearSelection()
                path = NavigationPath()
                Task { await store.refresh() }
            }
        }
    }

    private var signedOutPane: some View {
        VStack(spacing: 16) {
            Image(systemName: "network")
                .font(.system(size: 44))
                .foregroundStyle(.tertiary)
            Text("尚未登录远端网关")
                .font(.title3)
            Text("在「设置 → 网关」登录后，可查看远端主机与设备列表。本页不会改动本机模块通道。")
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
        switch store.state {
        case .idle, .loading:
            remoteStatusPane(
                symbol: "arrow.clockwise",
                title: "正在加载远端仪表盘…",
                detail: session.signedInUsername.map { "已登录：\($0)" } ?? ""
            )
        case .failed(let message):
            VStack(spacing: 14) {
                Image(systemName: "exclamationmark.triangle")
                    .font(.system(size: 40))
                    .foregroundStyle(.orange)
                Text("远端仪表盘加载失败")
                    .font(.title3)
                Text(message)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: 420)
                HStack(spacing: 12) {
                    Button("重试") {
                        Task { await store.refresh() }
                    }
                    .buttonStyle(.borderedProminent)
                    Button("打开设置 · 网关") {
                        openGatewaySettings()
                    }
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .padding(24)
        case .empty, .loaded:
            dashboardScroll
        }
    }

    private var dashboardScroll: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                header
                if let host = store.host {
                    hostCard(host)
                }
                WorkspaceSectionHeader(
                    title: "远端设备",
                    detail: store.devices.isEmpty
                        ? "网关尚未登记设备。"
                        : "点击设备查看 overview（只读）。"
                )
                if store.devices.isEmpty {
                    emptyDevicesCard
                } else {
                    LazyVStack(alignment: .leading, spacing: 10) {
                        ForEach(store.devices) { device in
                            Button {
                                path.append(device.id)
                            } label: {
                                deviceRow(device)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
            }
            .padding(viewport.pageInset)
            .frame(maxWidth: 1160)
            .frame(maxWidth: .infinity)
        }
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button {
                    Task { await store.refresh() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
                .labelStyle(.iconOnly)
                .buttonStyle(.proletariusToolbar)
                .help("刷新远端仪表盘")
                .keyboardShortcut("r", modifiers: .command)
            }
            .compatibleSharedToolbarBackgroundHidden()
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            WorkspaceSymbol(systemName: "network", tint: WorkspaceColors.accent, size: 36)
            VStack(alignment: .leading, spacing: 4) {
                Text("REMOTE · 远端仪表盘")
                    .font(.system(size: 18, weight: .semibold))
                Text(session.signedInUsername.map { "网关已登录：\($0)。主机与设备摘要来自远端 API。" }
                    ?? "主机与设备摘要来自远端 API。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
    }

    private func hostCard(_ host: RemoteHostOverview) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("远端主机")
                .font(.headline)
            HStack(spacing: 16) {
                hostMetric("CPU", String(format: "%.0f%%", host.cpuPercent), host.cpuModel)
                hostMetric("内存", String(format: "%.0f%%", host.memoryPercent), bytePair(host.memoryUsedBytes, host.memoryTotalBytes))
                hostMetric("磁盘", String(format: "%.0f%%", host.diskPercent), bytePair(host.diskUsedBytes, host.diskTotalBytes))
            }
            HStack(spacing: 16) {
                labeledValue("主板", host.boardModel.isEmpty ? "—" : host.boardModel)
                labeledValue("下行", formatBps(host.netRxBps))
                labeledValue("上行", formatBps(host.netTxBps))
            }
            .font(.callout)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func hostMetric(_ title: String, _ value: String, _ caption: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).font(.caption).foregroundStyle(.secondary)
            Text(value).font(.title3.weight(.semibold))
            Text(caption.isEmpty ? "—" : caption)
                .font(.caption2)
                .foregroundStyle(.secondary)
                .lineLimit(2)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func deviceRow(_ device: RemoteDeviceOverview) -> some View {
        HStack(spacing: 14) {
            Circle()
                .fill(device.healthy ? Color.green : Color.orange)
                .frame(width: 8, height: 8)
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 8) {
                    Text(device.name.isEmpty ? device.id : device.name)
                        .font(.system(size: 14, weight: .semibold))
                    Text(device.type)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Text(deviceSubtitle(device))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            Spacer(minLength: 0)
            VStack(alignment: .trailing, spacing: 2) {
                Text(device.vowifiActive ? "VoWiFi 活跃" : device.runtime.phase.displayName)
                    .font(.caption.weight(.medium))
                Text(device.networkConnected ? "网络已连接" : "网络未连接")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            Image(systemName: "chevron.right")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.tertiary)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .workspaceCard()
    }

    private var emptyDevicesCard: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("暂无远端设备")
                .font(.headline)
            Text("仪表盘设备列表为空。可在网关侧登记设备后点击刷新。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func remoteStatusPane(symbol: String, title: String, detail: String) -> some View {
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

    private func deviceSubtitle(_ device: RemoteDeviceOverview) -> String {
        var parts: [String] = []
        if !device.operatorName.isEmpty { parts.append(device.operatorName) }
        if !device.networkMode.isEmpty { parts.append(device.networkMode) }
        if device.signalDbm != 0 { parts.append("\(device.signalDbm) dBm") }
        if !device.model.isEmpty { parts.append(device.model) }
        return parts.isEmpty ? device.id : parts.joined(separator: " · ")
    }

    private func labeledValue(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).font(.caption2).foregroundStyle(.secondary)
            Text(value.isEmpty ? "—" : value)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func bytePair(_ used: Int, _ total: Int) -> String {
        "\(formatBytes(used)) / \(formatBytes(total))"
    }

    private func formatBytes(_ value: Int) -> String {
        let formatter = ByteCountFormatter()
        formatter.countStyle = .binary
        formatter.allowsNonnumericFormatting = false
        return formatter.string(fromByteCount: Int64(value))
    }

    private func formatBps(_ value: Double) -> String {
        let formatter = ByteCountFormatter()
        formatter.countStyle = .binary
        formatter.allowsNonnumericFormatting = false
        return formatter.string(fromByteCount: Int64(value)) + "/s"
    }

    private func openGatewaySettings() {
        UserDefaults.standard.set(
            SettingsDeepLink.gatewayTab,
            forKey: AppPreferenceKeys.settingsPreferredTab
        )
        openSettings()
    }
}
