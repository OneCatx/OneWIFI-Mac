import SwiftUI

/// Read-only device overview for `GET /api/devices/{id}/overview`.
struct RemoteDeviceOverviewView: View {
    let deviceId: String

    @ObservedObject private var store = RemoteDashboardStore.shared
    @Environment(\.workspaceViewport) private var viewport

    var body: some View {
        Group {
            switch store.detailState {
            case .idle, .loading:
                ProgressView("正在加载设备 overview…")
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            case .failed(let message):
                VStack(spacing: 14) {
                    Image(systemName: "exclamationmark.triangle")
                        .font(.system(size: 36))
                        .foregroundStyle(.orange)
                    Text("无法加载设备 overview")
                        .font(.title3)
                    Text(message)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .frame(maxWidth: 400)
                    Button("重试") {
                        Task { await store.selectDevice(deviceId) }
                    }
                    .buttonStyle(.borderedProminent)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .padding(24)
            case .empty:
                Text("无 overview 数据")
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            case .loaded:
                if let detail = store.selectedDetail, detail.id == deviceId {
                    detailScroll(detail)
                } else {
                    ProgressView("正在加载设备 overview…")
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .navigationTitle(store.selectedDetail?.name.isEmpty == false ? (store.selectedDetail?.name ?? deviceId) : deviceId)
        .task(id: deviceId) {
            await store.selectDevice(deviceId)
        }
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button {
                    Task { await store.reloadSelectedOverview() }
                } label: {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
                .labelStyle(.iconOnly)
                .buttonStyle(.proletariusToolbar)
                .help("刷新设备 overview")
            }
            .compatibleSharedToolbarBackgroundHidden()
        }
    }

    private func detailScroll(_ detail: RemoteDeviceOverviewDetail) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: viewport.sectionSpacing) {
                identityCard(detail)
                runtimeCard(detail)
                if let traffic = detail.traffic {
                    trafficCard(traffic)
                }
            }
            .padding(viewport.pageInset)
            .frame(maxWidth: 900)
            .frame(maxWidth: .infinity)
        }
    }

    private func identityCard(_ detail: RemoteDeviceOverviewDetail) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("设备")
                .font(.headline)
            detailGrid([
                ("ID", detail.id),
                ("类型", detail.deviceType),
                ("健康", detail.healthy ? "正常" : "异常"),
                ("物理在位", detail.physicalPresent ? "是" : "否"),
                ("控制在线", detail.controlOnline ? "是" : "否"),
                ("网络", detail.networkConnected ? "已连接" : "未连接"),
                ("VoWiFi", detail.vowifiActive ? "活跃" : (detail.vowifiEnabled ? "已启用" : "未启用")),
                ("运营商", emptyDash(detail.operatorName)),
                ("信号", detail.signalDbm == 0 ? "—" : "\(detail.signalDbm) dBm"),
                ("网络制式", emptyDash(detail.networkMode)),
                ("调制解调器", emptyDash(detail.modemModel)),
                ("公网 IP", emptyDash(detail.publicIP)),
                ("接口", emptyDash(detail.interfaceName)),
                ("本机号码", emptyDash(detail.localPhone)),
            ])
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func runtimeCard(_ detail: RemoteDeviceOverviewDetail) -> some View {
        let runtime = detail.runtime
        return VStack(alignment: .leading, spacing: 10) {
            Text("VoWiFi 运行时")
                .font(.headline)
            detailGrid([
                ("阶段", runtime.phase.displayName),
                ("SIM", runtime.simReady ? "就绪" : "未就绪"),
                ("接入", runtime.accessReady ? "就绪" : "未就绪"),
                ("隧道", runtime.tunnelReady ? "就绪" : "未就绪"),
                ("IMS", runtime.imsReady ? "就绪" : "未就绪"),
                ("短信", runtime.smsReady ? "就绪" : "未就绪"),
            ])
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func trafficCard(_ traffic: RemoteDeviceTraffic) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("流量")
                .font(.headline)
            detailGrid([
                ("下行", traffic.rx),
                ("上行", traffic.tx),
                ("下行速率", traffic.rate),
                ("上行速率", traffic.rateTx),
                ("状态", emptyDash(traffic.status ?? "")),
            ])
        }
        .padding(viewport.cardPadding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .workspaceCard()
    }

    private func detailGrid(_ rows: [(String, String)]) -> some View {
        LazyVGrid(
            columns: [
                GridItem(.adaptive(minimum: 200), spacing: 12, alignment: .topLeading),
            ],
            alignment: .leading,
            spacing: 10
        ) {
            ForEach(Array(rows.enumerated()), id: \.offset) { _, row in
                VStack(alignment: .leading, spacing: 2) {
                    Text(row.0)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(row.1)
                        .font(.callout)
                        .textSelection(.enabled)
                }
            }
        }
    }

    private func emptyDash(_ value: String) -> String {
        value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "—" : value
    }
}
