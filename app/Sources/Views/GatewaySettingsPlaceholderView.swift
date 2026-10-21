import SwiftUI

/// 设置「网关」：Base URL + 登录/登出 + 证书信任开关（MAC-P2 / P6）。
/// 禁止 WKWebView / 网页壳；HTTPS 仅走 `GatewayHTTPClient`，不经本机 UDS `APIClient`。
struct GatewaySettingsPlaceholderView: View {
    @Environment(\.workspaceViewport) private var viewport
    @ObservedObject private var session = GatewaySessionStore.shared
    @AppStorage(GatewayTrustPolicy.allowPrivateHostRelaxedTrustKey) private var allowPrivateHostRelaxedTrust = true
    @State private var username = ""
    @State private var password = ""

    var body: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            gatewayCard
            trustCard
        }
        .task { await session.bootstrap() }
    }

    private var gatewayCard: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            header
            originSection
            accountSection
            if let message = session.statusMessage, !message.isEmpty {
                Text(message)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
        .disabled(session.isBusy)
        .opacity(session.isBusy ? 0.85 : 1)
    }

    private var trustCard: some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: "checkmark.shield", tint: WorkspaceColors.accent, size: 34)
                Text("证书信任")
                    .font(.system(size: 15, weight: .semibold))
            }
            Toggle("信任局域网自签证书", isOn: $allowPrivateHostRelaxedTrust)
                .toggleStyle(.switch)
                .font(.callout)
                .help("仅对 loopback / 私网 IP 放宽 TLS 校验；公网主机始终走系统信任。")
            Text("仅在确认指纹与网关一致后保存信任；勿对陌生主机开启「一律信任」。关闭后，局域网自签证书也会按系统规则校验。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Text("本开关只影响远端网关 HTTPS 连接，不会改动本机模块通道。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
        .accessibilityElement(children: .contain)
        .accessibilityLabel("证书信任")
    }

    private var header: some View {
        HStack(spacing: 12) {
            WorkspaceSymbol(systemName: "network", tint: WorkspaceColors.accent, size: 34)
            VStack(alignment: .leading, spacing: 4) {
                Text("OneWIFI 网关")
                    .font(.system(size: 15, weight: .semibold))
                Text(statusCaption)
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("OneWIFI 网关，\(statusCaption)")
    }

    private var statusCaption: String {
        switch session.phase {
        case .booting:
            return "正在恢复会话…"
        case .signedOut:
            return "未登录远端网关"
        case .signedIn(let username, _):
            return "已登录：\(username)"
        case .failed(let message):
            return message
        }
    }

    private var originSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("网关地址")
                .font(.caption)
                .foregroundStyle(.secondary)
            TextField("http://192.168.1.88:6699", text: $session.originText)
                .textFieldStyle(.roundedBorder)
                .disabled(session.isSignedIn)
                .help("仅 http(s) 源地址（Base URL），不含路径、查询或用户信息")
            HStack {
                Button("保存地址") {
                    Task { _ = await session.saveOrigin(session.originText) }
                }
                .disabled(session.isSignedIn || session.isBusy)
                Spacer(minLength: 0)
            }
            Text("与手机端同源网关。示例：http://192.168.1.88:6699。本机模块通道与远端网关分离，互不影响。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    @ViewBuilder
    private var accountSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("账户")
                .font(.caption)
                .foregroundStyle(.secondary)

            if session.isSignedIn {
                HStack {
                    Text("会话令牌已保存在本机钥匙串。")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                    Spacer(minLength: 0)
                    Button("登出", role: .destructive) {
                        Task { await session.logout() }
                    }
                    .disabled(session.isBusy)
                }
            } else {
                TextField("用户名", text: $username)
                    .textFieldStyle(.roundedBorder)
                SecureField("密码", text: $password)
                    .textFieldStyle(.roundedBorder)
                HStack {
                    Button("登录") {
                        Task {
                            await session.login(username: username, password: password)
                            if session.isSignedIn {
                                password = ""
                            }
                        }
                    }
                    .disabled(session.isBusy || username.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || password.isEmpty)
                    .keyboardShortcut(.defaultAction)
                    Spacer(minLength: 0)
                }
            }
        }
    }
}
