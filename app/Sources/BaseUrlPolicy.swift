import Foundation

/// Mac 等价物：对齐 Android `base_url_policy.dart`。
/// 仅允许 http(s)、无 userInfo / path / query / fragment。
enum BaseUrlPolicy {
    static let defaultOrigin = URL(string: "http://192.168.1.88:6699")!
    static let backendDefaultListen = "0.0.0.0:6688"

    enum PolicyError: LocalizedError, Equatable {
        case empty
        case invalid
        case schemeOrHost
        case userInfo
        case path
        case queryOrFragment
        case emptyHost
        case emptyPort
        case invalidPort

        var errorDescription: String? {
            switch self {
            case .empty: return "服务地址不能为空。"
            case .invalid: return "服务地址无效。"
            case .schemeOrHost: return "服务地址必须是 http:// 或 https://。"
            case .userInfo: return "服务地址不能包含用户名或密码。"
            case .path: return "服务地址不能包含路径。"
            case .queryOrFragment: return "服务地址不能包含查询参数或片段。"
            case .emptyHost: return "主机不能为空。"
            case .emptyPort: return "端口不能为空。"
            case .invalidPort: return "端口范围 1–65535。"
            }
        }
    }

    static func parseAndValidate(_ input: String) throws -> URL {
        let candidate = input.trimmingCharacters(in: .whitespacesAndNewlines)
        if candidate.isEmpty { throw PolicyError.empty }

        guard var components = URLComponents(string: candidate) else {
            throw PolicyError.invalid
        }

        let scheme = (components.scheme ?? "").lowercased()
        guard scheme == "http" || scheme == "https",
              let host = components.host, !host.isEmpty else {
            throw PolicyError.schemeOrHost
        }
        if let user = components.user, !user.isEmpty { throw PolicyError.userInfo }
        if let password = components.password, !password.isEmpty { throw PolicyError.userInfo }

        let path = components.path
        if !path.isEmpty && path != "/" { throw PolicyError.path }
        if components.query != nil || components.fragment != nil {
            throw PolicyError.queryOrFragment
        }

        let defaultPort = scheme == "https" ? 443 : 80
        components.scheme = scheme
        components.host = host.lowercased()
        components.user = nil
        components.password = nil
        components.path = ""
        components.query = nil
        components.fragment = nil
        if let port = components.port, port == defaultPort {
            components.port = nil
        }

        guard let url = components.url else { throw PolicyError.invalid }
        return url
    }

    static func fromHostAndPort(host: String, port: String, https: Bool = false) throws -> URL {
        let trimmedHost = host.trimmingCharacters(in: .whitespacesAndNewlines)
        let trimmedPort = port.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmedHost.isEmpty { throw PolicyError.emptyHost }
        if trimmedPort.isEmpty { throw PolicyError.emptyPort }
        guard let parsedPort = Int(trimmedPort), parsedPort >= 1, parsedPort <= 65535 else {
            throw PolicyError.invalidPort
        }
        let scheme = https ? "https" : "http"
        let hostForURI: String
        if trimmedHost.contains(":"), !trimmedHost.hasPrefix("[") {
            hostForURI = "[\(trimmedHost)]"
        } else {
            hostForURI = trimmedHost
        }
        return try parseAndValidate("\(scheme)://\(hostForURI):\(parsedPort)")
    }

    static func split(_ origin: URL) -> (host: String, port: String, https: Bool) {
        let https = (origin.scheme ?? "").lowercased() == "https"
        let port = origin.port ?? (https ? 443 : 80)
        return (host: origin.host ?? "", port: "\(port)", https: https)
    }

    static func isPublicCleartext(_ origin: URL) -> Bool {
        (origin.scheme ?? "").lowercased() == "http"
            && !isLoopbackOrPrivateIPLiteral(origin.host ?? "")
    }

    static func isLoopbackOrPrivateIPLiteral(_ host: String) -> Bool {
        let normalized = host.lowercased()
        if normalized == "localhost" || normalized.hasSuffix(".localhost") {
            return true
        }
        if let ipv4 = parseIPv4(host) {
            let first = ipv4[0]
            let second = ipv4[1]
            return first == 127
                || first == 10
                || (first == 172 && second >= 16 && second <= 31)
                || (first == 192 && second == 168)
                || (first == 169 && second == 254)
        }
        if !normalized.contains(":") { return false }
        if normalized == "::1" { return true }
        if normalized.hasPrefix("fc") || normalized.hasPrefix("fd") { return true }
        return isIPv6LinkLocal(normalized)
    }

    private static func isIPv6LinkLocal(_ host: String) -> Bool {
        let first = host.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false).first.map(String.init) ?? ""
        let padded = first.padding(toLength: 4, withPad: "0", startingAt: 0)
        guard let nibble = Int(padded, radix: 16) else { return false }
        return nibble >= 0xfe80 && nibble <= 0xfebf
    }

    private static func parseIPv4(_ host: String) -> [Int]? {
        let parts = host.split(separator: ".", omittingEmptySubsequences: false).map(String.init)
        guard parts.count == 4 else { return nil }
        var octets: [Int] = []
        for part in parts {
            guard !part.isEmpty, part.range(of: "^\\d{1,3}$", options: .regularExpression) != nil,
                  let octet = Int(part), octet <= 255 else {
                return nil
            }
            octets.append(octet)
        }
        return octets
    }
}
