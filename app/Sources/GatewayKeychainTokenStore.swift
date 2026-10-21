import Foundation
import Security

/// Bearer `session_token` Keychain 存储。对齐 Android `SecureTokenStore`（按 Origin 分键；禁止打日志）。
protocol GatewayTokenStoreing: AnyObject {
    func read(_ origin: URL) throws -> String?
    func write(_ origin: URL, token: String) throws
    func clear(_ origin: URL) throws
    func clearAll() throws
}

final class GatewayKeychainTokenStore: GatewayTokenStoreing {
    static let shared = GatewayKeychainTokenStore()

    private let service = "com.oneims.onewifi.mac.gateway.bearer"

    func read(_ origin: URL) throws -> String? {
        let account = try accountKey(for: origin)
        var query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = item as? Data else {
            throw GatewayKeychainError.osStatus(status)
        }
        return String(data: data, encoding: .utf8)
    }

    func write(_ origin: URL, token: String) throws {
        let account = try accountKey(for: origin)
        let data = Data(token.utf8)
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updateStatus = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if updateStatus == errSecSuccess { return }
        if updateStatus == errSecItemNotFound {
            var add = query
            add[kSecValueData as String] = data
            add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            let addStatus = SecItemAdd(add as CFDictionary, nil)
            guard addStatus == errSecSuccess else { throw GatewayKeychainError.osStatus(addStatus) }
            return
        }
        throw GatewayKeychainError.osStatus(updateStatus)
    }

    func clear(_ origin: URL) throws {
        let account = try accountKey(for: origin)
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        let status = SecItemDelete(query as CFDictionary)
        if status == errSecSuccess || status == errSecItemNotFound { return }
        throw GatewayKeychainError.osStatus(status)
    }

    func clearAll() throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
        ]
        let status = SecItemDelete(query as CFDictionary)
        if status == errSecSuccess || status == errSecItemNotFound { return }
        throw GatewayKeychainError.osStatus(status)
    }

    private func accountKey(for origin: URL) throws -> String {
        let normalized = try BaseUrlPolicy.parseAndValidate(origin.absoluteString)
        let encoded = Data(normalized.absoluteString.utf8).base64EncodedString()
            .replacingOccurrences(of: "=", with: "")
        return "bearer_token.\(encoded)"
    }
}

enum GatewayKeychainError: LocalizedError {
    case osStatus(OSStatus)

    var errorDescription: String? {
        switch self {
        case .osStatus(let status):
            return "无法读写网关凭证（Keychain \(status)）"
        }
    }
}

/// 进程内缓存，避免每个请求都打 Keychain。
final class CachedGatewayTokenStore: GatewayTokenStoreing {
    private let inner: GatewayTokenStoreing
    /// 仅缓存非空 token；缺失键表示未命中（避免 `[String: String?]` 的 nil 语义坑）。
    private var cache: [String: String] = [:]
    private var knownEmpty: Set<String> = []

    init(_ inner: GatewayTokenStoreing) {
        self.inner = inner
    }

    func read(_ origin: URL) throws -> String? {
        let key = try cacheKey(origin)
        if let cached = cache[key] { return cached }
        if knownEmpty.contains(key) { return nil }
        let value = try inner.read(origin)
        if let value, !value.isEmpty {
            cache[key] = value
            knownEmpty.remove(key)
        } else {
            knownEmpty.insert(key)
            cache.removeValue(forKey: key)
        }
        return value
    }

    func write(_ origin: URL, token: String) throws {
        try inner.write(origin, token: token)
        let key = try cacheKey(origin)
        cache[key] = token
        knownEmpty.remove(key)
    }

    func clear(_ origin: URL) throws {
        try inner.clear(origin)
        let key = try cacheKey(origin)
        cache.removeValue(forKey: key)
        knownEmpty.insert(key)
    }

    func clearAll() throws {
        try inner.clearAll()
        cache.removeAll()
        knownEmpty.removeAll()
    }

    private func cacheKey(_ origin: URL) throws -> String {
        try BaseUrlPolicy.parseAndValidate(origin.absoluteString).absoluteString
    }
}
