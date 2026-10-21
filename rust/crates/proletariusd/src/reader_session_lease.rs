//! Short-lived cross-process lease that keeps the Go reader manager from
//! interleaving card operations with Rust's native session preparation.

use std::{path::Path, sync::Arc, time::Duration};

use proletarius_domain::BackendFailure;
use proletarius_vowifi::SimAkaResult;
use serde::{Deserialize, Serialize};
use tokio::{sync::watch, task::JoinHandle, time::sleep};
use zeroize::{Zeroize, Zeroizing};

const ACQUIRE_PATH: &str = "/api/internal/reader/session-lease/acquire";
const AKA_PATH: &str = "/api/internal/reader/session-lease/aka";
const RENEW_PATH: &str = "/api/internal/reader/session-lease/renew";
const RELEASE_PATH: &str = "/api/internal/reader/session-lease/release";
const LEASE_TIMEOUT: Duration = Duration::from_secs(16);
const AKA_TIMEOUT: Duration = Duration::from_secs(40);
const LEASE_RENEW_INTERVAL: Duration = Duration::from_secs(30);
const LEASE_RENEW_RETRY_INTERVAL: Duration = Duration::from_secs(5);
const LEASE_RENEW_RETRIES: usize = 1;

#[derive(Serialize)]
struct AcquireRequest<'a> {
    reader: &'a str,
}

#[derive(Serialize)]
struct ReleaseRequest<'a> {
    lease_id: &'a str,
}

#[derive(Serialize)]
struct RenewRequest<'a> {
    lease_id: &'a str,
}

#[derive(Serialize)]
struct AkaRequest<'a> {
    lease_id: &'a str,
    reader: &'a str,
    reader_generation: u64,
    application: &'static str,
    rand: &'a [u8],
    autn: &'a [u8],
}

#[derive(Deserialize)]
struct AcquireResponse {
    lease_id: String,
    reader: String,
    reader_generation: u64,
    identity: LeaseIdentityWire,
}

#[derive(Deserialize)]
struct LeaseIdentityWire {
    iccid: String,
    imsi: String,
    mcc: String,
    mnc: String,
    #[serde(default)]
    gid1: String,
    #[serde(default)]
    gid2: String,
    #[serde(default)]
    spn: String,
    #[serde(default)]
    private_identity: Zeroizing<String>,
    #[serde(default)]
    public_identity: Zeroizing<String>,
}

pub(crate) struct LeaseIdentity {
    pub(crate) iccid: Zeroizing<String>,
    pub(crate) imsi: Zeroizing<String>,
    pub(crate) mcc: String,
    pub(crate) mnc: String,
    pub(crate) gid1: String,
    pub(crate) gid2: String,
    pub(crate) spn: String,
    // Prepared as sensitive, generation-bound inputs for the native IMS
    // session owner. Keep zeroizing custody across this boundary; the live
    // session route is not switched yet, so these are intentionally unused.
    #[allow(dead_code)]
    pub(crate) private_identity: Zeroizing<String>,
    #[allow(dead_code)]
    pub(crate) public_identity: Zeroizing<String>,
}

/// Token and app credential are kept out of formatting and diagnostics. The
/// Go manager enforces a two-minute expiry if the Rust process exits abruptly.
pub(crate) struct NativeReaderSessionLease {
    lease_id: Zeroizing<String>,
    app_token: Option<Zeroizing<String>>,
    socket_path: std::path::PathBuf,
    pub(crate) reader: String,
    pub(crate) reader_generation: u64,
    pub(crate) identity: LeaseIdentity,
}

impl NativeReaderSessionLease {
    pub(crate) async fn acquire(
        socket_path: &Path,
        app_token: Option<&str>,
        reader: &str,
    ) -> Result<Self, BackendFailure> {
        let response: AcquireResponse = proletariusd_upstream_post(
            socket_path,
            app_token,
            ACQUIRE_PATH,
            &AcquireRequest { reader },
            LEASE_TIMEOUT,
        )
        .await?;
        let valid_lease_id = response.lease_id.len() == 64
            && response
                .lease_id
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit());
        let valid_identity = response.reader == reader
            && response.reader_generation != 0
            && valid_digits(&response.identity.iccid, 18, 22)
            && valid_digits(&response.identity.imsi, 5, 15)
            && valid_digits(&response.identity.mcc, 3, 3)
            && valid_digits(&response.identity.mnc, 2, 3);
        let valid_optional_ims_identity = [
            response.identity.private_identity.as_str(),
            response.identity.public_identity.as_str(),
        ]
        .into_iter()
        .all(|value| value.len() <= 512 && !value.bytes().any(|byte| byte.is_ascii_control()));
        if !valid_lease_id {
            return Err(BackendFailure::GoBackendUnavailable(
                "reader session lease returned an invalid token".into(),
            ));
        }
        if !valid_identity || !valid_optional_ims_identity {
            // A version mismatch may return a token with an invalid identity
            // after Go has already locked the card. Release that token now
            // instead of relying on the watchdog.
            let lease_id = Zeroizing::new(response.lease_id);
            let _ = proletariusd_upstream_post::<_, ReleaseResponse>(
                socket_path,
                app_token,
                RELEASE_PATH,
                &ReleaseRequest {
                    lease_id: lease_id.as_str(),
                },
                LEASE_TIMEOUT,
            )
            .await;
            return Err(BackendFailure::GoBackendUnavailable(
                "reader session lease returned an invalid identity binding".into(),
            ));
        }
        Ok(Self {
            lease_id: Zeroizing::new(response.lease_id),
            app_token: app_token.map(|value| Zeroizing::new(value.to_owned())),
            socket_path: socket_path.to_owned(),
            reader: response.reader,
            reader_generation: response.reader_generation,
            identity: LeaseIdentity {
                iccid: Zeroizing::new(response.identity.iccid),
                imsi: Zeroizing::new(response.identity.imsi),
                mcc: response.identity.mcc,
                mnc: response.identity.mnc,
                gid1: response.identity.gid1,
                gid2: response.identity.gid2,
                spn: response.identity.spn,
                private_identity: response.identity.private_identity,
                public_identity: response.identity.public_identity,
            },
        })
    }

    pub(crate) async fn release(&self) -> Result<(), BackendFailure> {
        let response: ReleaseResponse = proletariusd_upstream_post(
            &self.socket_path,
            self.app_token.as_deref().map(|token| token.as_str()),
            RELEASE_PATH,
            &ReleaseRequest {
                lease_id: self.lease_id.as_str(),
            },
            LEASE_TIMEOUT,
        )
        .await?;
        if !response.released {
            return Err(BackendFailure::GoBackendUnavailable(
                "reader session lease release was not confirmed".into(),
            ));
        }
        Ok(())
    }

    /// Extend the cross-process reader lock before its Go watchdog expires.
    /// The future session actor must stop and unwind IMS/IKE if renewal fails;
    /// silently continuing would leave Rust operating without card ownership.
    #[allow(
        dead_code,
        reason = "consumed by the gated native VoWiFi session actor"
    )]
    pub(crate) async fn renew(&self) -> Result<(), BackendFailure> {
        let response: RenewResponse = proletariusd_upstream_post(
            &self.socket_path,
            self.app_token.as_deref().map(|token| token.as_str()),
            RENEW_PATH,
            &RenewRequest {
                lease_id: self.lease_id.as_str(),
            },
            LEASE_TIMEOUT,
        )
        .await?;
        if !response.renewed {
            return Err(BackendFailure::GoBackendUnavailable(
                "reader session lease renewal was not confirmed".into(),
            ));
        }
        Ok(())
    }

    /// Run one AKA transaction through the generation-bound reader transport.
    /// Go dispatches USB through PC/SC and ESTK readers through its existing
    /// BLE bridge; the native lease excludes unrelated card operations.
    pub(crate) async fn authenticate(
        &self,
        application: ReaderAkaApplication,
        rand: &[u8; 16],
        autn: &[u8; 16],
    ) -> Result<SimAkaResult, BackendFailure> {
        let application = match application {
            ReaderAkaApplication::Usim => "usim",
            ReaderAkaApplication::Isim => "isim",
        };
        let response: AkaResponse = proletariusd_upstream_post(
            &self.socket_path,
            self.app_token.as_deref().map(|token| token.as_str()),
            AKA_PATH,
            &AkaRequest {
                lease_id: self.lease_id.as_str(),
                reader: &self.reader,
                reader_generation: self.reader_generation,
                application,
                rand,
                autn,
            },
            AKA_TIMEOUT,
        )
        .await?;
        response.into_result()
    }

    #[allow(
        dead_code,
        reason = "used when the gated native IMS runtime takes reader ownership"
    )]
    pub(crate) async fn authenticate_ims(
        &self,
        rand: &[u8; 16],
        autn: &[u8; 16],
    ) -> Result<SimAkaResult, BackendFailure> {
        self.authenticate(ReaderAkaApplication::Isim, rand, autn)
            .await
    }

    #[allow(
        dead_code,
        reason = "used by the gated native IKE actor after it takes session ownership"
    )]
    pub(crate) async fn authenticate_ike(
        &self,
        rand: &[u8; 16],
        autn: &[u8; 16],
    ) -> Result<SimAkaResult, BackendFailure> {
        self.authenticate(ReaderAkaApplication::Usim, rand, autn)
            .await
    }
}

/// Keeps the generation-bound Go reader lease alive for the lifetime of a
/// native tunnel. A transient IPC failure gets bounded retries; if all fail,
/// `lost` becomes terminal so the session owner can tear down IKE/IMS instead
/// of presenting a stale online state. The caller must retain this guard for
/// the entire session, not merely through initial AKA.
pub(crate) struct NativeReaderSessionHeartbeat {
    stop: watch::Sender<bool>,
    lost: watch::Receiver<bool>,
    task: Option<JoinHandle<()>>,
}

impl NativeReaderSessionHeartbeat {
    pub(crate) fn start(lease: Arc<NativeReaderSessionLease>) -> Self {
        Self::start_with_timing(
            lease,
            LEASE_RENEW_INTERVAL,
            LEASE_RENEW_RETRY_INTERVAL,
            LEASE_RENEW_RETRIES,
        )
    }

    fn start_with_timing(
        lease: Arc<NativeReaderSessionLease>,
        interval: Duration,
        retry_interval: Duration,
        retries: usize,
    ) -> Self {
        let (stop, mut stop_rx) = watch::channel(false);
        let (lost_tx, lost) = watch::channel(false);
        let task = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = sleep(interval) => {}
                    changed = stop_rx.changed() => {
                        if changed.is_err() || *stop_rx.borrow() {
                            return;
                        }
                    }
                }
                if *stop_rx.borrow() {
                    return;
                }

                let mut renewed = false;
                for attempt in 0..=retries {
                    let result = tokio::select! {
                        changed = stop_rx.changed() => {
                            if changed.is_err() || *stop_rx.borrow() {
                                return;
                            }
                            continue;
                        }
                        result = lease.renew() => result,
                    };
                    if result.is_ok() {
                        renewed = true;
                        break;
                    }
                    if attempt < retries {
                        tokio::select! {
                            _ = sleep(retry_interval) => {}
                            changed = stop_rx.changed() => {
                                if changed.is_err() || *stop_rx.borrow() {
                                    return;
                                }
                            }
                        }
                        if *stop_rx.borrow() {
                            return;
                        }
                    }
                }
                if !renewed {
                    let _ = lost_tx.send(true);
                    return;
                }
            }
        });
        Self {
            stop,
            lost,
            task: Some(task),
        }
    }

    #[allow(dead_code, reason = "observed by the keyed native session actor")]
    pub(crate) fn lost_receiver(&self) -> watch::Receiver<bool> {
        self.lost.clone()
    }

    pub(crate) fn is_lost(&self) -> bool {
        *self.lost.borrow()
    }

    pub(crate) async fn wait_lost(&mut self) {
        while !self.is_lost() {
            if self.lost.changed().await.is_err() {
                return;
            }
        }
    }

    pub(crate) async fn stop(mut self) {
        let _ = self.stop.send(true);
        if let Some(task) = self.task.take() {
            let _ = task.await;
        }
    }
}

impl Drop for NativeReaderSessionHeartbeat {
    fn drop(&mut self) {
        let _ = self.stop.send(true);
        if let Some(task) = self.task.take() {
            task.abort();
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReaderAkaApplication {
    Usim,
    Isim,
}

#[derive(Deserialize)]
struct AkaResponse {
    outcome: String,
    #[serde(default)]
    res: Vec<u8>,
    #[serde(default)]
    ck: Vec<u8>,
    #[serde(default)]
    ik: Vec<u8>,
    #[serde(default)]
    auts: Vec<u8>,
}

impl AkaResponse {
    fn into_result(mut self) -> Result<SimAkaResult, BackendFailure> {
        match self.outcome.as_str() {
            "success"
                if (4..=16).contains(&self.res.len())
                    && self.ck.len() == 16
                    && self.ik.len() == 16
                    && self.auts.is_empty() =>
            {
                let res = std::mem::take(&mut self.res);
                let ck = self
                    .ck
                    .as_slice()
                    .try_into()
                    .map_err(|_| invalid_aka_response())?;
                let ik = self
                    .ik
                    .as_slice()
                    .try_into()
                    .map_err(|_| invalid_aka_response())?;
                Ok(SimAkaResult::Success { res, ck, ik })
            }
            "synchronization_failure"
                if self.res.is_empty()
                    && self.ck.is_empty()
                    && self.ik.is_empty()
                    && self.auts.len() == 14 =>
            {
                let auts = self
                    .auts
                    .as_slice()
                    .try_into()
                    .map_err(|_| invalid_aka_response())?;
                Ok(SimAkaResult::SynchronizationFailure { auts })
            }
            "authentication_reject"
                if self.res.is_empty()
                    && self.ck.is_empty()
                    && self.ik.is_empty()
                    && self.auts.is_empty() =>
            {
                Ok(SimAkaResult::AuthenticationRejected)
            }
            _ => Err(invalid_aka_response()),
        }
    }
}

impl Drop for AkaResponse {
    fn drop(&mut self) {
        self.res.zeroize();
        self.ck.zeroize();
        self.ik.zeroize();
        self.auts.zeroize();
    }
}

fn invalid_aka_response() -> BackendFailure {
    BackendFailure::GoBackendUnavailable(
        "reader session lease returned an invalid AKA result".into(),
    )
}

#[derive(Deserialize)]
struct ReleaseResponse {
    released: bool,
}

#[derive(Deserialize)]
struct RenewResponse {
    renewed: bool,
}

async fn proletariusd_upstream_post<I, O>(
    socket_path: &Path,
    app_token: Option<&str>,
    path: &'static str,
    input: &I,
    timeout: Duration,
) -> Result<O, BackendFailure>
where
    I: Serialize,
    O: serde::de::DeserializeOwned,
{
    crate::upstream::post_json_with_timeout(socket_path, app_token, path, input, timeout).await
}

fn valid_digits(value: &str, min: usize, max: usize) -> bool {
    (min..=max).contains(&value.len()) && value.bytes().all(|byte| byte.is_ascii_digit())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::UnixListener;

    static NEXT_SOCKET: AtomicU64 = AtomicU64::new(1);

    #[tokio::test]
    async fn acquire_and_release_use_private_generation_bound_go_contract() {
        let socket_path = std::env::temp_dir().join(format!(
            "proletarius-reader-lease-{}-{}.sock",
            std::process::id(),
            NEXT_SOCKET.fetch_add(1, Ordering::Relaxed)
        ));
        let listener = UnixListener::bind(&socket_path).unwrap();
        let server = tokio::spawn(async move {
            let mut requests = Vec::new();
            let ck = vec![2_u8; 16];
            let ik = vec![3_u8; 16];
            let aka_response = serde_json::to_vec(&serde_json::json!({
                "outcome": "success",
                "res": [1, 2, 3, 4, 5, 6, 7, 8],
                "ck": ck,
                "ik": ik,
            }))
            .unwrap();
            let sync_response = serde_json::to_vec(&serde_json::json!({
                "outcome": "synchronization_failure",
                "auts": vec![0x5a_u8; 14],
            }))
            .unwrap();
            let bodies = vec![
                br#"{"lease_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reader":"BLE:ESTKme-RED:test","reader_generation":4,"identity":{"iccid":"8985200014631959072","imsi":"234100896392986","mcc":"234","mnc":"33","gid1":"ABCD","private_identity":"subscriber@ims.example.test","public_identity":"sip:+447700900123@ims.example.test"}}"#.to_vec(),
                aka_response,
                sync_response,
                br#"{"renewed":true}"#.to_vec(),
                br#"{"released":true}"#.to_vec(),
            ];
            for body in &bodies {
                let (mut stream, _) = listener.accept().await.unwrap();
                let request = read_request(&mut stream).await;
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(response.as_bytes()).await.unwrap();
                stream.write_all(body).await.unwrap();
                requests.push(request);
            }
            requests
        });

        let lease = NativeReaderSessionLease::acquire(
            &socket_path,
            Some("fixture-app-token"),
            "BLE:ESTKme-RED:test",
        )
        .await
        .unwrap();
        assert_eq!(lease.reader_generation, 4);
        assert_eq!(lease.identity.iccid.as_str(), "8985200014631959072");
        assert_eq!(
            lease.identity.private_identity.as_str(),
            "subscriber@ims.example.test"
        );
        assert_eq!(
            lease.identity.public_identity.as_str(),
            "sip:+447700900123@ims.example.test"
        );
        let rand = [0xa1; 16];
        let autn = [0xb2; 16];
        let aka_result = lease.authenticate_ike(&rand, &autn).await.unwrap();
        match &aka_result {
            SimAkaResult::Success { res, ck, ik } => {
                assert_eq!(res.as_slice(), &[1, 2, 3, 4, 5, 6, 7, 8]);
                assert_eq!(ck.as_slice(), &[2; 16]);
                assert_eq!(ik.as_slice(), &[3; 16]);
            }
            _ => panic!("expected a successful leased AKA result"),
        }
        let ims_result = lease.authenticate_ims(&rand, &autn).await.unwrap();
        assert!(matches!(
            &ims_result,
            SimAkaResult::SynchronizationFailure { auts } if auts.as_slice() == &[0x5a; 14]
        ));
        lease.renew().await.unwrap();
        lease.release().await.unwrap();

        let requests = tokio::time::timeout(Duration::from_secs(2), server)
            .await
            .unwrap()
            .unwrap();
        let acquire = String::from_utf8_lossy(&requests[0]).to_ascii_lowercase();
        let aka = String::from_utf8_lossy(&requests[1]).to_ascii_lowercase();
        let ims_aka = String::from_utf8_lossy(&requests[2]).to_ascii_lowercase();
        let renew = String::from_utf8_lossy(&requests[3]).to_ascii_lowercase();
        let release = String::from_utf8_lossy(&requests[4]).to_ascii_lowercase();
        assert!(acquire.starts_with("post /api/internal/reader/session-lease/acquire "));
        assert!(acquire.contains("x-proletarius-app-token: fixture-app-token\r\n"));
        assert!(
            String::from_utf8_lossy(&requests[0]).contains("\"reader\":\"BLE:ESTKme-RED:test\"")
        );
        assert!(aka.starts_with("post /api/internal/reader/session-lease/aka "));
        assert!(aka.contains("x-proletarius-app-token: fixture-app-token\r\n"));
        assert!(aka.contains("\"reader_generation\":4"));
        assert!(aka.contains("\"application\":\"usim\""));
        assert!(aka.contains("\"rand\":[161,161,161"));
        assert!(aka.contains("\"autn\":[178,178,178"));
        assert!(ims_aka.contains("\"application\":\"isim\""));
        assert!(renew.starts_with("post /api/internal/reader/session-lease/renew "));
        assert!(renew.contains("x-proletarius-app-token: fixture-app-token\r\n"));
        assert!(String::from_utf8_lossy(&requests[3]).contains(&"a".repeat(64)));
        assert!(release.starts_with("post /api/internal/reader/session-lease/release "));
        assert!(release.contains("x-proletarius-app-token: fixture-app-token\r\n"));
        assert!(String::from_utf8_lossy(&requests[4]).contains(&"a".repeat(64)));
        let _ = std::fs::remove_file(socket_path);
    }

    #[tokio::test]
    async fn heartbeat_retries_transient_failures_then_marks_reader_lease_lost() {
        let socket_path = std::env::temp_dir().join(format!(
            "proletarius-reader-heartbeat-{}-{}.sock",
            std::process::id(),
            NEXT_SOCKET.fetch_add(1, Ordering::Relaxed)
        ));
        let listener = UnixListener::bind(&socket_path).unwrap();
        let server = tokio::spawn(async move {
            let responses = [
                br#"{"lease_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","reader":"Reader A","reader_generation":8,"identity":{"iccid":"8985200014631959072","imsi":"234100896392986","mcc":"234","mnc":"33"}}"#.as_slice(),
                br#"{"renewed":false}"#.as_slice(),
                br#"{"renewed":false}"#.as_slice(),
                br#"{"released":true}"#.as_slice(),
            ];
            let mut paths = Vec::new();
            for body in responses {
                let (mut stream, _) = listener.accept().await.unwrap();
                let request = read_request(&mut stream).await;
                let first_line = String::from_utf8_lossy(&request)
                    .lines()
                    .next()
                    .unwrap_or_default()
                    .to_owned();
                paths.push(first_line);
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(response.as_bytes()).await.unwrap();
                stream.write_all(body).await.unwrap();
            }
            paths
        });

        let lease = Arc::new(
            NativeReaderSessionLease::acquire(&socket_path, None, "Reader A")
                .await
                .unwrap(),
        );
        let mut heartbeat = NativeReaderSessionHeartbeat::start_with_timing(
            lease.clone(),
            Duration::from_millis(1),
            Duration::from_millis(1),
            1,
        );
        tokio::time::timeout(Duration::from_secs(1), heartbeat.wait_lost())
            .await
            .expect("the failed bounded renewals must become terminal");
        assert!(heartbeat.is_lost());
        heartbeat.stop().await;
        lease.release().await.unwrap();

        let paths = tokio::time::timeout(Duration::from_secs(1), server)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(paths.len(), 4);
        assert!(paths[0].starts_with("POST /api/internal/reader/session-lease/acquire "));
        assert!(paths[1].starts_with("POST /api/internal/reader/session-lease/renew "));
        assert!(paths[2].starts_with("POST /api/internal/reader/session-lease/renew "));
        assert!(paths[3].starts_with("POST /api/internal/reader/session-lease/release "));
        let _ = std::fs::remove_file(socket_path);
    }

    #[tokio::test]
    async fn heartbeat_success_renews_and_can_stop_without_marking_the_lease_lost() {
        let socket_path = std::env::temp_dir().join(format!(
            "proletarius-reader-heartbeat-ok-{}-{}.sock",
            std::process::id(),
            NEXT_SOCKET.fetch_add(1, Ordering::Relaxed)
        ));
        let listener = UnixListener::bind(&socket_path).unwrap();
        let (renewed_tx, renewed_rx) = tokio::sync::oneshot::channel();
        let server = tokio::spawn(async move {
            let responses = [
                br#"{"lease_id":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","reader":"Reader B","reader_generation":9,"identity":{"iccid":"8985200014631959072","imsi":"234100896392986","mcc":"234","mnc":"33"}}"#.as_slice(),
                br#"{"renewed":true}"#.as_slice(),
                br#"{"released":true}"#.as_slice(),
            ];
            let mut paths = Vec::new();
            let mut renewed_tx = Some(renewed_tx);
            for (index, body) in responses.into_iter().enumerate() {
                let (mut stream, _) = listener.accept().await.unwrap();
                let request = read_request(&mut stream).await;
                let first_line = String::from_utf8_lossy(&request)
                    .lines()
                    .next()
                    .unwrap_or_default()
                    .to_owned();
                paths.push(first_line);
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(response.as_bytes()).await.unwrap();
                stream.write_all(body).await.unwrap();
                if index == 1 {
                    if let Some(renewed_tx) = renewed_tx.take() {
                        let _ = renewed_tx.send(());
                    }
                }
            }
            paths
        });

        let lease = Arc::new(
            NativeReaderSessionLease::acquire(&socket_path, None, "Reader B")
                .await
                .unwrap(),
        );
        let heartbeat = NativeReaderSessionHeartbeat::start_with_timing(
            lease.clone(),
            Duration::from_millis(1),
            Duration::from_millis(1),
            1,
        );
        tokio::time::timeout(Duration::from_secs(1), renewed_rx)
            .await
            .expect("heartbeat should renew the active lease")
            .unwrap();
        assert!(!heartbeat.is_lost());
        heartbeat.stop().await;
        lease.release().await.unwrap();

        let paths = tokio::time::timeout(Duration::from_secs(1), server)
            .await
            .unwrap()
            .unwrap();
        assert!(paths[0].starts_with("POST /api/internal/reader/session-lease/acquire "));
        assert!(paths[1].starts_with("POST /api/internal/reader/session-lease/renew "));
        assert!(paths[2].starts_with("POST /api/internal/reader/session-lease/release "));
        let _ = std::fs::remove_file(socket_path);
    }

    async fn read_request(stream: &mut tokio::net::UnixStream) -> Vec<u8> {
        let mut request = Vec::new();
        let mut chunk = [0_u8; 1024];
        loop {
            let count = stream.read(&mut chunk).await.unwrap();
            assert_ne!(count, 0, "client closed before sending the full request");
            request.extend_from_slice(&chunk[..count]);
            let Some(header_end) = request.windows(4).position(|window| window == b"\r\n\r\n")
            else {
                continue;
            };
            let headers = String::from_utf8_lossy(&request[..header_end]).to_ascii_lowercase();
            let content_length = headers
                .lines()
                .find_map(|line| line.strip_prefix("content-length:"))
                .and_then(|value| value.trim().parse::<usize>().ok())
                .unwrap_or(0);
            if request.len() >= header_end + 4 + content_length {
                return request;
            }
        }
    }

    #[test]
    fn malformed_card_identity_is_rejected() {
        assert!(!valid_digits("234x", 3, 3));
        assert!(!valid_digits("", 5, 15));
        assert!(valid_digits("234100896392986", 5, 15));
    }

    #[test]
    fn aka_response_requires_exact_secret_lengths_and_preserves_sync_failure() {
        let sync = AkaResponse {
            outcome: "synchronization_failure".into(),
            res: Vec::new(),
            ck: Vec::new(),
            ik: Vec::new(),
            auts: vec![0x5a; 14],
        }
        .into_result()
        .unwrap();
        assert!(matches!(sync, SimAkaResult::SynchronizationFailure { .. }));

        let malformed = AkaResponse {
            outcome: "success".into(),
            res: vec![1; 8],
            ck: vec![2; 15],
            ik: vec![3; 16],
            auts: Vec::new(),
        }
        .into_result();
        assert!(malformed.is_err());

        let rejected = AkaResponse {
            outcome: "authentication_reject".into(),
            res: Vec::new(),
            ck: Vec::new(),
            ik: Vec::new(),
            auts: Vec::new(),
        }
        .into_result()
        .unwrap();
        assert!(matches!(rejected, SimAkaResult::AuthenticationRejected));
    }
}
