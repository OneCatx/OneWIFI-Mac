import AppKit
import SwiftUI

/// Remote upstream proxies: list + enable switch.
/// Channel: Gateway `/api/upstream-proxies*` via `GatewayHTTPClient` + Bearer.
/// Not local Mihomo / Switch front proxy.
struct RemoteUpstreamView: View {
    @ObservedObject private var session = GatewaySessionStore.shared
    @ObservedObject private var store = RemoteUpstreamStore.shared
    @Environment(\.workspaceViewport) private var viewport
    @Environment(\.openSettings) private var openSettings

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
                await store.refresh()
            }
        }
        .compatibleOnChange(of: session.isSignedIn) { signedIn in
            if signedIn {
                Task { await store.refresh() }
            } else {
                store.resetForSignOut()
            }
        }
    }

    private var signedOutPane: some View {
        VStack(spacing: 16) {
            Image(systemName: "point.3.connected.trianglepath.dotted")
                .font(.system(size: 44))
                .foregroundStyle(.tertiary)
            Text("尚未登录远端网关")
                .font(.title3)
            Text("在「设置 → 网关」登录后，可查看与启停远端上游代理。本页与本机 Mihomo / 前置代理无关。")
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
            statusPane(
                symbol: "arrow.clockwise",
                title: "正在加载远端代理…",
                detail: session.signedInUsername.map { "已登录：\($0)" } ?? ""
            )
        case .failed(let message):
            VStack(spacing: 14) {
                Image(systemName: "exclamationmark.triangle")
                    .font(.system(size: 40))
                    .foregroundStyle(.orange)
                Text("远端代理加载失败")
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
            listScroll
        }
    }

    private var listScroll: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                HStack(alignment: .firstTextBaseline) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("远端代理")
                            .font(.title2.weight(.semibold))
                        Text(session.signedInUsername.map { "网关已登录：\($0)。列表来自 /api/upstream-proxies。" }
                            ?? "列表来自 /api/upstream-proxies。")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer(minLength: 12)
                    Button {
                        Task { await store.refresh() }
                    } label: {
                        Label("刷新", systemImage: "arrow.clockwise")
                    }
                    .disabled(store.toggleBusyId != nil)
                }

                if let actionError = store.actionError, !actionError.isEmpty {
                    Text(actionError)
                        .font(.callout)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }

                WorkspaceSectionHeader(
                    title: "上游代理",
                    detail: store.proxies.isEmpty
                        ? "网关尚未配置上游代理。"
                        : "可启停；新建 / 删除 / 国家规则本票不做。"
                )

                if store.proxies.isEmpty {
                    VStack(alignment: .leading, spacing: 8) {
                        Text("暂无远端代理")
                            .font(.headline)
                        Text("在网关侧配置上游代理后点击刷新。与本机「代理」Tab（Mihomo）分表面。")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .padding(viewport.cardPadding)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .workspaceCard()
                } else {
                    LazyVStack(alignment: .leading, spacing: 10) {
                        ForEach(store.proxies) { proxy in
                            proxyRow(proxy)
                        }
                    }
                }
            }
            .padding(viewport.pageInset)
            .frame(maxWidth: 1160)
            .frame(maxWidth: .infinity)
        }
    }

    private func proxyRow(_ proxy: RemoteUpstreamProxy) -> some View {
        HStack(alignment: .center, spacing: 14) {
            Image(systemName: proxy.enabled ? "network" : "network.slash")
                .font(.system(size: 18, weight: .medium))
                .foregroundStyle(proxy.enabled ? Color.accentColor : .secondary)
                .frame(width: 28)
            VStack(alignment: .leading, spacing: 4) {
                Text(proxy.name)
                    .font(.headline)
                Text(proxy.addr.isEmpty ? proxy.id : "\(proxy.addr) · \(proxy.id)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            Spacer(minLength: 8)
            Toggle(
                proxy.enabled ? "已启用" : "已停用",
                isOn: Binding(
                    get: { proxy.enabled },
                    set: { newValue in
                        Task { await store.setEnabled(id: proxy.id, enabled: newValue) }
                    }
                )
            )
            .labelsHidden()
            .disabled(store.toggleBusyId != nil)
            .accessibilityLabel(proxy.enabled ? "停用 \(proxy.name)" : "启用 \(proxy.name)")
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
        .opacity(store.toggleBusyId == proxy.id ? 0.7 : 1)
    }

    private func statusPane(symbol: String, title: String, detail: String) -> some View {
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
