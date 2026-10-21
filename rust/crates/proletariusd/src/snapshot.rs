use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use proletarius_api_contract::{
    DeviceSnapshot, ModuleSnapshot, ReaderDiscoveryDevice, ReaderSnapshot, SnapshotIssue,
    UpstreamDeviceStatus, UpstreamHealth, UpstreamReaderInfo, UpstreamReaderVoWiFiStatus,
};
use proletarius_reader::{PcscReaderScanner, ReaderScanner};

use crate::ServerState;

const HEALTH_PATH: &str = "/api/health";
const STATUS_PATH: &str = "/api/status";
const READER_INFO_PATH: &str = "/api/reader/info";
const READER_VOWIFI_PATH: &str = "/api/reader/vowifi";
const RUST_READER_DISCOVERY_PATH: &str = "/api/v2/reader-discovery";

pub(crate) async fn capture(state: &ServerState) -> DeviceSnapshot {
    // Assign the revision before any I/O starts. If concurrent captures finish
    // out of order, consumers can discard an older response deterministically.
    let revision = next_revision(&state.snapshot_revision);
    let discovery_task = tokio::task::spawn_blocking(|| PcscReaderScanner.scan());
    let (health_result, status_result, discovery_result) = tokio::join!(
        crate::upstream::get_json::<UpstreamHealth>(
            &state.go_socket_path,
            state.app_token.as_deref(),
            HEALTH_PATH,
        ),
        crate::upstream::get_json::<UpstreamDeviceStatus>(
            &state.go_socket_path,
            state.app_token.as_deref(),
            STATUS_PATH,
        ),
        discovery_task,
    );

    let mut issues = Vec::new();
    let health = result_or_issue(HEALTH_PATH, health_result, &mut issues);
    let status = result_or_issue(STATUS_PATH, status_result, &mut issues);
    let mut reader = ReaderSnapshot::default();

    match discovery_result {
        Ok(Ok(observations)) => {
            let discovery = crate::reader_inventory::reconcile(state, observations).await;
            reader.readers = discovery
                .readers
                .iter()
                .map(|item| item.name.clone())
                .collect();
            reader.selected = discovery.selected;
            reader.inventory_source = "rust-pcsc".into();
            reader.inventory_revision = discovery.revision;
            reader.devices = discovery
                .readers
                .into_iter()
                .map(|item| ReaderDiscoveryDevice {
                    name: item.name,
                    card_state: item.card.as_str(),
                    in_use: item.in_use,
                    muted: item.muted,
                    atr_hex: encode_hex(&item.atr),
                })
                .collect();
        }
        Ok(Err(error)) => issues.push(SnapshotIssue {
            endpoint: RUST_READER_DISCOVERY_PATH,
            code: "rust_pcsc_discovery_failed",
            message: error.to_string(),
            retryable: true,
        }),
        Err(error) => issues.push(SnapshotIssue {
            endpoint: RUST_READER_DISCOVERY_PATH,
            code: "rust_pcsc_discovery_task_failed",
            message: error.to_string(),
            retryable: true,
        }),
    }

    let selected_is_current = reader
        .selected
        .as_ref()
        .map(|selected| reader.readers.contains(selected))
        .unwrap_or(false);
    if reader.selected.is_some() && !selected_is_current {
        issues.push(SnapshotIssue {
            endpoint: RUST_READER_DISCOVERY_PATH,
            code: "reader_selection_stale",
            message: "selected reader is not present in the enumerated reader list".into(),
            retryable: true,
        });
    }

    if selected_is_current {
        let (info_result, vowifi_result) = tokio::join!(
            crate::upstream::get_json::<UpstreamReaderInfo>(
                &state.go_socket_path,
                state.app_token.as_deref(),
                READER_INFO_PATH,
            ),
            crate::upstream::get_json::<UpstreamReaderVoWiFiStatus>(
                &state.go_socket_path,
                state.app_token.as_deref(),
                READER_VOWIFI_PATH,
            ),
        );
        reader.info = result_or_issue(READER_INFO_PATH, info_result, &mut issues);
        reader.vowifi = result_or_issue(READER_VOWIFI_PATH, vowifi_result, &mut issues);
        validate_reader_identity(&mut reader, &mut issues);
    }

    let ok = health.is_some() || status.is_some() || reader.inventory_source == "rust-pcsc";
    DeviceSnapshot {
        schema_version: 2,
        instance_id: state.snapshot_instance_id.clone(),
        revision,
        ok,
        complete: issues.is_empty(),
        captured_at_ms: captured_at_ms(),
        source: "rust-go-shadow",
        module: ModuleSnapshot { health, status },
        reader,
        issues,
    }
}

pub(crate) fn new_instance_id() -> String {
    let started_at = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    format!("{:x}-{started_at:x}", std::process::id())
}

fn next_revision(counter: &AtomicU64) -> u64 {
    counter
        .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |current| {
            Some(current.saturating_add(1))
        })
        .unwrap_or(u64::MAX)
        .saturating_add(1)
}

fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(HEX[(byte >> 4) as usize] as char);
        output.push(HEX[(byte & 0x0f) as usize] as char);
    }
    output
}

fn validate_reader_identity(reader: &mut ReaderSnapshot, issues: &mut Vec<SnapshotIssue>) {
    let reader_identity_valid = matches!(
        (&reader.selected, &reader.info),
        (Some(selected), Some(info))
            if info.status.connected
                && info.status.reader_name.as_deref() == Some(selected.as_str())
    );
    if !reader_identity_valid && reader.selected.is_some() && reader.info.is_some() {
        issues.push(SnapshotIssue {
            endpoint: READER_INFO_PATH,
            code: "reader_identity_mismatch",
            message: "reader info does not match the selected reader".into(),
            retryable: true,
        });
    }

    let Some(vowifi) = reader.vowifi.as_ref() else {
        return;
    };
    if !vowifi_requires_identity_binding(vowifi) {
        return;
    }

    let identity_issue = if !reader_identity_valid {
        Some((
            "vowifi_identity_unverified",
            "reader identity was unavailable while a VoWiFi session status was returned",
        ))
    } else {
        let selected = reader.selected.as_deref().unwrap_or_default();
        let info = reader.info.as_ref().expect("validated above");
        let expected_sim_id = info.usim_info.as_ref().and_then(current_sim_id);
        let generation_matches = matches!(
            (info.status.reader_generation, vowifi.reader_generation),
            (Some(expected), Some(actual)) if expected != 0 && expected == actual
        );
        let reader_matches = vowifi.reader_id.as_deref() == Some(selected);
        let sim_matches =
            expected_sim_id.is_some_and(|expected| vowifi.sim_id.as_deref() == Some(expected));
        if generation_matches && reader_matches && sim_matches {
            None
        } else {
            Some((
                "vowifi_identity_mismatch",
                "VoWiFi session does not belong to the selected reader/SIM generation",
            ))
        }
    };

    if let Some((code, message)) = identity_issue {
        // A status snapshot is read-only.  The Go owner remains responsible
        // for teardown, while Rust refuses to re-publish an unbound ready
        // state as if it belonged to the newly selected profile.
        reader.vowifi = None;
        issues.push(SnapshotIssue {
            endpoint: READER_VOWIFI_PATH,
            code,
            message: message.into(),
            retryable: true,
        });
    }
}

fn current_sim_id(info: &proletarius_api_contract::UpstreamUsimInfo) -> Option<&str> {
    let iccid = info.iccid.trim();
    if !iccid.is_empty() {
        return Some(iccid);
    }
    let imsi = info.imsi.trim();
    (!imsi.is_empty()).then_some(imsi)
}

fn vowifi_requires_identity_binding(vowifi: &UpstreamReaderVoWiFiStatus) -> bool {
    vowifi
        .reader_id
        .as_deref()
        .is_some_and(|value| !value.trim().is_empty())
        || vowifi
            .sim_id
            .as_deref()
            .is_some_and(|value| !value.trim().is_empty())
        || vowifi.reader_generation.is_some()
        || vowifi.tunnel_state != "disconnected"
        || vowifi.ims_state != "idle"
        || vowifi.sms_ready.unwrap_or(false)
        || vowifi.call_ready.unwrap_or(false)
}

fn result_or_issue<T>(
    endpoint: &'static str,
    result: Result<T, proletarius_domain::BackendFailure>,
    issues: &mut Vec<SnapshotIssue>,
) -> Option<T> {
    match result {
        Ok(value) => Some(value),
        Err(error) => {
            issues.push(SnapshotIssue {
                endpoint,
                code: "upstream_snapshot_failed",
                message: error.to_string(),
                retryable: true,
            });
            None
        }
    }
}

fn captured_at_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::AtomicU64;

    use super::{encode_hex, next_revision, result_or_issue, validate_reader_identity};
    use proletarius_api_contract::{ReaderSnapshot, UpstreamReaderInfo, UpstreamReaderStatus};
    use proletarius_domain::BackendFailure;
    use proletarius_reader::{CardPresence, ReaderObservation};

    #[test]
    fn snapshot_revision_is_monotonic_and_saturating() {
        let counter = AtomicU64::new(0);
        assert_eq!(next_revision(&counter), 1);
        assert_eq!(next_revision(&counter), 2);

        let saturated = AtomicU64::new(u64::MAX);
        assert_eq!(next_revision(&saturated), u64::MAX);
        assert_eq!(next_revision(&saturated), u64::MAX);
    }

    #[test]
    fn partial_failure_is_preserved_as_an_issue() {
        let mut issues = Vec::new();
        let value: Option<()> = result_or_issue(
            "/api/status",
            Err(BackendFailure::GoBackendUnavailable("timeout".into())),
            &mut issues,
        );
        assert!(value.is_none());
        assert_eq!(issues.len(), 1);
        assert_eq!(issues[0].endpoint, "/api/status");
        assert!(issues[0].retryable);
    }

    #[test]
    fn mismatched_reader_identity_is_reported() {
        let mut reader = ReaderSnapshot {
            readers: vec!["Reader A".into()],
            selected: Some("Reader A".into()),
            info: Some(UpstreamReaderInfo {
                status: UpstreamReaderStatus {
                    connected: true,
                    reader_name: Some("Reader B".into()),
                    reader_generation: Some(1),
                    has_card: true,
                    card_type: Some("euicc".into()),
                    has_usim_info: true,
                    has_esim: true,
                },
                usim_info: None,
                epdg_fqdn: None,
            }),
            vowifi: None,
            inventory_source: "rust-pcsc".into(),
            inventory_revision: 1,
            devices: Vec::new(),
        };
        let mut issues = Vec::new();
        validate_reader_identity(&mut reader, &mut issues);
        assert_eq!(issues.len(), 1);
        assert_eq!(issues[0].code, "reader_identity_mismatch");
    }

    #[test]
    fn live_vowifi_is_quarantined_when_reader_sim_or_generation_mismatch() {
        for mismatch in ["reader", "sim", "generation", "missing_generation"] {
            let mut reader = bound_reader_snapshot();
            let status = reader.vowifi.as_mut().expect("live status");
            match mismatch {
                "reader" => status.reader_id = Some("Reader B".into()),
                "sim" => status.sim_id = Some("8985200014631959073".into()),
                "generation" => status.reader_generation = Some(6),
                "missing_generation" => status.reader_generation = None,
                _ => unreachable!(),
            }

            let mut issues = Vec::new();
            validate_reader_identity(&mut reader, &mut issues);

            assert!(reader.vowifi.is_none(), "{mismatch} status was retained");
            assert!(issues.iter().any(|issue| {
                issue.endpoint == "/api/reader/vowifi"
                    && issue.code == "vowifi_identity_mismatch"
                    && issue.retryable
            }));
        }
    }

    #[test]
    fn live_vowifi_is_not_reused_when_go_reader_identity_is_unavailable() {
        let mut reader = bound_reader_snapshot();
        reader.info = None;
        let mut issues = Vec::new();

        validate_reader_identity(&mut reader, &mut issues);

        assert!(reader.vowifi.is_none());
        assert!(issues.iter().any(|issue| {
            issue.endpoint == "/api/reader/vowifi"
                && issue.code == "vowifi_identity_unverified"
                && issue.retryable
        }));
    }

    fn bound_reader_snapshot() -> ReaderSnapshot {
        let info: UpstreamReaderInfo = serde_json::from_value(serde_json::json!({
            "status": {
                "connected": true,
                "reader_name": "Reader A",
                "reader_generation": 7,
                "has_card": true,
                "card_type": "euicc",
                "has_usim_info": true,
                "has_esim": true
            },
            "usim_info": {
                "iccid": "8985200014631959072",
                "imsi": "234330123456789",
                "mcc": "234",
                "mnc": "33"
            }
        }))
        .expect("reader info fixture");
        let vowifi = serde_json::from_value(serde_json::json!({
            "reader_id": "Reader A",
            "sim_id": "8985200014631959072",
            "reader_generation": 7,
            "available": true,
            "tunnel_state": "connected",
            "ims_state": "registered",
            "sms_ready": true,
            "call_ready": true
        }))
        .expect("VoWiFi fixture");
        ReaderSnapshot {
            readers: vec!["Reader A".into()],
            selected: Some("Reader A".into()),
            info: Some(info),
            vowifi: Some(vowifi),
            inventory_source: "rust-pcsc".into(),
            inventory_revision: 1,
            devices: Vec::new(),
        }
    }

    #[test]
    fn reader_atr_is_encoded_without_go_inventory() {
        let observation = ReaderObservation {
            name: "A".into(),
            card: CardPresence::Present,
            in_use: true,
            muted: false,
            atr: vec![0x3b, 0x00],
        };
        assert_eq!(encode_hex(&observation.atr), "3b00");
    }
}
