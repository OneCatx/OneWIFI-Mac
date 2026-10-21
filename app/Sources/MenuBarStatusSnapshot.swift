import Foundation
import Combine

/// Presentation rules shared by the panel and its offline regression tests.
/// A running local service does not imply that either device is online.
enum MenuBarStatusPolicy {
    static func moduleStatus(serviceRunning: Bool, hasStatus: Bool, stale: Bool,
                             hardwareIssue: Bool, hardwarePresent: Bool,
                             simInserted: Bool?) -> String {
        guard serviceRunning else { return "状态未知" }
        if stale { return "数据过期" }
        guard hasStatus else { return "正在读取" }
        if hardwareIssue || !hardwarePresent { return "未连接" }
        if simInserted == false { return "SIM 未插入" }
        return "已连接"
    }

    static func imsStatus(loaded: Bool, failed: Bool, registered: Bool,
                          smsReady: Bool?, callReady: Bool?, connecting: Bool) -> String {
        guard loaded else { return "读取中" }
        if failed { return "异常" }
        if registered { return smsReady == true && callReady == true ? "已就绪" : "恢复中" }
        return connecting ? "连接中" : "未启用"
    }
}
