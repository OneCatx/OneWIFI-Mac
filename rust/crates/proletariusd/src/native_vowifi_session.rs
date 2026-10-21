//! Native reader VoWiFi session preparation.
//!
//! This is deliberately a preparation boundary, not a second live tunnel
//! owner: it atomically snapshots card identity, carrier selection, override
//! policy and proxy choice before any IKE packet can be emitted. The later
//! native owner consumes this immutable plan after IMS runtime wiring passes
//! its own gates.

use std::{
    env, fs,
    future::Future,
    net::{IpAddr, SocketAddr},
    path::{Path, PathBuf},
    sync::OnceLock,
    time::Duration,
};

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use proletarius_ims::aka_exchange::SimAnswer;
use proletarius_vowifi::{
    DatagramTransportError, EpdgDatagramTransport, NativeTunnel, NativeTunnelSettings,
    SimAkaResult, TunnelConfiguration, TunnelStartupError,
    carrier::{NativeTunnelOverrides, Registry, SimIdentity},
    carrier_overrides::OverrideSet,
    ims_profile::{
        NativeImsDeviceIdentity, NativeImsProfileError, NativeImsRegisterProfile, NativeImsRuntime,
        NativeImsSubscriberIdentity, NativeSecurityEndpoints,
    },
    registration_coordinator::{
        NativeProfileRegistrationFailure, RegisteredImsSession, RegistrationServices,
        register_native_profile_with_identity,
    },
    start_configured_native_tunnel,
};
use serde::Deserialize;
use serde_json::json;
use zeroize::{Zeroize, Zeroizing};

use crate::transport_selection::{SelectedTransport, SessionSelectionError};

const REQUEST_LIMIT: usize = 4_096;
const OVERRIDES_ENVIRONMENT: &str = "PROLETARIUS_VOWIFI_CONFIG";
static CARRIER_REGISTRY: OnceLock<Result<Registry, String>> = OnceLock::new();

/// Explicit production-owner gates for the dormant native VoWiFi path.
///
/// Keep this private and fail-closed until the real actor owns all five
/// resources together. In particular, this is not an environment-driven
/// feature flag: changing a single deployment setting must never let Rust
/// open a reader while Go still owns its live session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct NativeOwnerReadiness {
    cross_process_reader_lease: bool,
    ble_aka_transport_bridge: bool,
    keyed_lifecycle_actor: bool,
    protected_ims_runtime: bool,
    live_sms_call_runtime: bool,
}

impl NativeOwnerReadiness {
    const fn current() -> Self {
        Self {
            cross_process_reader_lease: true,
            ble_aka_transport_bridge: false,
            keyed_lifecycle_actor: false,
            protected_ims_runtime: false,
            live_sms_call_runtime: false,
        }
    }

    const fn ready(self) -> bool {
        self.cross_process_reader_lease
            && self.ble_aka_transport_bridge
            && self.keyed_lifecycle_actor
            && self.protected_ims_runtime
            && self.live_sms_call_runtime
    }

    fn missing(self) -> Vec<&'static str> {
        let mut missing = Vec::with_capacity(5);
        if !self.cross_process_reader_lease {
            missing.push("cross_process_reader_lease");
        }
        if !self.ble_aka_transport_bridge {
            missing.push("ble_aka_transport_bridge");
        }
        if !self.keyed_lifecycle_actor {
            missing.push("keyed_lifecycle_actor");
        }
        if !self.protected_ims_runtime {
            missing.push("protected_ims_runtime");
        }
        if !self.live_sms_call_runtime {
            missing.push("live_sms_call_runtime");
        }
        missing
    }
}

#[derive(Deserialize)]
struct PlanRequest {
    reader: String,
    #[serde(default)]
    proxy: String,
    #[serde(default)]
    epdg: String,
    #[serde(default)]
    epdg_port: Option<u16>,
}

/// One immutable reader/SIM session context. This deliberately has no
/// `Clone`, `Debug` or serialization implementation: it contains the bound
/// AKA identity and is the only future owner allowed to launch a tunnel.
struct NativeSessionContext {
    reader: String,
    reader_generation: u64,
    /// Opaque reader/SIM key derived from the same physical-card snapshot as
    /// the AKA binding. It never comes from the HTTP request and is retained
    /// for the future per-session supervisor.
    #[allow(dead_code)] // Consumed when the gated native actor takes ownership.
    session_key: crate::native_vowifi_supervisor::ReaderSessionKey,
    mcc: String,
    mnc: String,
    carrier_id: String,
    config: proletarius_vowifi::carrier::NativeCarrierTunnelConfig,
    /// Resolved from the same SIM/template/override snapshot as the tunnel.
    /// It is not live yet; the future registered-session owner consumes it.
    ims: NativeImsRegisterProfile,
    /// Resolve and validate IMPI/IMPU before proxy/tunnel startup; these
    /// zeroizing values are bound to the same generation as IKE AKA.
    #[allow(dead_code, reason = "consumed by the gated native IMS session owner")]
    subscriber_identity: NativeImsSubscriberIdentity,
    /// Read once from the existing Go-owned persistent identity and bound to
    /// this context. It must never appear in plan diagnostics or logs.
    #[allow(dead_code)] // Consumed by the gated native IMS runtime owner.
    device_identity: NativeImsDeviceIdentity,
    transport: SelectedTransport,
    /// The Go bridge keeps the selected PC/SC or ESTK BLE reader reserved for
    /// this context's lifetime. IKE/IMS AKA must use this exact lease rather
    /// than opening a parallel PC/SC transaction against a mutable selection.
    #[allow(dead_code)] // Moved into the keyed actor when live session routes are enabled.
    lease: std::sync::Arc<crate::reader_session_lease::NativeReaderSessionLease>,
}

impl NativeSessionContext {
    #[allow(dead_code)] // Becomes live only after protected IMS runtime takeover.
    async fn authenticate_ike(
        &self,
        challenge: proletarius_ike::eap_aka::EapAkaChallenge,
    ) -> Result<SimAkaResult, NativeAkaError> {
        self.lease
            .authenticate_ike(&challenge.rand, &challenge.autn)
            .await
            .map_err(NativeAkaError::Lease)
    }

    /// Register over controls serviced by the same authenticated tunnel and
    /// SIM lease. P-CSCF is taken only from IKE CFG_REPLY; this path does not
    /// resolve IMS names through local DNS. Initial, AKA and protected
    /// REGISTER all consume the identity prepared with this exact lease.
    #[allow(dead_code, reason = "consumed by the gated keyed native session actor")]
    async fn register_ims(
        &self,
        tunnel: &NativeTunnel,
        services: RegistrationServices<'_>,
        endpoints: NativeSecurityEndpoints,
        deadline: tokio::time::Instant,
        stop: tokio::sync::watch::Receiver<bool>,
    ) -> Result<RegisteredImsSession, NativeImsRegistrationError> {
        let (local_ip, pcscf) = select_ims_runtime_endpoints(&self.ims, tunnel.configuration())
            .map_err(NativeImsRegistrationError::Endpoint)?;
        let tokens = NativeSipTransactionIds::generate()
            .map_err(|()| NativeImsRegistrationError::Entropy)?;
        let runtime = NativeImsRuntime {
            imsi: self.lease.identity.imsi.as_str(),
            isim_private_identity: Some(self.lease.identity.private_identity.as_str()),
            isim_public_identity: Some(self.lease.identity.public_identity.as_str()),
            local_ip,
            pcscf,
            call_id: &tokens.call_id,
            from_tag: &tokens.from_tag,
            via_branch: &tokens.via_branch,
            cell_id: None,
            mcc: &self.mcc,
            mnc: &self.mnc,
        };
        let lease = self.lease.clone();
        register_native_profile_with_identity(
            services,
            &self.ims,
            &self.device_identity,
            &self.subscriber_identity,
            runtime,
            endpoints,
            deadline,
            stop,
            move |rand, autn| {
                let lease = lease.clone();
                async move {
                    let result = lease
                        .authenticate_ims(&rand, &autn)
                        .await
                        .map_err(NativeAkaError::Lease)?;
                    Ok::<_, NativeAkaError>(ims_sim_answer(result))
                }
            },
        )
        .await
        .map_err(|error| NativeImsRegistrationError::Registration(Box::new(error)))
    }

    /// Starts the IKE owner from this exact context. This intentionally has no
    /// public HTTP route yet: a tunnel alone must never be represented as
    /// IMS/SMS/voice readiness before the protected runtime and its lifecycle
    /// manager are attached. The eventual supervisor owns this context and
    /// invokes this method at most once per generation.
    #[allow(dead_code)] // Enabled only by the next gated daemon owner.
    async fn start_tunnel(
        &self,
        actor_stop: tokio::sync::watch::Receiver<bool>,
    ) -> Result<NativeTunnelOwner, NativeTunnelStartError> {
        let mut heartbeat =
            crate::reader_session_lease::NativeReaderSessionHeartbeat::start(self.lease.clone());
        // Never give the supervisor's stop receiver directly to IKE. Once IMS
        // is registered, the keyed actor must first shut down the protected
        // REGISTER/SIP owner, then signal IKE to send its protected DELETE.
        let (ike_stop, ike_stop_receiver) = tokio::sync::watch::channel(false);
        let actor_stop_forwarder = forward_startup_stop(actor_stop, ike_stop.clone());
        let transport = tokio::select! {
            result = EpdgDatagramTransport::open(
                self.transport.mode.clone(),
                &self.config.epdg,
                self.config.epdg_port,
                self.transport.proxy,
            ) => match result {
                Ok(transport) => transport,
                Err(error) => {
                    actor_stop_forwarder.abort();
                    let _ = actor_stop_forwarder.await;
                    heartbeat.stop().await;
                    return Err(NativeTunnelStartError::Transport(error));
                }
            },
            _ = heartbeat.wait_lost() => {
                actor_stop_forwarder.abort();
                let _ = actor_stop_forwarder.await;
                heartbeat.stop().await;
                return Err(NativeTunnelStartError::LeaseLost);
            }
        };
        let lease = &self.lease;
        let settings = NativeTunnelSettings {
            identity: self.config.identity(),
            apn: &self.config.apn,
            ike_proposals: &self.config.proposals.ike,
            esp_proposals: &self.config.proposals.esp,
            ip_stack: self.config.ip_stack,
            checkcode_mode: self.config.aka_challenge_mode,
            initial_contact: true,
        };
        let startup = start_configured_native_tunnel(
            transport,
            settings,
            move |challenge| async move {
                lease
                    .authenticate_ike(&challenge.rand, &challenge.autn)
                    .await
                    .map_err(NativeAkaError::Lease)
            },
            ike_stop_receiver,
        );
        tokio::pin!(startup);
        let result = tokio::select! {
            result = &mut startup => result,
            _ = heartbeat.wait_lost() => {
                let _ = ike_stop.send(true);
                // If IKE completed just before the lease failure, retain the
                // tunnel long enough for its owner to send the ordered DELETE.
                let startup_result = startup.await;
                actor_stop_forwarder.abort();
                let _ = actor_stop_forwarder.await;
                return match startup_result {
                    Ok(tunnel) => Ok(NativeTunnelOwner {
                        tunnel,
                        lease_heartbeat: heartbeat,
                        ike_stop,
                    }),
                    Err(_) => {
                        heartbeat.stop().await;
                        Err(NativeTunnelStartError::LeaseLost)
                    }
                };
            }
        };
        actor_stop_forwarder.abort();
        let _ = actor_stop_forwarder.await;
        match result {
            Ok(tunnel) => Ok(NativeTunnelOwner {
                tunnel,
                lease_heartbeat: heartbeat,
                ike_stop,
            }),
            Err(error) => {
                heartbeat.stop().await;
                Err(NativeTunnelStartError::Startup(error))
            }
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum NativeImsEndpointSelectionError {
    MissingTunnelPcscf,
    ConfiguredPcscfNotAdvertised,
    NoMatchingTunnelAddress,
}

fn select_ims_runtime_endpoints(
    profile: &NativeImsRegisterProfile,
    tunnel: &TunnelConfiguration,
) -> Result<(IpAddr, SocketAddr), NativeImsEndpointSelectionError> {
    let configured = profile.configured_pcscf();
    let pcscf_ip = if let Some(configured) = configured {
        if !tunnel.pcscf_servers.contains(&configured.ip()) {
            return Err(NativeImsEndpointSelectionError::ConfiguredPcscfNotAdvertised);
        }
        configured.ip()
    } else {
        tunnel
            .pcscf_servers
            .iter()
            .copied()
            .find(|pcscf| tunnel_local_address(tunnel, pcscf.is_ipv4()).is_some())
            .ok_or(NativeImsEndpointSelectionError::MissingTunnelPcscf)?
    };
    let local_ip = tunnel_local_address(tunnel, pcscf_ip.is_ipv4())
        .ok_or(NativeImsEndpointSelectionError::NoMatchingTunnelAddress)?;
    let port = configured.map_or(5_060, |endpoint| endpoint.port());
    if port == 0 {
        return Err(NativeImsEndpointSelectionError::ConfiguredPcscfNotAdvertised);
    }
    Ok((local_ip, SocketAddr::new(pcscf_ip, port)))
}

fn tunnel_local_address(tunnel: &TunnelConfiguration, ipv4: bool) -> Option<IpAddr> {
    if ipv4 {
        tunnel.ipv4_addresses.first().copied().map(IpAddr::V4)
    } else {
        tunnel
            .ipv6_addresses
            .first()
            .map(|(address, _)| IpAddr::V6(*address))
    }
}

struct NativeSipTransactionIds {
    call_id: String,
    from_tag: String,
    via_branch: String,
}

impl NativeSipTransactionIds {
    fn generate() -> Result<Self, ()> {
        let mut entropy = [0_u8; 40];
        if getrandom::fill(&mut entropy).is_err() {
            entropy.zeroize();
            return Err(());
        }
        let call_id = format!("{}@proletarius", hex(&entropy[..16]));
        let from_tag = hex(&entropy[16..24]);
        let via_branch = format!("z9hG4bK{}", hex(&entropy[24..]));
        entropy.zeroize();
        Ok(Self {
            call_id,
            from_tag,
            via_branch,
        })
    }
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(DIGITS[usize::from(byte >> 4)]));
        output.push(char::from(DIGITS[usize::from(byte & 0x0f)]));
    }
    output
}

fn ims_sim_answer(mut result: SimAkaResult) -> SimAnswer {
    match &mut result {
        SimAkaResult::Success { res, ck, ik } => SimAnswer::Success {
            res: Zeroizing::new(std::mem::take(res)),
            ck: Zeroizing::new(*ck),
            ik: Zeroizing::new(*ik),
        },
        SimAkaResult::SynchronizationFailure { auts } => {
            SimAnswer::SynchronizationFailure(Zeroizing::new(*auts))
        }
        SimAkaResult::AuthenticationRejected => SimAnswer::Rejected,
    }
}

enum NativeImsRegistrationError {
    Endpoint(NativeImsEndpointSelectionError),
    Entropy,
    Registration(Box<NativeProfileRegistrationFailure<NativeAkaError>>),
}

fn forward_startup_stop(
    mut actor_stop: tokio::sync::watch::Receiver<bool>,
    ike_stop: tokio::sync::watch::Sender<bool>,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        loop {
            if *actor_stop.borrow() || actor_stop.has_changed().is_err() {
                let _ = ike_stop.send(true);
                return;
            }
            if actor_stop.changed().await.is_err() {
                let _ = ike_stop.send(true);
                return;
            }
        }
    })
}

/// Owns the authenticated tunnel and the reader lease heartbeat together.
/// Dropping one without the other is impossible; the future keyed session
/// actor must watch `lease_lost_receiver` and orderly-stop IMS before IKE.
#[allow(dead_code, reason = "consumed by the gated keyed native session actor")]
struct NativeTunnelOwner {
    tunnel: NativeTunnel,
    lease_heartbeat: crate::reader_session_lease::NativeReaderSessionHeartbeat,
    ike_stop: tokio::sync::watch::Sender<bool>,
}

impl NativeTunnelOwner {
    #[allow(
        dead_code,
        reason = "used by the single network task that owns this IKE tunnel"
    )]
    fn ike_stop_receiver(&self) -> tokio::sync::watch::Receiver<bool> {
        self.ike_stop.subscribe()
    }

    #[allow(dead_code, reason = "observed by the keyed native session actor")]
    fn lease_lost_receiver(&self) -> tokio::sync::watch::Receiver<bool> {
        self.lease_heartbeat.lost_receiver()
    }

    #[allow(
        dead_code,
        reason = "sent after RegisteredImsSession::shutdown by the keyed actor"
    )]
    fn request_ike_stop(&self) {
        let _ = self.ike_stop.send(true);
    }

    /// Run protected IMS cleanup independently of the caller's task, and only
    /// then release IKE to send its protected DELETE. Dropping the returned
    /// JoinHandle detaches the cleanup task; it does not cancel the ordering
    /// barrier if the keyed actor itself is being torn down.
    #[allow(dead_code, reason = "consumed by the gated keyed native session actor")]
    fn shutdown_registered_ims(
        &self,
        session: proletarius_vowifi::registration_coordinator::RegisteredImsSession,
    ) -> tokio::task::JoinHandle<
        Result<(), proletarius_vowifi::registration_coordinator::CleanupReport>,
    > {
        spawn_ims_shutdown_before_ike(self.ike_stop.clone(), session.shutdown())
    }
}

fn spawn_ims_shutdown_before_ike<T, F>(
    ike_stop: tokio::sync::watch::Sender<bool>,
    shutdown: F,
) -> tokio::task::JoinHandle<T>
where
    T: Send + 'static,
    F: Future<Output = T> + Send + 'static,
{
    tokio::spawn(async move {
        let result = shutdown.await;
        let _ = ike_stop.send(true);
        result
    })
}

#[derive(Debug)]
#[allow(dead_code)] // Returned by the gated native tunnel owner.
enum NativeAkaError {
    Lease(proletarius_domain::BackendFailure),
}

#[derive(Debug)]
#[allow(dead_code)] // Returned by the gated daemon owner.
enum NativeTunnelStartError {
    LeaseLost,
    Transport(DatagramTransportError),
    Startup(TunnelStartupError<NativeAkaError>),
}

pub(crate) async fn handle(
    request: Request<Incoming>,
    go_socket_path: &Path,
    app_token: Option<&str>,
) -> Response<Full<Bytes>> {
    let body = match tokio::time::timeout(
        Duration::from_secs(2),
        Limited::new(request.into_body(), REQUEST_LIMIT).collect(),
    )
    .await
    {
        Ok(Ok(body)) => body.to_bytes(),
        Ok(Err(_)) => {
            return static_failure(
                StatusCode::BAD_REQUEST,
                "invalid_native_session_request",
                false,
            );
        }
        Err(_) => return static_failure(StatusCode::REQUEST_TIMEOUT, "request_timeout", true),
    };
    let Ok(request) = serde_json::from_slice::<PlanRequest>(&body) else {
        return static_failure(
            StatusCode::BAD_REQUEST,
            "invalid_native_session_request",
            false,
        );
    };
    if let Err(error) = validate_request(&request) {
        return failure(error);
    }
    let lease = match crate::reader_session_lease::NativeReaderSessionLease::acquire(
        go_socket_path,
        app_token,
        request.reader.trim(),
    )
    .await
    {
        Ok(lease) => lease,
        Err(_) => {
            return static_failure(
                StatusCode::SERVICE_UNAVAILABLE,
                "reader_session_lease_unavailable",
                true,
            );
        }
    };
    let plan = match prepare(request, lease).await {
        Ok(plan) => plan,
        Err((error, lease)) => {
            if lease.release().await.is_err() {
                return static_failure(
                    StatusCode::SERVICE_UNAVAILABLE,
                    "reader_session_lease_release_failed",
                    true,
                );
            }
            return failure(error);
        }
    };
    let readiness = native_session_owner_readiness();
    let response = json!({
        "ok": true,
        "reader": &plan.reader,
        "reader_generation": plan.reader_generation,
        "native_actor_ready": readiness.ready(),
        "owner_missing": readiness.missing(),
        "mcc": &plan.mcc,
        "mnc": &plan.mnc,
        "carrier_id": &plan.carrier_id,
        "epdg": &plan.config.epdg,
        "epdg_port": plan.config.epdg_port,
        "epdg_source": &plan.config.epdg_source,
        "apn": &plan.config.apn,
        "ip_stack": ip_stack_name(plan.config.ip_stack),
        "ike_proposals": &plan.config.proposals.ike,
        "esp_proposals": &plan.config.proposals.esp,
        "transport": plan.transport.mode.as_str(),
        "proxy_source": plan.transport.proxy_source,
        "proxy_error": &plan.transport.proxy_error,
        "ims": {
            "domain": plan.ims.domain(),
            "realm": plan.ims.realm(),
            "phone_context": plan.ims.phone_context(),
            "configured_pcscf": plan.ims.configured_pcscf().map(|address| address.to_string()),
            "transport": plan.ims.transport().as_str(),
            "local_port": plan.ims.local_port(),
            "tcp_keepalive_seconds": plan.ims.tcp_keepalive_seconds(),
            "options_ping_interval_seconds": plan.ims.options_ping_interval_seconds(),
            "register_template": plan.ims.template().id(),
            "register_expires_seconds": plan.ims.template().expires_seconds(),
            "sec_agree": ims_sec_agree_name(plan.ims.template().sec_agree()),
            "strict_security_server_offer": plan.ims.template().strict_security_server_offer(),
            "device_identity": {
                "mode": plan.ims.device_identity_mode(),
                "enabled": plan.device_identity.enabled(),
            },
        },
    });
    if plan.lease.release().await.is_err() {
        return static_failure(
            StatusCode::SERVICE_UNAVAILABLE,
            "reader_session_lease_release_failed",
            true,
        );
    }
    crate::json_response(StatusCode::OK, &response)
}

/// This stays fixed until all owner gates are backed by the same real native
/// actor. Do not replace it with an environment flag: that would let a
/// diagnostic request violate Go's live reader ownership boundary.
const fn native_session_owner_readiness() -> NativeOwnerReadiness {
    NativeOwnerReadiness::current()
}

#[allow(dead_code)]
fn native_session_owner_not_ready(readiness: NativeOwnerReadiness) -> Response<Full<Bytes>> {
    crate::json_response(
        StatusCode::CONFLICT,
        &json!({
            "error": "native_session_owner_not_ready",
            "code": "native_session_owner_not_ready",
            "retryable": false,
            "missing": readiness.missing(),
        }),
    )
}

async fn prepare(
    request: PlanRequest,
    lease: crate::reader_session_lease::NativeReaderSessionLease,
) -> Result<
    NativeSessionContext,
    (
        PlanError,
        crate::reader_session_lease::NativeReaderSessionLease,
    ),
> {
    macro_rules! fail {
        ($error:expr) => {
            return Err(($error, lease))
        };
    }
    let reader = request.reader.trim();
    if lease.reader != reader || lease.reader_generation == 0 {
        fail!(PlanError::ReaderIdentity("reader_identity_changed"));
    }
    let identity = &lease.identity;
    if identity.mnc.is_empty() {
        fail!(PlanError::Carrier);
    }
    let session_key = match crate::native_vowifi_supervisor::ReaderSessionKey::new(
        reader,
        identity.iccid.as_str(),
    ) {
        Ok(key) => key,
        Err(_) => fail!(PlanError::ReaderIdentity("reader_identity_failed")),
    };
    let sim = SimIdentity {
        mcc: identity.mcc.clone(),
        mnc: identity.mnc.clone(),
        imsi: identity.imsi.to_string(),
        iccid: identity.iccid.to_string(),
        gid1: identity.gid1.clone(),
        gid2: identity.gid2.clone(),
        spn: identity.spn.clone(),
    };
    let registry = match CARRIER_REGISTRY.get_or_init(Registry::embedded).as_ref() {
        Ok(registry) => registry,
        Err(_) => fail!(PlanError::Carrier),
    };
    let carrier = match registry.resolve(&sim) {
        Ok(Some(carrier)) => carrier,
        Ok(None) | Err(_) => fail!(PlanError::Carrier),
    };
    let overrides_set = match load_overrides() {
        Ok(overrides) => overrides,
        Err(error) => fail!(error),
    };
    let override_value = overrides_set.get(&sim.mcc, &sim.mnc).cloned();
    let mut overrides = override_value
        .as_ref()
        .map(|override_value| override_value.tunnel_overrides())
        .unwrap_or_default();
    if let Err(error) = apply_request_overrides(&mut overrides, request.epdg, request.epdg_port) {
        fail!(error);
    }
    let config = match carrier.native_tunnel_config(&sim, &overrides) {
        Ok(config) => config,
        Err(_) => fail!(PlanError::Carrier),
    };
    let ims = match carrier.native_ims_register_profile(&sim, override_value.as_ref()) {
        Ok(profile) => profile,
        Err(_) => fail!(PlanError::Carrier),
    };
    let subscriber_identity = match resolve_session_subscriber_identity(&ims, identity) {
        Ok(identity) => identity,
        Err(error) => fail!(PlanError::SubscriberIdentity(error)),
    };
    // The Go compatibility owner remains the single writer while migration is
    // in progress. Native preparation can only read its existing value; it
    // must fail before transport I/O if no stable terminal identity exists.
    let persisted_imei = match crate::reader_device_identity::load_go_persisted_imei() {
        Ok(imei) => imei,
        Err(error) => fail!(PlanError::DeviceIdentityConfig(error)),
    };
    let device_identity =
        match ims.resolve_device_identity(persisted_imei.as_ref().map(|value| value.as_str())) {
            Ok(identity) => identity,
            Err(error) => fail!(PlanError::DeviceIdentity(error)),
        };
    let transport =
        match crate::transport_selection::select_for_session(request.proxy.trim(), &config.epdg)
            .await
        {
            Ok(transport) => transport,
            Err(error) => fail!(PlanError::Transport(error)),
        };
    let reader_generation = lease.reader_generation;
    Ok(NativeSessionContext {
        reader: reader.to_owned(),
        reader_generation,
        session_key,
        mcc: sim.mcc,
        mnc: sim.mnc,
        carrier_id: carrier.id,
        config,
        ims,
        subscriber_identity,
        device_identity,
        transport,
        lease: std::sync::Arc::new(lease),
    })
}

fn resolve_session_subscriber_identity(
    profile: &NativeImsRegisterProfile,
    identity: &crate::reader_session_lease::LeaseIdentity,
) -> Result<NativeImsSubscriberIdentity, NativeImsProfileError> {
    profile.resolve_subscriber_identity_from_snapshot(
        identity.imsi.as_str(),
        Some(identity.private_identity.as_str()),
        Some(identity.public_identity.as_str()),
    )
}

/// Reject malformed caller input before acquiring a PC/SC transaction or
/// starting DNS/route/proxy discovery. Carrier-derived ePDG values are still
/// validated by `select_for_session` after the same SIM snapshot resolves.
fn validate_request(request: &PlanRequest) -> Result<(), PlanError> {
    let reader = request.reader.trim();
    if reader.is_empty()
        || reader.len() > 512
        || reader.bytes().any(|byte| byte.is_ascii_control())
        || request.epdg_port == Some(0)
    {
        return Err(PlanError::InvalidRequest);
    }
    let epdg = request.epdg.trim();
    if !epdg.is_empty() && !crate::transport_selection::valid_hostname(epdg) {
        return Err(PlanError::InvalidEpdg);
    }
    let proxy = request.proxy.trim();
    if proxy.len() > 512 || proxy.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(PlanError::InvalidRequest);
    }
    Ok(())
}

fn apply_request_overrides(
    overrides: &mut NativeTunnelOverrides,
    epdg: String,
    epdg_port: Option<u16>,
) -> Result<(), PlanError> {
    let epdg = epdg.trim();
    if !epdg.is_empty() {
        overrides.epdg = Some(epdg.to_owned());
    }
    if let Some(port) = epdg_port {
        if port == 0 {
            return Err(PlanError::InvalidRequest);
        }
        overrides.epdg_port = Some(port);
    }
    Ok(())
}

fn load_overrides() -> Result<OverrideSet, PlanError> {
    let path = override_path()?;
    let data = match fs::read(path) {
        Ok(data) => data,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(OverrideSet::default());
        }
        Err(_) => return Err(PlanError::Overrides),
    };
    if data.iter().all(u8::is_ascii_whitespace) {
        return Ok(OverrideSet::default());
    }
    OverrideSet::parse(&data).map_err(|_| PlanError::Overrides)
}

fn override_path() -> Result<PathBuf, PlanError> {
    if let Some(path) = env::var_os(OVERRIDES_ENVIRONMENT)
        && !path.is_empty()
    {
        let path = PathBuf::from(path);
        return if path.is_absolute() {
            Ok(path)
        } else {
            Err(PlanError::Overrides)
        };
    }
    Ok(crate::data_root::resolve()
        .map_err(|_| PlanError::Overrides)?
        .join("carrier-overrides.yaml"))
}

fn ip_stack_name(value: proletarius_vowifi::RequestedIpStack) -> &'static str {
    match value {
        proletarius_vowifi::RequestedIpStack::Ipv4 => "ipv4",
        proletarius_vowifi::RequestedIpStack::Ipv6 => "ipv6",
        proletarius_vowifi::RequestedIpStack::Dual => "ipv4v6",
    }
}

fn ims_sec_agree_name(value: proletarius_ims::SecAgreeMode) -> &'static str {
    match value {
        proletarius_ims::SecAgreeMode::Disabled => "disabled",
        proletarius_ims::SecAgreeMode::Auto => "auto",
        proletarius_ims::SecAgreeMode::Required => "required",
    }
}

#[derive(Debug)]
enum PlanError {
    InvalidRequest,
    InvalidEpdg,
    #[allow(
        dead_code,
        reason = "preserves typed reader-worker error mapping for the native actor route"
    )]
    Reader(crate::reader_task::ReaderTaskError),
    ReaderIdentity(&'static str),
    Carrier,
    Overrides,
    SubscriberIdentity(NativeImsProfileError),
    DeviceIdentity(NativeImsProfileError),
    DeviceIdentityConfig(crate::reader_device_identity::PersistedDeviceIdentityError),
    Transport(SessionSelectionError),
}

impl PlanError {
    fn status(&self) -> StatusCode {
        match self {
            Self::InvalidRequest | Self::InvalidEpdg => StatusCode::UNPROCESSABLE_ENTITY,
            Self::Reader(crate::reader_task::ReaderTaskError::Timeout) => {
                StatusCode::GATEWAY_TIMEOUT
            }
            Self::Reader(crate::reader_task::ReaderTaskError::WorkerFailed) => {
                StatusCode::INTERNAL_SERVER_ERROR
            }
            Self::Reader(crate::reader_task::ReaderTaskError::Busy) => StatusCode::CONFLICT,
            Self::ReaderIdentity("reader_busy") => StatusCode::SERVICE_UNAVAILABLE,
            Self::ReaderIdentity(
                "reader_card_absent" | "reader_unavailable" | "reader_identity_changed",
            ) => StatusCode::CONFLICT,
            Self::ReaderIdentity(_) => StatusCode::BAD_GATEWAY,
            Self::Carrier
            | Self::Overrides
            | Self::SubscriberIdentity(_)
            | Self::DeviceIdentity(_)
            | Self::DeviceIdentityConfig(_) => StatusCode::UNPROCESSABLE_ENTITY,
            Self::Transport(SessionSelectionError::TimedOut) => StatusCode::GATEWAY_TIMEOUT,
            Self::Transport(_) => StatusCode::BAD_GATEWAY,
        }
    }

    fn code(&self) -> &'static str {
        match self {
            Self::InvalidRequest => "invalid_native_session_request",
            Self::InvalidEpdg => "invalid_epdg_hostname",
            Self::Reader(crate::reader_task::ReaderTaskError::Timeout) => {
                "reader_operation_timeout"
            }
            Self::Reader(crate::reader_task::ReaderTaskError::WorkerFailed) => "reader_task_failed",
            Self::Reader(crate::reader_task::ReaderTaskError::Busy) => "card_busy",
            Self::ReaderIdentity(code) => code,
            Self::Carrier => "native_carrier_selection_failed",
            Self::Overrides => "native_carrier_overrides_invalid",
            Self::SubscriberIdentity(NativeImsProfileError::IncompleteIsimIdentity) => {
                "native_ims_identity_incomplete"
            }
            Self::SubscriberIdentity(_) => "native_ims_identity_invalid",
            Self::DeviceIdentity(NativeImsProfileError::MissingPersistentDeviceIdentity) => {
                "native_device_identity_missing"
            }
            Self::DeviceIdentity(_) => "native_device_identity_invalid",
            Self::DeviceIdentityConfig(
                crate::reader_device_identity::PersistedDeviceIdentityError::Unreadable,
            ) => "native_device_identity_unreadable",
            Self::DeviceIdentityConfig(
                crate::reader_device_identity::PersistedDeviceIdentityError::Oversized,
            ) => "native_device_identity_oversized",
            Self::Transport(SessionSelectionError::InvalidEpdg) => "invalid_epdg_hostname",
            Self::Transport(SessionSelectionError::TimedOut) => "transport_selection_timeout",
            Self::Transport(SessionSelectionError::MissingVerifiedProxy) => {
                "proxy_address_unavailable"
            }
            Self::Transport(SessionSelectionError::Policy { .. }) => "native_transport_unavailable",
        }
    }

    fn retryable(&self) -> bool {
        matches!(
            self,
            Self::Reader(crate::reader_task::ReaderTaskError::Timeout)
                | Self::ReaderIdentity("reader_busy" | "reader_identity_failed")
                | Self::Transport(_)
        )
    }
}

fn failure(error: PlanError) -> Response<Full<Bytes>> {
    let status = error.status();
    let code = error.code();
    static_failure(status, code, error.retryable())
}

fn static_failure(status: StatusCode, code: &str, retryable: bool) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error": code, "code": code, "retryable": retryable}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use http_body_util::BodyExt;

    #[test]
    fn request_overrides_are_highest_priority_and_reject_zero_port() {
        let mut overrides = NativeTunnelOverrides {
            epdg: Some("template.example".into()),
            epdg_port: Some(500),
            ..Default::default()
        };
        apply_request_overrides(&mut overrides, " custom.example ".into(), Some(4500)).unwrap();
        assert_eq!(overrides.epdg.as_deref(), Some("custom.example"));
        assert_eq!(overrides.epdg_port, Some(4500));
        assert!(apply_request_overrides(&mut overrides, String::new(), Some(0)).is_err());
    }

    #[test]
    fn session_identity_is_resolved_from_the_generation_bound_lease() {
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "33".into(),
            imsi: "234330000000001".into(),
            iccid: "8985200014631959072".into(),
            ..Default::default()
        };
        let carrier = Registry::embedded()
            .unwrap()
            .resolve(&sim)
            .unwrap()
            .unwrap();
        let overrides =
            OverrideSet::parse(b"carrier_overrides:\n  '234/33': {ims_identity_source: isim}\n")
                .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        let lease_identity = crate::reader_session_lease::LeaseIdentity {
            iccid: zeroize::Zeroizing::new(sim.iccid.clone()),
            imsi: zeroize::Zeroizing::new(sim.imsi.clone()),
            mcc: sim.mcc.clone(),
            mnc: sim.mnc.clone(),
            gid1: String::new(),
            gid2: String::new(),
            spn: String::new(),
            private_identity: zeroize::Zeroizing::new("subscriber@ims.example.test".into()),
            public_identity: zeroize::Zeroizing::new("sip:+447700900123@ims.example.test".into()),
        };

        let resolved = resolve_session_subscriber_identity(&profile, &lease_identity).unwrap();
        assert_eq!(resolved.private_identity(), "subscriber@ims.example.test");
        assert_eq!(
            resolved.public_identity(),
            "sip:+447700900123@ims.example.test"
        );

        let incomplete_identity = crate::reader_session_lease::LeaseIdentity {
            private_identity: zeroize::Zeroizing::default(),
            ..lease_identity
        };
        assert!(matches!(
            resolve_session_subscriber_identity(&profile, &incomplete_identity),
            Err(NativeImsProfileError::IncompleteIsimIdentity)
        ));
    }

    #[test]
    fn ims_endpoint_selection_uses_only_the_authenticated_tunnel_configuration() {
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "33".into(),
            imsi: "234330000000001".into(),
            ..Default::default()
        };
        let carrier = Registry::embedded()
            .unwrap()
            .resolve(&sim)
            .unwrap()
            .unwrap();
        let profile = carrier.native_ims_register_profile(&sim, None).unwrap();
        let tunnel = TunnelConfiguration {
            ipv4_addresses: vec!["10.10.0.2".parse().unwrap()],
            pcscf_servers: vec!["10.10.0.3".parse().unwrap()],
            ..Default::default()
        };
        assert_eq!(
            select_ims_runtime_endpoints(&profile, &tunnel).unwrap(),
            (
                "10.10.0.2".parse().unwrap(),
                "10.10.0.3:5060".parse().unwrap(),
            )
        );

        let no_pcscf = TunnelConfiguration {
            ipv4_addresses: vec!["10.10.0.2".parse().unwrap()],
            ..Default::default()
        };
        assert_eq!(
            select_ims_runtime_endpoints(&profile, &no_pcscf),
            Err(NativeImsEndpointSelectionError::MissingTunnelPcscf)
        );
    }

    #[test]
    fn configured_pcscf_must_match_cfg_reply_and_use_a_matching_tunnel_address() {
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "33".into(),
            imsi: "234330000000001".into(),
            ..Default::default()
        };
        let carrier = Registry::embedded()
            .unwrap()
            .resolve(&sim)
            .unwrap()
            .unwrap();
        let overrides =
            OverrideSet::parse(b"carrier_overrides:\n  '234/33': {ims_pcscf: '10.10.0.3:5070'}\n")
                .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        let tunnel = TunnelConfiguration {
            ipv4_addresses: vec!["10.10.0.2".parse().unwrap()],
            pcscf_servers: vec!["10.10.0.3".parse().unwrap()],
            ..Default::default()
        };
        assert_eq!(
            select_ims_runtime_endpoints(&profile, &tunnel).unwrap(),
            (
                "10.10.0.2".parse().unwrap(),
                "10.10.0.3:5070".parse().unwrap(),
            )
        );

        let unadvertised = TunnelConfiguration {
            pcscf_servers: vec!["10.10.0.4".parse().unwrap()],
            ..tunnel.clone()
        };
        assert_eq!(
            select_ims_runtime_endpoints(&profile, &unadvertised),
            Err(NativeImsEndpointSelectionError::ConfiguredPcscfNotAdvertised)
        );

        let wrong_family = TunnelConfiguration {
            ipv4_addresses: Vec::new(),
            ipv6_addresses: vec![("2001:db8::2".parse().unwrap(), 64)],
            ..tunnel
        };
        assert_eq!(
            select_ims_runtime_endpoints(&profile, &wrong_family),
            Err(NativeImsEndpointSelectionError::NoMatchingTunnelAddress)
        );
    }

    #[tokio::test]
    async fn actor_stop_is_forwarded_to_the_private_ike_start_channel() {
        let (actor_stop_tx, actor_stop) = tokio::sync::watch::channel(false);
        let (ike_stop_tx, mut ike_stop) = tokio::sync::watch::channel(false);
        let forwarder = forward_startup_stop(actor_stop, ike_stop_tx);

        actor_stop_tx.send(true).unwrap();
        tokio::time::timeout(Duration::from_secs(1), ike_stop.changed())
            .await
            .expect("actor stop should reach the startup-owned IKE channel")
            .unwrap();
        assert!(*ike_stop.borrow());
        forwarder.await.unwrap();
    }

    #[tokio::test]
    async fn cancelling_the_actor_waiter_cannot_stop_ike_before_ims_cleanup() {
        let (ike_stop_tx, mut ike_stop) = tokio::sync::watch::channel(false);
        let (cleanup_entered_tx, cleanup_entered_rx) = tokio::sync::oneshot::channel();
        let (finish_cleanup_tx, finish_cleanup_rx) = tokio::sync::oneshot::channel();
        let cleanup = spawn_ims_shutdown_before_ike(ike_stop_tx, async move {
            cleanup_entered_tx.send(()).unwrap();
            finish_cleanup_rx.await.unwrap();
            Err::<(), _>("simulated IMS cleanup error")
        });

        // The actor may disappear while cleanup is in flight. The independently
        // owned cleanup task must still hold IKE open until IMS cleanup returns.
        drop(cleanup);
        cleanup_entered_rx.await.unwrap();
        assert!(!*ike_stop.borrow());
        finish_cleanup_tx.send(()).unwrap();
        tokio::time::timeout(Duration::from_secs(1), ike_stop.changed())
            .await
            .expect("IKE stop follows completion of IMS cleanup")
            .unwrap();
        assert!(*ike_stop.borrow());
    }

    #[test]
    fn malformed_plan_input_fails_before_any_reader_or_network_work() {
        for request in [
            PlanRequest {
                reader: "Reader\nA".into(),
                proxy: String::new(),
                epdg: String::new(),
                epdg_port: None,
            },
            PlanRequest {
                reader: "Reader A".into(),
                proxy: "proxy\0host:1080".into(),
                epdg: String::new(),
                epdg_port: None,
            },
            PlanRequest {
                reader: "Reader A".into(),
                proxy: String::new(),
                epdg: "https://epdg.example".into(),
                epdg_port: None,
            },
        ] {
            assert!(validate_request(&request).is_err());
        }
        assert!(
            validate_request(&PlanRequest {
                reader: "Reader A".into(),
                proxy: "127.0.0.1:7897".into(),
                epdg: "epdg.epc.mnc033.mcc234.pub.3gppnetwork.org".into(),
                epdg_port: Some(4500),
            })
            .is_ok()
        );
    }

    #[test]
    fn native_plan_readiness_has_lease_but_keeps_live_runtime_gates_closed() {
        let readiness = native_session_owner_readiness();
        assert!(readiness.cross_process_reader_lease);
        assert!(!readiness.ready());
        assert_eq!(
            readiness.missing(),
            vec![
                "ble_aka_transport_bridge",
                "keyed_lifecycle_actor",
                "protected_ims_runtime",
                "live_sms_call_runtime",
            ]
        );
    }

    #[test]
    fn native_owner_requires_every_gate() {
        let ready = NativeOwnerReadiness {
            cross_process_reader_lease: true,
            ble_aka_transport_bridge: true,
            keyed_lifecycle_actor: true,
            protected_ims_runtime: true,
            live_sms_call_runtime: true,
        };
        assert!(ready.ready());

        for readiness in [
            NativeOwnerReadiness {
                cross_process_reader_lease: false,
                ..ready
            },
            NativeOwnerReadiness {
                ble_aka_transport_bridge: false,
                ..ready
            },
            NativeOwnerReadiness {
                keyed_lifecycle_actor: false,
                ..ready
            },
            NativeOwnerReadiness {
                protected_ims_runtime: false,
                ..ready
            },
            NativeOwnerReadiness {
                live_sms_call_runtime: false,
                ..ready
            },
        ] {
            assert!(!readiness.ready());
        }
    }

    #[tokio::test]
    async fn native_owner_not_ready_response_remains_conflict_and_lists_blockers() {
        let response = native_session_owner_not_ready(native_session_owner_readiness());
        assert_eq!(response.status(), StatusCode::CONFLICT);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let payload: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(payload["code"], "native_session_owner_not_ready");
        assert_eq!(payload["retryable"], false);
        assert_eq!(
            payload["missing"],
            serde_json::json!([
                "ble_aka_transport_bridge",
                "keyed_lifecycle_actor",
                "protected_ims_runtime",
                "live_sms_call_runtime",
            ])
        );
    }

    #[test]
    fn reader_failure_codes_preserve_actionability_and_retryability() {
        let cases = [
            (
                PlanError::ReaderIdentity("reader_busy"),
                StatusCode::SERVICE_UNAVAILABLE,
                true,
            ),
            (
                PlanError::ReaderIdentity("reader_card_absent"),
                StatusCode::CONFLICT,
                false,
            ),
            (
                PlanError::Reader(crate::reader_task::ReaderTaskError::Timeout),
                StatusCode::GATEWAY_TIMEOUT,
                true,
            ),
            (
                PlanError::ReaderIdentity("reader_identity_failed"),
                StatusCode::BAD_GATEWAY,
                true,
            ),
            (
                PlanError::Transport(SessionSelectionError::TimedOut),
                StatusCode::GATEWAY_TIMEOUT,
                true,
            ),
        ];
        for (error, status, retryable) in cases {
            assert_eq!(error.status(), status);
            assert_eq!(error.retryable(), retryable);
        }
        assert_eq!(
            PlanError::Transport(SessionSelectionError::TimedOut).code(),
            "transport_selection_timeout"
        );
    }

    #[test]
    fn fallback_config_path_has_expected_filename() {
        // The path selection itself has no I/O and the inherited test process
        // must not mutate its environment. This asserts the stable fallback.
        let path = override_path().expect("configured Switch data path");
        assert!(path.ends_with("carrier-overrides.yaml"));
    }

    #[test]
    fn persistent_identity_failures_stop_before_network_transport() {
        let missing =
            PlanError::DeviceIdentity(NativeImsProfileError::MissingPersistentDeviceIdentity);
        assert_eq!(missing.status(), StatusCode::UNPROCESSABLE_ENTITY);
        assert_eq!(missing.code(), "native_device_identity_missing");

        let unreadable = PlanError::DeviceIdentityConfig(
            crate::reader_device_identity::PersistedDeviceIdentityError::Unreadable,
        );
        assert_eq!(unreadable.status(), StatusCode::UNPROCESSABLE_ENTITY);
        assert_eq!(unreadable.code(), "native_device_identity_unreadable");

        let incomplete_isim =
            PlanError::SubscriberIdentity(NativeImsProfileError::IncompleteIsimIdentity);
        assert_eq!(incomplete_isim.status(), StatusCode::UNPROCESSABLE_ENTITY);
        assert_eq!(incomplete_isim.code(), "native_ims_identity_incomplete");
    }
}
