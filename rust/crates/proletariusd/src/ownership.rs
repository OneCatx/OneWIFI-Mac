use proletarius_api_contract::{MigrationComponent, MigrationReadinessResponse};

pub(crate) const OWNERSHIP_SCHEMA_VERSION: u16 = 2;

const COMPONENTS: &[MigrationComponent<'static>] = &[
    MigrationComponent {
        id: "gateway",
        owner: "rust",
        state: "native",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "Unix socket, authentication, supervision and recovery admission",
    },
    MigrationComponent {
        id: "device_snapshot",
        owner: "rust",
        state: "aggregate-shadow",
        requires_go_runtime: true,
        blocks_go_removal: true,
        detail: "Schema-versioned aggregate read path; Rust owns PC/SC inventory and ICCID verification while module, remaining reader fields and VoWiFi session payloads still come from Go",
    },
    MigrationComponent {
        id: "dji_module",
        owner: "go",
        state: "compatibility",
        requires_go_runtime: true,
        blocks_go_removal: true,
        detail: "USB/QMI, eSIM, SMS, VoLTE calls and module recovery are not Rust-owned",
    },
    MigrationComponent {
        id: "reader_inventory",
        owner: "rust",
        state: "native",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "PC/SC enumeration and revisioned discovery state are Rust-owned; Go reconciliation is bounded compatibility assistance only",
    },
    MigrationComponent {
        id: "reader_session",
        owner: "go",
        state: "protocol-shadow",
        requires_go_runtime: true,
        blocks_go_removal: true,
        detail: "eSIM profile and mutating/general APDU sessions remain on Go; read-only identity and per-reader AKA transactions use Rust PC/SC endpoints",
    },
    MigrationComponent {
        id: "reader_identity",
        owner: "rust",
        state: "native-read-only",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "Rust discovers the complete USIM AID from EF-DIR and reads EF-ICCID/length-bounded EF-IMSI plus optional EF-GID1 in one transaction; EF-AD is optional and never guessed",
    },
    MigrationComponent {
        id: "reader_vowifi",
        owner: "go",
        state: "protocol-shadow",
        requires_go_runtime: true,
        blocks_go_removal: true,
        detail: "Rust shadow path implements proxy selection, SOCKS UDP, configured GCM and CBC/group-14 startup, multi-round EAP-AKA, final AUTH/CHILD/TS/config verification, ESP, liveness, peer IKE DELETE and bounded active local IKE DELETE with local end-to-end fault tests; the prepared native context binds IKE AKA to a generation-bound Go USB/BLE lease and the gated tunnel-start path renews that lease every 30 seconds with bounded retries. The diagnostic plan still releases its lease after preparation; keyed live ownership, protected IMS/SMS/call integration and carrier hardware acceptance remain gated and block Go removal",
    },
    MigrationComponent {
        id: "message_archive",
        owner: "rust",
        state: "native-store",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "Rust owns the locked atomic local SMS archive API; Go producers share the same disk lock",
    },
    MigrationComponent {
        id: "message_runtime",
        owner: "go",
        state: "compatibility-producer",
        requires_go_runtime: true,
        blocks_go_removal: true,
        detail: "Live IMS/module SMS production, device routing and SIM/ME storage mutation are not Rust-owned",
    },
    MigrationComponent {
        id: "sms_rp_codec",
        owner: "rust",
        state: "native",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "Reader RP-DATA, SMS-DELIVER and SMS-STATUS-REPORT codecs use authenticated native routes; live IMS transmission remains Go-owned",
    },
    MigrationComponent {
        id: "call_archive",
        owner: "rust",
        state: "native",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "Rust owns call history list, clear and delete with a locked atomic JSON store",
    },
    MigrationComponent {
        id: "mac_audio",
        owner: "swift",
        state: "native",
        requires_go_runtime: false,
        blocks_go_removal: false,
        detail: "CoreAudio device selection, AirPods routing and microphone/speaker bridge remain native Swift",
    },
];

pub(crate) fn readiness() -> MigrationReadinessResponse<'static> {
    let rust_owned_components = COMPONENTS
        .iter()
        .filter(|component| {
            component.owner == "rust"
                && !component.requires_go_runtime
                && !component.blocks_go_removal
        })
        .count();
    let go_blocking_components = COMPONENTS
        .iter()
        .filter(|component| component.requires_go_runtime || component.blocks_go_removal)
        .count();
    let total = COMPONENTS.len();
    let rust_ownership_percent = rust_owned_components
        .checked_mul(100)
        .and_then(|value| value.checked_div(total))
        .unwrap_or(0)
        .min(100) as u8;
    MigrationReadinessResponse {
        schema_version: OWNERSHIP_SCHEMA_VERSION,
        ready_to_remove_go: COMPONENTS
            .iter()
            .all(|component| !component.blocks_go_removal && !component.requires_go_runtime),
        rust_owned_components,
        go_blocking_components,
        rust_ownership_percent,
        components: COMPONENTS,
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RouteOwner {
    Rust,
    GoCompatibility,
}

pub(crate) fn route_owner(method: &str, path: &str) -> RouteOwner {
    if (method == "GET"
        && matches!(
            path,
            "/api/multistandby/messages" | "/api/multistandby/changes"
        ))
        || (method == "DELETE" && path.starts_with("/api/multistandby/messages/"))
        || path.starts_with(crate::multistandby_store::INTERNAL_PREFIX)
        || path.starts_with(crate::reader_reservation::PREFIX)
    {
        return RouteOwner::Rust;
    }
    // Rust-owned GET routes
    if method == "GET"
        && matches!(
            path,
            "/api/v2/runtime"
                | "/api/v2/device-snapshot"
                | "/api/v2/migration-readiness"
                | "/api/v2/sms-archive"
                | "/api/reader/list"
                | "/api/calls"
        )
    {
        return RouteOwner::Rust;
    }
    // Rust-owned POST routes
    if method == "POST"
        && matches!(
            path,
            "/api/v2/carrier/resolve"
                | "/api/v2/call/dtmf"
                | "/api/v2/ims/validate-register-response"
                | "/api/v2/reader/authenticate"
                | "/api/v2/reader/eap-aka"
                | "/api/v2/reader/identity"
                | "/api/v2/vowifi/eap-identity"
                | "/api/v2/vowifi/eap-exchange/release"
                | "/api/v2/vowifi/proxy-policy"
                | "/api/v2/vowifi/proxy-probe"
                | "/api/v2/vowifi/select-transport"
                | "/api/v2/vowifi/session-policy"
                | "/api/v2/vowifi/native-session-plan"
                | "/api/v2/sms/encode-rpdu"
                | "/api/v2/sms/decode-rpdu"
                | "/api/v2/sms/prepare-tpdu"
                | "/api/v2/sms/decode-incoming-tpdu"
                | "/api/calls/clear"
                | "/api/calls/delete"
                | "/api/v2/sms-archive/clear"
                | "/api/v2/sms-archive/delete"
                | "/api/v2/sms-archive/delete-sender"
        )
    {
        return RouteOwner::Rust;
    }
    RouteOwner::GoCompatibility
}

#[cfg(test)]
mod tests {
    use super::{COMPONENTS, RouteOwner, readiness, route_owner};
    use std::collections::HashSet;

    #[test]
    fn manifest_ids_are_unique_and_go_removal_is_fail_closed() {
        let ids = COMPONENTS
            .iter()
            .map(|component| component.id)
            .collect::<HashSet<_>>();
        assert_eq!(ids.len(), COMPONENTS.len());
        assert!(!readiness().ready_to_remove_go);
        let progress = readiness();
        assert!(
            progress.rust_owned_components + progress.go_blocking_components <= COMPONENTS.len()
        );
        assert!(progress.rust_ownership_percent > 0);
        assert!(progress.rust_ownership_percent < 100);
        // Keep the published migration baseline honest: changes to component
        // ownership must update the plan and its cutover evidence together.
        assert_eq!(progress.rust_owned_components, 6);
        assert_eq!(progress.go_blocking_components, 5);
        assert_eq!(progress.rust_ownership_percent, 50);
        assert!(COMPONENTS.iter().any(|component| {
            component.id == "reader_vowifi"
                && component.owner == "go"
                && component.requires_go_runtime
                && component.blocks_go_removal
        }));
        assert!(COMPONENTS.iter().any(|component| {
            component.id == "message_archive"
                && component.owner == "rust"
                && !component.requires_go_runtime
                && !component.blocks_go_removal
        }));
        assert!(COMPONENTS.iter().any(|component| {
            component.id == "message_runtime"
                && component.owner == "go"
                && component.requires_go_runtime
                && component.blocks_go_removal
        }));
        assert!(
            COMPONENTS
                .iter()
                .all(|component| { !component.requires_go_runtime || component.blocks_go_removal })
        );
    }

    #[test]
    fn only_native_v2_reads_are_rust_owned() {
        for path in [
            "/api/v2/runtime",
            "/api/v2/device-snapshot",
            "/api/v2/migration-readiness",
            "/api/v2/sms-archive",
            "/api/reader/list",
            "/api/calls",
        ] {
            assert_eq!(route_owner("GET", path), RouteOwner::Rust);
        }
        assert_eq!(
            route_owner("POST", "/api/v2/migration-readiness"),
            RouteOwner::GoCompatibility
        );
        for path in [
            "/api/calls/clear",
            "/api/v2/carrier/resolve",
            "/api/v2/call/dtmf",
            "/api/v2/ims/validate-register-response",
            "/api/v2/reader/authenticate",
            "/api/v2/reader/eap-aka",
            "/api/v2/reader/identity",
            "/api/v2/vowifi/eap-identity",
            "/api/v2/vowifi/eap-exchange/release",
            "/api/v2/vowifi/proxy-policy",
            "/api/v2/vowifi/proxy-probe",
            "/api/v2/vowifi/select-transport",
            "/api/v2/sms/encode-rpdu",
            "/api/v2/sms/decode-rpdu",
            "/api/v2/sms/prepare-tpdu",
            "/api/v2/sms/decode-incoming-tpdu",
            "/api/calls/delete",
            "/api/v2/sms-archive/clear",
            "/api/v2/sms-archive/delete",
            "/api/v2/sms-archive/delete-sender",
        ] {
            assert_eq!(route_owner("POST", path), RouteOwner::Rust);
        }
        for path in [
            "/api/vowifi",
            "/api/vowifi/result",
            "/api/reader/vowifi",
            "/api/reader/vowifi/calls/dial",
            "/api/reader/vowifi/calls/answer",
            "/api/reader/vowifi/calls/reject",
            "/api/reader/vowifi/calls/hangup",
            "/api/reader/vowifi/calls/dtmf",
            "/api/sms/send",
            "/api/call/dial",
            "/api/esim/switch",
            "/api/reader/esim/download",
            "/api/reader/esim/switch",
            "/api/reader/esim/profile",
            "/api/reader/vowifi/proxy-check",
        ] {
            assert_eq!(route_owner("GET", path), RouteOwner::GoCompatibility);
        }
        for path in [
            "/api/vowifi",
            "/api/reader/vowifi",
            "/api/reader/vowifi/calls/dial",
            "/api/reader/vowifi/calls/answer",
            "/api/reader/vowifi/calls/reject",
            "/api/reader/vowifi/calls/hangup",
            "/api/reader/vowifi/calls/dtmf",
            "/api/sms/send",
            "/api/reader/esim/download",
            "/api/reader/esim/switch",
            "/api/reader/esim/profile",
            "/api/reader/vowifi/proxy-check",
        ] {
            assert_eq!(route_owner("POST", path), RouteOwner::GoCompatibility);
        }
        for (method, path) in [
            ("PATCH", "/api/reader/esim/profile"),
            ("DELETE", "/api/reader/esim/profile"),
        ] {
            assert_eq!(route_owner(method, path), RouteOwner::GoCompatibility);
        }
    }

    #[test]
    fn component_owners_are_explicit() {
        let valid = HashSet::from(["rust", "go", "swift", "deprecated"]);
        assert!(
            COMPONENTS
                .iter()
                .all(|component| valid.contains(component.owner))
        );
    }
}
