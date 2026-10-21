import AppKit
import SwiftUI

/// Remote HTTPS certificate status (read-only).
/// Channel: Gateway `GET /api/settings/https` via `GatewayHTTPClient` + Bearer.
/// No PEM download / upload / reissue in this ticket.
struct RemoteCertificateView: View {
    @ObservedObject private var session = GatewaySessionStore.shared
    @ObservedObject private var store = RemoteCertificateStore.shared
    @Environment(\.workspaceViewport) private var viewport
    @Environment(\.openSettings) private var openSettings

    private static let notAfterFormatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateStyle = .medium
        formatter.timeStyle = .short
        return formatter
    }()

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
            Image(systemName: "lock.shield")
                .font(.system(size: 44))
                .foregroundStyle(.tertiary)
            Text("尚未登录远端网关")
                .font(.title3)
            Text("在「设置 → 网关」登录后，可查看远端 HTTPS 证书状态。本页只读，不上传或更换证书。")
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
                title: "正在加载远端证书…",
                detail: session.signedInUsername.map { "已登录：\($0)" } ?? ""
            )
        case .failed(let message):
            VStack(spacing: 14) {
                Image(systemName: "exclamationmark.triangle")
                    .font(.system(size: 40))
                    .foregroundStyle(.orange)
                Text("远端证书加载失败")
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
            detailScroll
        }
    }

    private var detailScroll: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                HStack(alignment: .firstTextBaseline) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("远端证书")
                            .font(.title2.weight(.semibold))
                        Text(session.signedInUsername.map { "网关已登录：\($0)。状态来自 GET /api/settings/https。" }
                            ?? "状态来自 GET /api/settings/https。")
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
                }

                if let https = store.httpsState {
                    statusCard(https)
                    endpointsCard(https)
                    identityCard(https)
                } else {
                    VStack(alignment: .leading, spacing: 8) {
                        Text("暂无证书状态")
                            .font(.headline)
                        Text("网关未返回可用的 HTTPS 证书信息。")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                    }
                    .padding(viewport.cardPadding)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .workspaceCard()
                }
            }
            .padding(viewport.pageInset)
            .frame(maxWidth: 1160)
            .frame(maxWidth: .infinity)
        }
    }

    private func statusCard(_ https: RemoteHttpsState) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            WorkspaceSectionHeader(
                title: "HTTPS 状态",
                detail: "只读列表/状态；上传、续签、删除本票不做。"
            )
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 160), spacing: 12)], spacing: 12) {
                labeledValue("启用", https.enabled ? "是" : "否")
                labeledValue("来源", https.displaySource)
                labeledValue("覆盖主机", https.coversHost ? "是" : "否")
                labeledValue("主机", https.host ?? "—")
            }
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func endpointsCard(_ https: RemoteHttpsState) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("访问地址")
                .font(.headline)
            labeledValue("HTTP", https.httpUrl.isEmpty ? "—" : https.httpUrl)
            labeledValue("HTTPS", https.httpsUrl.isEmpty ? "—" : https.httpsUrl)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func identityCard(_ https: RemoteHttpsState) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("证书标识")
                .font(.headline)
            labeledValue("常用名", https.displayCommonName)
            labeledValue(
                "有效期至",
                https.notAfter.map { Self.notAfterFormatter.string(from: $0) } ?? "—"
            )
            labeledValue("指纹", https.fingerprint ?? "—")
            if !https.names.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    Text("备用名")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                    Text(https.names.joined(separator: ", "))
                        .font(.body)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func labeledValue(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).font(.caption2).foregroundStyle(.secondary)
            Text(value.isEmpty ? "—" : value)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
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
