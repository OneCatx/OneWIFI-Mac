import SwiftUI
import AppKit

/// 关于与更新：版本信息、更新与许可分组展示。
struct AboutView: View {
    @EnvironmentObject private var updateChecker: UpdateChecker
    @Environment(\.workspaceViewport) private var viewport

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                aboutHeading

                HStack(alignment: .center, spacing: 20) {
                    ProletariusLogo(placement: .about)
                    VStack(alignment: .leading, spacing: 8) {
                        Text(Bundle.main.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String ?? "OneWIFI")
                            .font(.system(size: 22, weight: .bold))
                        Text("版本 \(updateChecker.currentVersion)（构建 \(updateChecker.buildNumber)）")
                            .font(.callout.monospacedDigit())
                            .foregroundStyle(WorkspaceColors.accent)
                        Text("构建于 \(buildDateText)")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer(minLength: 0)
                    if let url = telegramURL {
                        Button {
                            NSWorkspace.shared.open(url)
                        } label: {
                            ProletariusLogo(placement: .compactAction)
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel("Telegram")
                        .help("Telegram")
                    }
                }
                .padding(viewport.cardPadding)
                .frame(maxWidth: .infinity, alignment: .leading)
                .workspaceCard()

                // Height only adjusts spacing; it must not move a focused
                // update control into a different column or squeeze license text.
                aboutCardsLayout {
                    updateCard
                    licenseCard
                        .frame(width: usesAboutColumns ? 280 : nil)
                }
            }
            .padding(viewport.pageInset)
            .frame(maxWidth: 1160)
            .frame(maxWidth: .infinity)
        }
        .navigationTitle("关于与更新")
        .tint(WorkspaceColors.accent)
    }

    private var usesAboutColumns: Bool {
        viewport.contentWidth >= 760
    }

    private var aboutCardsLayout: AnyLayout {
        usesAboutColumns
            ? AnyLayout(HStackLayout(alignment: .top, spacing: viewport.sectionSpacing))
            : AnyLayout(VStackLayout(alignment: .leading, spacing: viewport.sectionSpacing))
    }

    @ViewBuilder
    private var aboutHeading: some View {
        if viewport.isCompactHeight {
            HStack(spacing: 12) {
                WorkspaceSymbol(systemName: "info.circle", tint: WorkspaceColors.accent, size: 36)
                VStack(alignment: .leading, spacing: 4) {
                    Text("关于与更新").font(.system(size: 18, weight: .semibold))
                    Text("OneWIFI · 应用版本、更新渠道与许可信息。")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        } else {
            WorkspaceHero(title: "关于与更新", subtitle: "应用版本、更新渠道与许可信息。", symbol: "info.circle", eyebrow: "ONEWIFI")
        }
    }

    private var updateCard: some View {
                VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                    HStack(spacing: 12) {
                        WorkspaceSymbol(systemName: "arrow.triangle.2.circlepath", tint: WorkspaceColors.accent, size: 38)
                        WorkspaceSectionHeader(title: "软件更新")
                    }
                    if updateChecker.isUpdateServiceConfigured {
                        updateControls
                    } else {
                        Label("更新服务尚未配置", systemImage: "arrow.down.circle")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .padding(viewport.isCompactHeight ? 10 : 14)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(WorkspaceColors.inset, in: RoundedRectangle(cornerRadius: 12))
                    }
                }
                .padding(viewport.cardPadding)
                .frame(maxWidth: .infinity, alignment: .leading)
                .workspaceCard()
    }

    private var licenseCard: some View {
                HStack(alignment: .top, spacing: 12) {
                    WorkspaceSymbol(systemName: "doc.text", tint: WorkspaceColors.accent, size: 38)
                    VStack(alignment: .leading, spacing: 8) {
                        Text("许可证").font(.system(size: 16, weight: .semibold))
                        Text("PolyForm Noncommercial License 1.0.0（仅限非商业用途）")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer(minLength: 0)
                }
                .padding(viewport.cardPadding)
                .frame(maxWidth: .infinity, alignment: .leading)
                .workspaceCard()
    }

    private var updateControls: some View {
        VStack(alignment: .leading, spacing: viewport.isCompactHeight ? 10 : 18) {
            HStack(spacing: 24) {
                Text("更新渠道").font(.callout).foregroundStyle(.secondary)
                Picker("更新渠道", selection: $updateChecker.channel) {
                    ForEach(UpdateChecker.Channel.allCases) { channel in
                        Text(channel.rawValue).tag(channel)
                    }
                }
                .labelsHidden()
                .pickerStyle(.segmented)
                .frame(maxWidth: 280)
                Spacer(minLength: 0)
            }
            .compatibleOnChange(of: updateChecker.channel) { _ in
                Task { await updateChecker.check() }
            }

            HStack(spacing: 20) {
                VStack(alignment: .leading, spacing: 6) {
                    Text("最新版本").font(.caption).foregroundStyle(.secondary)
                    Text(updateChecker.latestVersionString)
                        .font(.system(size: 17, weight: .semibold).monospacedDigit())
                        .foregroundStyle(updateChecker.hasNewerVersion ? WorkspaceColors.accent : Color.primary)
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
                        Label(updateChecker.hasNewerVersion ? "查看更新" : "检查更新", systemImage: updateChecker.hasNewerVersion ? "arrow.up.right" : "arrow.clockwise")
                    }
                }
                .buttonStyle(.proletariusPrimary)
                .disabled(updateChecker.checking || !updateChecker.isUpdateServiceConfigured)
                .help(updateChecker.isUpdateServiceConfigured
                    ? (updateChecker.hasNewerVersion ? "点击查看更新" : "点击检查更新")
                    : "更新服务尚未配置")
            }
            .padding(viewport.isCompactHeight ? 12 : 18)
            .background(WorkspaceColors.inset, in: RoundedRectangle(cornerRadius: 12))

            LabeledContent("上次检查", value: updateChecker.lastCheckDate?.formatted(date: .abbreviated, time: .shortened) ?? "从未")
                .font(.callout)
            statusRow
            Divider()
            Toggle("启动时自动检查更新", isOn: $updateChecker.autoCheck)
                .disabled(!updateChecker.isUpdateServiceConfigured)
                .font(.callout)
                .toggleStyle(.switch)
        }
    }

    // MARK: - 更新状态

    @ViewBuilder
    private var statusRow: some View {
        if !updateChecker.isUpdateServiceConfigured {
            Text("更新服务尚未配置")
                .foregroundStyle(.secondary)
        } else if let error = updateChecker.lastError {
            Text(error)
                .foregroundStyle(.red)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private var telegramURL: URL? {
        let value = ExternalServiceConfiguration.telegramURL
            .trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty,
              let url = URL(string: value),
              url.scheme?.lowercased() == "https" else {
            return nil
        }
        return url
    }

    // MARK: - 构建时间

    private var buildDateText: String {
        guard let date = buildDate else { return "-" }
        return date.formatted(date: .abbreviated, time: .omitted)
    }

    private var buildDate: Date? {
        guard let path = Bundle.main.executablePath else { return nil }
        let attrs = try? FileManager.default.attributesOfItem(atPath: path)
        return attrs?[.modificationDate] as? Date
    }
}
