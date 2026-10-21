import Foundation

/// HTTPS settings / certificate status DTO.
/// Field set follows WEB `/api/settings/https` + Android `HttpsStateDto`.
/// Read-only for MAC-P5; no upload/reissue/delete in this ticket.
struct RemoteHttpsState: Equatable {
    let enabled: Bool
    let httpUrl: String
    let httpsUrl: String
    let fingerprint: String?
    let notAfter: Date?
    let source: String?
    let names: [String]
    let coversHost: Bool
    let host: String?

    var hasCertificateMaterial: Bool {
        fingerprint != nil || notAfter != nil || source != nil || !names.isEmpty
    }

    var displayCommonName: String {
        if let first = names.first, !first.isEmpty { return first }
        if let host, !host.isEmpty { return host }
        return "—"
    }

    var displaySource: String {
        switch (source ?? "").lowercased() {
        case "custom":
            return "自定义"
        case "self_signed", "self-signed", "selfsigned":
            return "自签名"
        case "":
            return "—"
        default:
            return source ?? "—"
        }
    }

    static func parse(_ value: Any) throws -> RemoteHttpsState {
        let data = try gatewayMap(value)
        let namesRaw = data["names"] as? [Any] ?? []
        let names = namesRaw.compactMap { item -> String? in
            guard let text = item as? String else { return nil }
            let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
            return trimmed.isEmpty ? nil : trimmed
        }
        let fingerprintRaw = (try? gatewayString(data["fingerprint"]))?
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let sourceRaw = (try? gatewayString(data["source"]))?
            .trimmingCharacters(in: .whitespacesAndNewlines)
        let hostRaw = (try? gatewayString(data["host"]))?
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return RemoteHttpsState(
            enabled: (try? gatewayBool(data["enabled"])) ?? false,
            httpUrl: (try? gatewayString(data["http_url"])) ?? "",
            httpsUrl: (try? gatewayString(data["https_url"])) ?? "",
            fingerprint: (fingerprintRaw?.isEmpty == false) ? fingerprintRaw : nil,
            notAfter: gatewayOptionalDate(data["not_after"]),
            source: (sourceRaw?.isEmpty == false) ? sourceRaw : nil,
            names: names,
            coversHost: (try? gatewayBool(data["covers_host"])) ?? false,
            host: (hostRaw?.isEmpty == false) ? hostRaw : nil
        )
    }
}
