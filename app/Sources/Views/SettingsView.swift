import AppKit
import SwiftUI

struct SettingsView: View {
    private enum SettingsTab: String, CaseIterable, Identifiable {
        case general = "通用"
        case privacy = "隐私"
        case gateway = "网关"

        var id: String { rawValue }

        static func fromDeepLink(_ raw: String?) -> SettingsTab? {
            switch raw {
            case SettingsDeepLink.gatewayTab, "网关":
                return .gateway
            case "general", "通用":
                return .general
            case "privacy", "隐私":
                return .privacy
            default:
                return nil
            }
        }
    }

    @AppStorage(AppPreferenceKeys.appearance) private var appearance = AppAppearance.system.rawValue
    @AppStorage(AppPreferenceKeys.privacyMaskEnabled) private var privacyMaskEnabled = true
    @AppStorage("silentLaunch") private var silentLaunchEnabled = false
    @AppStorage(MenuBarDisplayOptions.showSignalKey) private var showSignal = false
    @AppStorage(MenuBarDisplayOptions.showDownloadKey) private var showDownload = false
    @AppStorage(MenuBarDisplayOptions.showUploadKey) private var showUpload = false
    @ObservedObject private var updateChecker = UpdateChecker.shared
    @State private var autoLaunchEnabled = false
    @State private var errorMessage: String?
    @State private var selectedTab: SettingsTab = .general
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        GeometryReader { geometry in
            let viewport = WorkspaceViewport(width: geometry.size.width, height: geometry.size.height)
            settingsContent(in: viewport)
        }
        .frame(minWidth: 560, idealWidth: 640, minHeight: 480, idealHeight: 620)
        .background(WorkspaceColors.canvas)
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.16), value: selectedTab)
        .tint(WorkspaceColors.accent)
        .buttonStyle(.proletarius)
        .environment(\.workspaceVisualStyle, .nekoko)
        .onAppear {
            autoLaunchEnabled = AutoLaunch.isEnabled
            consumePreferredTab()
        }
        .onReceive(NotificationCenter.default.publisher(for: AutoLaunch.didChangeNotification)) { _ in
            autoLaunchEnabled = AutoLaunch.isEnabled
        }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            consumePreferredTab()
        }
        .alert("设置未能保存", isPresented: Binding(
            get: { errorMessage != nil },
            set: { if !$0 { errorMessage = nil } }
        )) {
            Button("好", role: .cancel) { errorMessage = nil }
        } message: {
            Text(errorMessage ?? "未知错误")
        }
    }

    private func settingsContent(in viewport: WorkspaceViewport) -> some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: "slider.horizontal.3", tint: WorkspaceColors.accent, size: 36)
                VStack(alignment: .leading, spacing: 4) {
                    Text("PREFERENCES · 设置").font(.system(size: 18, weight: .semibold))
                    Text("调整外观、启动、更新检查、隐私偏好与 OneWIFI 网关（含证书信任）。")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, viewport.pageInset)
            .padding(.top, viewport.pageInset)

            Picker("设置分类", selection: $selectedTab) {
                ForEach(SettingsTab.allCases) { tab in
                    Text(tab.rawValue).tag(tab)
                }
            }
            .labelsHidden()
            .pickerStyle(.segmented)
            .frame(width: 300)
            .padding(.horizontal, viewport.pageInset)

            ScrollView {
                Group {
                    switch selectedTab {
                    case .general: generalSettings(in: viewport)
                    case .privacy: privacySettings(in: viewport)
                    case .gateway: GatewaySettingsPlaceholderView()
                    }
                }
                .id(selectedTab)
                .transition(.opacity)
                .padding(.horizontal, viewport.pageInset)
                .padding(.bottom, viewport.pageInset)
            }
        }
        .environment(\.workspaceViewport, viewport)
    }

    private func generalSettings(in viewport: WorkspaceViewport) -> some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            settingsSection(title: "外观", symbol: "circle.lefthalf.filled", in: viewport) {
                Picker("外观模式", selection: $appearance) {
                    ForEach(AppAppearance.allCases) { mode in
                        Text(mode.title).tag(mode.rawValue)
                    }
                }
                .labelsHidden()
                .pickerStyle(.segmented)
            }

            settingsSection(title: "启动", symbol: "power", in: viewport) {
                Toggle("登录时启动 OneWIFI", isOn: Binding(
                    get: { autoLaunchEnabled },
                    set: { updateAutoLaunch($0) }))
                Toggle("静默启动", isOn: $silentLaunchEnabled)
                    .help("启动时不展开主窗口，Dock 与菜单栏入口仍可使用")
            }

            settingsSection(title: "菜单栏", symbol: "menubar.rectangle", in: viewport) {
                Toggle("显示信号强度", isOn: $showSignal)
                Toggle("显示下载速率", isOn: $showDownload)
                Toggle("显示上传速率", isOn: $showUpload)
            }

            settingsSection(title: "软件更新", symbol: "arrow.triangle.2.circlepath", in: viewport) {
                updateCheckControls
            }
        }
    }

    @ViewBuilder
    private var updateCheckControls: some View {
        if updateChecker.isUpdateServiceConfigured {
            HStack(spacing: 16) {
                Text("更新渠道").foregroundStyle(.secondary)
                Picker("更新渠道", selection: $updateChecker.channel) {
                    ForEach(UpdateChecker.Channel.allCases) { channel in
                        Text(channel.rawValue).tag(channel)
                    }
                }
                .labelsHidden()
                .pickerStyle(.segmented)
                .frame(maxWidth: 240)
                Spacer(minLength: 0)
            }
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text("当前 \(updateChecker.currentVersion)")
                        .font(.callout.monospacedDigit())
                    Text(updateChecker.hasNewerVersion
                        ? "发现 \(updateChecker.latestVersionString)"
                        : "已是最新或尚未检查")
                        .font(.caption)
                        .foregroundStyle(updateChecker.hasNewerVersion ? WorkspaceColors.accent : .secondary)
                }
                Spacer(minLength: 0)
                Button {
                    if updateChecker.hasNewerVersion {
                        updateChecker.unskipAndPrompt()
                    } else {
                        Task { await updateChecker.check(prompt: true) }
                    }
                } label: {
                    if updateChecker.checking {
                        HStack(spacing: 8) {
                            ProgressView().controlSize(.small)
                            Text("检查中")
                        }
                    } else {
                        Text(updateChecker.hasNewerVersion ? "查看更新" : "检查更新")
                    }
                }
                .disabled(updateChecker.checking)
            }
            Toggle("启动时自动检查更新", isOn: $updateChecker.autoCheck)
            if let error = updateChecker.lastError {
                Text(error)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                Text("上次检查：\(updateChecker.lastCheckDate?.formatted(date: .abbreviated, time: .shortened) ?? "从未")")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Text("此处检查 OneWIFI for Mac 应用更新；与本机模块通道、远端网关登录相互独立。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        } else {
            Text("更新服务尚未配置。可在「关于与更新」查看版本信息；配置更新源后即可在此检查。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private func privacySettings(in viewport: WorkspaceViewport) -> some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            settingsSection(title: "敏感信息", symbol: "hand.raised.fill", in: viewport) {
                Toggle("默认启用隐私遮罩", isOn: $privacyMaskEnabled)
                Text("开启后，号码、ICCID、IMEI、序列号和通知内容会被模糊处理。进入短信会话后仍显示消息正文。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func settingsSection<Content: View>(title: String, symbol: String, in viewport: WorkspaceViewport, @ViewBuilder content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: symbol, tint: WorkspaceColors.accent, size: 34)
                Text(title).font(.system(size: 15, weight: .semibold))
            }
            content()
                .font(.callout)
                .toggleStyle(.switch)
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func updateAutoLaunch(_ enabled: Bool) {
        do {
            try AutoLaunch.setEnabled(enabled)
            autoLaunchEnabled = AutoLaunch.isEnabled
        } catch {
            autoLaunchEnabled = AutoLaunch.isEnabled
            errorMessage = error.localizedDescription
        }
    }

    private func consumePreferredTab() {
        let key = AppPreferenceKeys.settingsPreferredTab
        guard let raw = UserDefaults.standard.string(forKey: key),
              let tab = SettingsTab.fromDeepLink(raw) else {
            return
        }
        UserDefaults.standard.removeObject(forKey: key)
        selectedTab = tab
    }
}
