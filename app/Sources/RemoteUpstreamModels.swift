import Foundation

/// Upstream proxy DTO: field set follows WEB `proxy_api` + Android `UpstreamProxy`.
/// No invented keys. Distinct from local Mihomo / Switch front proxy.
struct RemoteUpstreamProxy: Identifiable, Equatable, Hashable {
    let id: String
    let name: String
    let addr: String
    let enabled: Bool

    static func parse(_ value: Any) throws -> RemoteUpstreamProxy {
        let data = try gatewayMap(value)
        let id = try gatewayString(data["id"]).trimmingCharacters(in: .whitespacesAndNewlines)
        guard !id.isEmpty else { throw GatewayHTTPError.dtoShape }
        let nameRaw = (try? gatewayString(data["name"])) ?? ""
        let addrRaw = (try? gatewayString(data["addr"]))
            ?? (try? gatewayString(data["server"]))
            ?? ""
        let enabled = (try? gatewayBool(data["enabled"])) ?? true
        return RemoteUpstreamProxy(
            id: id,
            name: nameRaw.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                ? id
                : nameRaw.trimmingCharacters(in: .whitespacesAndNewlines),
            addr: addrRaw.trimmingCharacters(in: .whitespacesAndNewlines),
            enabled: enabled
        )
    }

    static func parseList(_ value: Any) throws -> [RemoteUpstreamProxy] {
        if value is NSNull { return [] }
        guard let array = value as? [Any] else { throw GatewayHTTPError.dtoShape }
        return try array.map { try parse($0) }
    }
}
