import Foundation

/// 网关 Origin 持久化：对齐 Android `base_url_store.dart`（键 `service_origin`）。
protocol GatewayOriginStoreing: AnyObject {
    func read() throws -> URL
    func write(_ origin: URL) throws
}

final class UserDefaultsGatewayOriginStore: GatewayOriginStoreing {
    static let shared = UserDefaultsGatewayOriginStore()

    private static let originKey = "service_origin"
    private let defaults: UserDefaults

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    func read() throws -> URL {
        guard let stored = defaults.string(forKey: Self.originKey) else {
            return BaseUrlPolicy.defaultOrigin
        }
        return try BaseUrlPolicy.parseAndValidate(stored)
    }

    func write(_ origin: URL) throws {
        let normalized = try BaseUrlPolicy.parseAndValidate(origin.absoluteString)
        defaults.set(normalized.absoluteString, forKey: Self.originKey)
    }
}

/// 进程内缓存，避免每次请求都打 UserDefaults。
final class CachedGatewayOriginStore: GatewayOriginStoreing {
    private let inner: GatewayOriginStoreing
    private var cached: URL?

    init(_ inner: GatewayOriginStoreing) {
        self.inner = inner
    }

    func read() throws -> URL {
        if let cached { return cached }
        let value = try inner.read()
        cached = value
        return value
    }

    func write(_ origin: URL) throws {
        let normalized = try BaseUrlPolicy.parseAndValidate(origin.absoluteString)
        try inner.write(normalized)
        cached = normalized
    }
}
