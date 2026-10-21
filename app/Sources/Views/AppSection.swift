import SwiftUI

/// Navigation sections and their presentation metadata.
///
/// Keeping this value type separate from ContentView prevents the root view
/// from becoming the owner of unrelated navigation styling and makes the
/// section list reusable by the menu-bar and state summary views.
enum AppSection: String, CaseIterable, Identifiable {
    case activity = "活动"
    case home = "模块"
    case reader = "读卡器"
    case multistandby = "N待"
    case sms = "本机短信"
    case calls = "本机通话"
    case remoteHub = "远端仪表盘"
    case remoteSms = "远端短信"
    case remoteCalls = "远端通话"
    case remoteProxy = "远端代理"
    case remoteCert = "远端证书"
    case debug = "调试与诊断"
    case about = "关于与更新"

    var id: String { rawValue }

    var accent: Color {
        switch self {
        case .activity: return ProletariusPalette.activity
        case .home: return ProletariusPalette.module
        case .reader: return ProletariusPalette.reader
        case .multistandby: return ProletariusPalette.reader
        case .sms: return ProletariusPalette.messages
        case .calls: return ProletariusPalette.calls
        case .remoteHub: return ProletariusPalette.about
        case .remoteSms: return ProletariusPalette.messages
        case .remoteCalls: return ProletariusPalette.calls
        case .remoteProxy: return ProletariusPalette.diagnostics
        case .remoteCert: return ProletariusPalette.about
        case .debug: return ProletariusPalette.diagnostics
        case .about: return ProletariusPalette.about
        }
    }

    var subtitle: String {
        switch self {
        case .activity: return "运行状态与实时活动"
        case .home: return "DJI 模块与蜂窝状态"
        case .reader: return "PC/SC、eSIM 与 Wi-Fi Calling"
        case .multistandby: return "多号码在线管理"
        case .sms: return "本机模块与 VoWiFi 消息"
        case .calls: return "本机蜂窝语音工作台"
        case .remoteHub: return "远端仪表盘与远端设备入口"
        case .remoteSms: return "远端网关短信会话"
        case .remoteCalls: return "远端通话控制面（无 WebRTC）"
        case .remoteProxy: return "远端上游代理列表与启停"
        case .remoteCert: return "远端 HTTPS 证书状态（只读）"
        case .debug: return "运行证据与诊断工具"
        case .about: return "版本与应用设置"
        }
    }

    var icon: String {
        switch self {
        case .activity: return "waveform.path.ecg.rectangle"
        case .home: return "antenna.radiowaves.left.and.right"
        case .reader: return "creditcard.and.123"
        case .multistandby: return "Mobius"
        case .sms: return "message"
        case .calls: return "phone"
        case .remoteHub: return "network"
        case .remoteSms: return "message.badge"
        case .remoteCalls: return "phone.badge.waveform"
        case .remoteProxy: return "point.3.connected.trianglepath.dotted"
        case .remoteCert: return "lock.shield"
        case .debug: return "stethoscope"
        case .about: return "info.circle"
        }
    }
}
