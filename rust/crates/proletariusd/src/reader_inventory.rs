use bytes::Bytes;
use http_body_util::Full;
use hyper::{Response, StatusCode};
use proletarius_api_contract::{ErrorResponse, UpstreamReaderList};
use proletarius_reader::{
    CardPresence, DiscoverySnapshot, PcscReaderScanner, ReaderObservation, ReaderScanner,
};
use serde::Serialize;
use std::time::Duration;

use crate::ServerState;
use crate::availability::BackendPhase;

const RECONCILE_PATH: &str = "/api/internal/reader/reconcile";
const RECONCILE_TIMEOUT: Duration = Duration::from_secs(2);

#[derive(Serialize)]
struct ReconcileRequest<'a> {
    readers: &'a [String],
}

pub(crate) async fn handle(state: &ServerState) -> Response<Full<Bytes>> {
    let observations = match tokio::task::spawn_blocking(|| PcscReaderScanner.scan()).await {
        Ok(Ok(value)) => value,
        Ok(Err(error)) => return failure(StatusCode::BAD_GATEWAY, &error.to_string()),
        Err(error) => return failure(StatusCode::INTERNAL_SERVER_ERROR, &error.to_string()),
    };
    let discovery = reconcile(state, observations).await;
    crate::json_response(StatusCode::OK, &response_from_discovery(&discovery))
}

/// Makes the native PC/SC inventory authoritative while optionally importing
/// the compatibility process' logical selection. The selected name is always
/// filtered against the freshly scanned inventory, so unplugged hardware can
/// never survive reconciliation as a stale connected reader.
pub(crate) async fn reconcile(
    state: &ServerState,
    observations: Vec<ReaderObservation>,
) -> DiscoverySnapshot {
    let mut observations = observations;
    let readers = observations
        .iter()
        .map(|reader| reader.name.clone())
        .collect::<Vec<_>>();

    // Go still owns the live APDU/VoWiFi session. Import its logical selection
    // as an observation only: this endpoint is polled by the dashboard, and a
    // transient PC/SC inventory miss must not tear down a live Go session.
    // Native reconciliation below filters stale selections from the published
    // snapshot while the explicit Go reader-list path retains hot-unplug
    // teardown ownership.
    let upstream = if state.availability.snapshot().phase == BackendPhase::Ready {
        crate::upstream::post_json_with_timeout::<_, UpstreamReaderList>(
            &state.go_socket_path,
            state.app_token.as_deref(),
            RECONCILE_PATH,
            &ReconcileRequest { readers: &readers },
            RECONCILE_TIMEOUT,
        )
        .await
        .ok()
    } else {
        None
    };
    // The Go compatibility owner also sees the app-owned ESTK BLE bridge.
    // Keep native PC/SC discovery authoritative for USB, but import only a
    // connected BLE identity from the authenticated upstream response.
    if let Some(upstream) = &upstream {
        for name in upstream
            .readers
            .iter()
            .filter(|name| valid_ble_reader(name))
        {
            if observations.iter().any(|reader| reader.name == *name) {
                continue;
            }
            observations.push(ReaderObservation {
                name: name.clone(),
                card: CardPresence::Present,
                in_use: false,
                muted: false,
                atr: Vec::new(),
            });
        }
    }
    let fallback_selected = state
        .reader_discovery
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .snapshot()
        .selected;
    // An available Go response is authoritative even when `selected` is
    // null: that explicitly means the compatibility manager disconnected.
    // Only use the previous native hint while Go is unavailable; otherwise a
    // physically present reader could retain a stale logical selection.
    let selected_hint =
        selected_hint_from_upstream(upstream.as_ref(), fallback_selected.as_deref());
    state
        .reader_discovery
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
        .reconcile(observations, selected_hint)
}

fn valid_ble_reader(name: &str) -> bool {
    let Some(id) = name.strip_prefix("BLE:") else {
        return false;
    };
    id.len() == 36
        && id.bytes().enumerate().all(|(index, byte)| {
            if matches!(index, 8 | 13 | 18 | 23) {
                byte == b'-'
            } else {
                byte.is_ascii_hexdigit()
            }
        })
}

fn selected_hint_from_upstream<'a>(
    upstream: Option<&'a UpstreamReaderList>,
    fallback: Option<&'a str>,
) -> Option<&'a str> {
    match upstream {
        Some(value) => value.selected.as_deref(),
        None => fallback,
    }
}

fn response_from_discovery(discovery: &DiscoverySnapshot) -> UpstreamReaderList {
    UpstreamReaderList {
        readers: discovery
            .readers
            .iter()
            .map(|reader| reader.name.clone())
            .collect(),
        selected: discovery.selected.clone(),
    }
}

fn failure(status: StatusCode, detail: &str) -> Response<Full<Bytes>> {
    let message = format!("枚举读卡器失败：{detail}");
    crate::json_response(
        status,
        &ErrorResponse {
            error: &message,
            code: "reader_list_failed",
            retryable: true,
        },
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use proletarius_reader::{CardPresence, ReaderDiscovery, ReaderObservation};

    #[test]
    fn stale_upstream_selection_is_removed_from_the_read_only_snapshot() {
        let readers = ["Reader A".to_owned()];
        let response = UpstreamReaderList {
            readers: readers.to_vec(),
            selected: Some("Reader B".to_owned()),
        };
        let mut discovery = ReaderDiscovery::default();
        let snapshot = discovery.reconcile(
            vec![ReaderObservation {
                name: "Reader A".into(),
                card: CardPresence::Present,
                in_use: false,
                muted: false,
                atr: vec![],
            }],
            selected_hint_from_upstream(Some(&response), None),
        );
        assert_eq!(snapshot.selected, None);
        assert_eq!(snapshot.readers.len(), 1);
    }

    #[test]
    fn native_response_does_not_require_an_upstream_payload() {
        let discovery = DiscoverySnapshot {
            revision: 7,
            readers: vec![ReaderObservation {
                name: "Reader A".into(),
                card: CardPresence::Present,
                in_use: false,
                muted: false,
                atr: vec![0x3b, 0x00],
            }],
            selected: Some("Reader A".into()),
        };
        let response = response_from_discovery(&discovery);
        assert_eq!(response.readers, vec!["Reader A"]);
        assert_eq!(response.selected.as_deref(), Some("Reader A"));
    }

    #[test]
    fn explicit_upstream_disconnect_clears_stale_selection() {
        let response = UpstreamReaderList {
            readers: vec!["Reader A".into()],
            selected: None,
        };
        assert_eq!(
            selected_hint_from_upstream(Some(&response), Some("Reader A")),
            None
        );
        assert_eq!(
            selected_hint_from_upstream(None, Some("Reader A")),
            Some("Reader A")
        );
    }

    #[test]
    fn ble_identity_requires_uuid_shape() {
        assert!(valid_ble_reader("BLE:12345678-1234-1234-1234-123456789abc"));
        assert!(!valid_ble_reader("BLE:../outside"));
        assert!(!valid_ble_reader("USB reader"));
    }
}
