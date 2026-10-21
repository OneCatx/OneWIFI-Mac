import Foundation

/// 网关 HTTPS 信任策略（客户端本地偏好，非 WEB API）。
/// 仅影响 `GatewayHTTPClient` 的 `URLSession` 委托；不碰本机 UDS `APIClient`。
enum GatewayTrustPolicy {
    /// 默认开启：与 MAC-P2 私网自签放宽口径一致；用户可关闭以强制系统校验。
    static let allowPrivateHostRelaxedTrustKey = "gatewayAllowPrivateHostRelaxedTrust"

    static var allowPrivateHostRelaxedTrust: Bool {
        get {
            let defaults = UserDefaults.standard
            if defaults.object(forKey: allowPrivateHostRelaxedTrustKey) == nil {
                return true
            }
            return defaults.bool(forKey: allowPrivateHostRelaxedTrustKey)
        }
        set {
            UserDefaults.standard.set(newValue, forKey: allowPrivateHostRelaxedTrustKey)
        }
    }

    /// 是否对指定主机放宽服务端信任（仅 loopback / 私网 IP 字面量）。
    static func shouldRelaxTrust(forHost host: String) -> Bool {
        guard allowPrivateHostRelaxedTrust else { return false }
        return BaseUrlPolicy.isLoopbackOrPrivateIPLiteral(host)
    }
}
