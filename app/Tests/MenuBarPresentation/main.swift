import AppKit
import SwiftUI

@main
private enum MenuBarPresentationTests {
    @MainActor
    static func main() throws {
        _ = NSApplication.shared
        NSApp.setActivationPolicy(.prohibited)
        MenuBarFixture.defaults.set(true, forKey: AppPreferenceKeys.privacyMaskEnabled)
        try verifySnapshotPolicy()
        let output = CommandLine.arguments.dropFirst().first.map(URL.init(fileURLWithPath:))
        if let output { try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true) }
        let backend = BackendProcess()
        let store = DashboardStore()
        let delegate = AppDelegate()
        let updates = UpdateChecker()
        let panel = MenuBarDashboardPanel(appDelegate: delegate, backend: backend, store: store,
            smsStore: SMSStore(), attentionStore: AttentionStore(), updateChecker: updates)
        precondition(panel.moduleStatusText == "已连接" && panel.readerIMSShort == "已就绪")
        precondition(panel.moduleDetailText?.contains("13800138000") == false)
        precondition(panel.readerDetailText?.contains("13900139000") == false)
        precondition(panel.moduleDetailText?.contains("•") == true)
        panel.privacyMaskEnabled = false
        precondition(panel.moduleDetailText?.contains("13800138000") == true)
        panel.privacyMaskEnabled = true
        store.voWiFiStatus?.state = "failed"
        precondition(panel.moduleIMSShort == "异常" && panel.readerIMSShort == "已就绪")
        store.voWiFiStatus?.state = "ready"
        store.status?.simInserted = false
        delegate.bindDashboardStore(store)
        precondition(panel.moduleStatusText == "SIM 未插入")
        precondition(delegate.menuBarPresentation.networkSummary.contains("读卡器"))
        precondition(delegate.menuBarPresentation.networkSummary.contains("SIM 未插入"))
        store.status = DeviceStatus(hardwareStatus: nil, usbDevice: nil, imei: nil, simInserted: nil,
            operatorName: nil, phoneNumber: nil, networkMode: nil, signalDbm: nil, radioBand: nil)
        store.readerConnected = false
        delegate.bindDashboardStore(store)
        precondition(panel.moduleStatusText == "未连接")
        precondition(delegate.menuBarPresentation.networkSummary == "模块状态待确认")

        var images = 0
        for scheme: ColorScheme in [.light, .dark] {
            for scenario in ["ready", "stale-module", "module-absent", "offline"] {
                backend.state = scenario == "offline" ? .stopped : .running
                store.status = DeviceStatus()
                store.statusStale = scenario == "stale-module"
                store.readerConnected = true
                if scenario == "module-absent" { store.status?.hardwareStatus = "未检测到 USB 模块" }
                delegate.downloadBytesPerSecond = 4096
                delegate.uploadBytesPerSecond = 1024
                delegate.bindDashboardStore(store)
                if scenario == "stale-module" {
                    precondition(panel.moduleStatusText == "数据过期")
                    precondition(panel.readerStatusText == "已连接" && panel.readerIMSShort == "已就绪")
                    precondition(delegate.menuBarPresentation.networkSummary.contains("读卡器"))
                    precondition(delegate.menuBarPresentation.networkSummary.contains("模块数据过期"))
                    precondition(delegate.menuBarPresentation.downloadRate == "—")
                }
                if scenario == "offline" {
                    precondition(panel.moduleStatusText == "状态未知" && panel.readerStatusText == "状态未知")
                    precondition(panel.moduleIMSShort == "状态未知" && panel.readerIMSShort == "状态未知")
                    precondition(panel.callStatusText == "状态未知")
                    precondition(panel.displayedDownloadRate == "—" && panel.displayedUploadRate == "—")
                }
                let image = try render(panel.environment(\.colorScheme, scheme))
                if let output {
                    let name = "menu-\(scenario)-\(scheme == .dark ? "dark" : "light").png"
                    try NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:])!
                        .write(to: output.appendingPathComponent(name))
                }
                images += 1
            }
        }
        let generation = MainWindowRequestCenter.shared.generation
        for section in [AppSection.home, .reader, .sms, .calls] {
            panel.navigate(to: section)
            precondition(MainWindowRequestCenter.shared.destination == section)
        }
        precondition(MainWindowRequestCenter.shared.generation == generation + 4)
        precondition(MenuBarFixture.actions.isEmpty, "rendering/navigation must not perform business or settings actions")
        let badge = makeMenuBarAttentionBadgeImage(count: 1)
        let wideBadge = makeMenuBarAttentionBadgeImage(count: 999)
        precondition(badge.isTemplate && badge.size.width < 12 && wideBadge.size.width < 25)
        print("Compact count: 1 = \(badge.size.width)pt; 99+ = \(wideBadge.size.width)pt; no background capsule")
        for mask in 0..<8 {
            MenuBarFixture.defaults.set(mask & 1 != 0, forKey: MenuBarDisplayOptions.showSignalKey)
            MenuBarFixture.defaults.set(mask & 2 != 0, forKey: MenuBarDisplayOptions.showDownloadKey)
            MenuBarFixture.defaults.set(mask & 4 != 0, forKey: MenuBarDisplayOptions.showUploadKey)
            let options = MenuBarDisplayOptions(defaults: MenuBarFixture.defaults)
            let titles = options.rateTitles(download: "4 KB/s", upload: "1 KB/s")
            precondition((titles.download != nil) == (mask & 2 != 0))
            precondition((titles.upload != nil) == (mask & 4 != 0))
            precondition((titles.combined == nil) == (mask & 6 == 0))
        }
        print("Menu bar: \(images) native panel renders, 8 option combinations, privacy/navigation/stale independence/snapshot ownership checks passed; no production services linked")
        if let output { print("Render images: \(output.path)") }
    }

    @MainActor
    private static func verifySnapshotPolicy() throws {
        precondition(MenuBarStatusPolicy.moduleStatus(serviceRunning: true, hasStatus: true,
            stale: false, hardwareIssue: false, hardwarePresent: false, simInserted: nil) == "未连接")
        precondition(MenuBarStatusPolicy.imsStatus(loaded: true, failed: false, registered: true,
            smsReady: nil, callReady: nil, connecting: false) == "恢复中")
    }

    @MainActor
    private static func render<V: View>(_ view: V) throws -> CGImage {
        let host = NSHostingView(rootView: view)
        let size = host.fittingSize
        precondition(size.width == 320 && size.height < 680 && size.height > 300,
                     "panel must remain a compact 320pt surface, found \(size)")
        let bounds = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: bounds, styleMask: .borderless, backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.contentView = host
        host.frame = bounds
        host.layoutSubtreeIfNeeded()
        RunLoop.main.run(until: Date().addingTimeInterval(0.08))
        host.layoutSubtreeIfNeeded()
        defer { window.contentView = nil; window.close() }
        guard let bitmap = host.bitmapImageRepForCachingDisplay(in: bounds) else { throw NSError(domain: "MenuBarTests", code: 1) }
        host.cacheDisplay(in: bounds, to: bitmap)
        guard let image = bitmap.cgImage else { throw NSError(domain: "MenuBarTests", code: 2) }
        return image
    }
}
