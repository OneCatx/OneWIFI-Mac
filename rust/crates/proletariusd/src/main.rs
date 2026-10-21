#![forbid(unsafe_code)]

mod archive_io;
mod availability;
mod call_archive;
mod call_codec;
mod carrier;
mod data_root;
mod eap_identity;
mod gateway;
mod ims_codec;
mod message_archive;
mod multistandby_store;
mod native_vowifi_session;
mod native_vowifi_supervisor;
mod ownership;
mod proxy_policy;
mod proxy_probe;
mod reader_aka_binding;
mod reader_auth;
mod reader_device_identity;
mod reader_eap_aka;
mod reader_identity;
mod reader_inventory;
mod reader_reservation;
mod reader_session_lease;
mod reader_task;
mod sms_codec;
mod snapshot;
mod supervisor;
mod system_proxy;
mod transport_selection;
mod upstream;
mod vowifi_session_policy;

use std::convert::Infallible;
use std::env;
use std::fs;
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::AtomicU64;
use std::sync::{Arc, Mutex};

use bytes::Bytes;
use http_body_util::Full;
use hyper::body::Incoming;
use hyper::header::{HeaderName, HeaderValue};
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use nix::unistd::{Pid, getppid};
use proletarius_api_contract::{ErrorResponse, RuntimeResponse};
use proletarius_call::CALL_PROTOCOL_CONTRACT;
use proletarius_domain::{BackendFailure, MigrationStage};
use proletarius_ike::IKE_WIRE_CONTRACT;
use proletarius_ims::IMS_PROTOCOL_CONTRACT;
use proletarius_reader::ReaderDiscovery;
use proletarius_sim::SIM_PROTOCOL_CONTRACT;
use proletarius_sms::SMS_PROTOCOL_CONTRACT;
use proletarius_store::STORE_CONTRACT;
use proletarius_vowifi::PROXY_CONTRACT;
use subtle::ConstantTimeEq;
use tokio::net::{UnixListener, UnixStream};
use tokio::signal::unix::{SignalKind, signal};
use tokio::task::JoinSet;
use tokio::time::{Duration, Instant, interval, sleep_until};

use availability::{BackendAvailability, BackendPhase};
use gateway::GoBackendProcess;
use supervisor::{RestartDecision, RestartTracker};

const APP_TOKEN_ENVIRONMENT: &str = "PROLETARIUS_APP_TOKEN";
const APP_TOKEN_HEADER: &str = "x-proletarius-app-token";
const BACKEND_VERSION: &str = env!("CARGO_PKG_VERSION");
const READ_RECOVERY_WAIT: Duration = Duration::from_secs(5);

#[derive(Clone)]
struct ServerState {
    app_token: Option<String>,
    internal_token: String,
    multistandby_store: multistandby_store::Store,
    go_socket_path: PathBuf,
    availability: BackendAvailability,
    reader_discovery: Arc<Mutex<ReaderDiscovery>>,
    snapshot_instance_id: String,
    snapshot_revision: Arc<AtomicU64>,
    eap_exchanges: Arc<eap_identity::EapExchangeStore>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum RecoveryAdmission {
    Forward,
    WaitForRead,
    RejectMutation,
}

struct SocketGuard {
    path: PathBuf,
}

impl Drop for SocketGuard {
    fn drop(&mut self) {
        let _ = fs::remove_file(&self.path);
    }
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("switchd-rs: {error}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), BackendFailure> {
    let data_root = data_root::resolve().map_err(BackendFailure::Io)?;
    let multistandby_store = multistandby_store::Store::open(&data_root)
        .map_err(|e| BackendFailure::Io(e.to_string()))?;
    multistandby_store
        .recover()
        .map_err(|e| BackendFailure::Io(e.to_string()))?;
    let internal_token =
        multistandby_store::random_token().map_err(|e| BackendFailure::Io(e.to_string()))?;
    let socket_path = parse_listen_path(env::args().skip(1))?;
    prepare_socket_path(&socket_path)?;
    let listener = UnixListener::bind(&socket_path).map_err(io_failure)?;
    fs::set_permissions(&socket_path, fs::Permissions::from_mode(0o600)).map_err(io_failure)?;
    let _guard = SocketGuard {
        path: socket_path.clone(),
    };
    // Bind the public socket before starting the Go owner. Swift can now
    // distinguish "gateway alive, hardware backend starting" from a daemon
    // launch failure instead of incorrectly reporting a five-second timeout.
    let initial_backend = GoBackendProcess::spawn(&socket_path, &internal_token)?;
    let go_socket_path = initial_backend.socket_path().to_owned();
    let availability = BackendAvailability::new();
    let state = ServerState {
        app_token: normalized_app_token(),
        internal_token: internal_token.clone(),
        multistandby_store,
        go_socket_path,
        availability: availability.clone(),
        reader_discovery: Arc::new(Mutex::new(ReaderDiscovery::default())),
        snapshot_instance_id: snapshot::new_instance_id(),
        snapshot_revision: Arc::new(AtomicU64::new(0)),
        eap_exchanges: Arc::new(eap_identity::EapExchangeStore::default()),
    };
    let mut go_backend = Some(initial_backend);
    let mut backend_started_at = Some(Instant::now());
    let mut restart_at = None;
    let mut restart_tracker = RestartTracker::default();
    let mut tasks = JoinSet::new();
    let mut terminate = signal(SignalKind::terminate()).map_err(io_failure)?;
    let mut interrupt = signal(SignalKind::interrupt()).map_err(io_failure)?;
    // The Swift parent owns the stdout/stderr pipe readers. If it crashes,
    // writes return EPIPE; handling SIGPIPE keeps the gateway alive long
    // enough to terminate its Go compatibility child and remove both sockets.
    let mut broken_pipe = signal(SignalKind::pipe()).map_err(io_failure)?;
    let mut child_monitor = interval(Duration::from_millis(250));
    let mut eap_exchange_cleanup = interval(Duration::from_secs(30));
    let initial_parent = getppid();
    let mut parent_monitor = interval(Duration::from_millis(500));
    let mut terminal_child_failure = None;

    eprintln!(
        "switchd-rs {} stage={} listen={}",
        BACKEND_VERSION,
        MigrationStage::MessageArchiveCutover.as_str(),
        socket_path.display()
    );

    loop {
        let scheduled_restart = restart_at;
        tokio::select! {
            accepted = listener.accept() => {
                let (stream, _) = accepted.map_err(io_failure)?;
                let connection_state = state.clone();
                tasks.spawn(async move {
                    if let Err(error) = serve_connection(stream, connection_state).await
                        && !benign_connection_error_message(&error.to_string())
                    {
                        eprintln!("switchd-rs connection: {error}");
                    }
                });
            }
            _ = terminate.recv() => break,
            _ = interrupt.recv() => break,
            _ = broken_pipe.recv() => {},
            _ = eap_exchange_cleanup.tick() => state.eap_exchanges.prune(),
            _ = parent_monitor.tick() => {
                let current_parent = getppid();
                if parent_process_changed(initial_parent, current_parent) {
                    break;
                }
            }
            _ = child_monitor.tick(), if go_backend.is_some() => {
                let backend = go_backend.as_mut().expect("guarded by select condition");
                if let Some(status) = backend.try_wait()? {
                    let uptime = backend_started_at.take().map_or(Duration::ZERO, |started| started.elapsed());
                    let finished = go_backend.take().expect("backend exists");
                    finished.terminate().await?;
                    reader_reservation::confirmed_go_exit();
                    let store = state.multistandby_store.clone();
                    archive_io::run(move || store.recover()).await.map_err(|error| BackendFailure::Io(error.to_string()))?;
                    match schedule_restart(&mut restart_tracker, uptime, &availability) {
                        RestartDecision::RetryAfter { attempt, delay } => {
                            eprintln!(
                                "switchd-rs: Go backend exited ({status}); restart attempt {attempt} in {}ms",
                                delay.as_millis()
                            );
                            restart_at = Some(Instant::now() + delay);
                        }
                        RestartDecision::Exhausted => {
                            terminal_child_failure = Some(format!(
                                "{status}; restart budget exhausted after {} rapid failures",
                                restart_tracker.attempts()
                            ));
                            break;
                        }
                    }
                } else if let Some(started) = backend_started_at
                    && restart_tracker.observe_healthy(started.elapsed())
                {
                    availability.reset_restart_attempts();
                    eprintln!("switchd-rs: Go backend stable; restart budget reset");
                } else if socket_is_live(&state.go_socket_path)
                    && availability.snapshot().phase != BackendPhase::Ready
                {
                    availability.mark_ready();
                    eprintln!(
                        "switchd-rs: Go backend generation {} ready",
                        availability.snapshot().generation
                    );
                }
            }
            _ = async move {
                if let Some(deadline) = scheduled_restart {
                    sleep_until(deadline).await;
                } else {
                    std::future::pending::<()>().await;
                }
            } => {
                restart_at = None;
                match GoBackendProcess::spawn(&socket_path, &internal_token) {
                    Ok(backend) => {
                        go_backend = Some(backend);
                        backend_started_at = Some(Instant::now());
                        availability.child_started();
                        eprintln!(
                            "switchd-rs: Go backend restarted on attempt {}",
                            restart_tracker.attempts()
                        );
                    }
                    Err(error) => match schedule_restart(
                        &mut restart_tracker,
                        Duration::ZERO,
                        &availability,
                    ) {
                        RestartDecision::RetryAfter { attempt, delay } => {
                            eprintln!(
                                "switchd-rs: Go backend restart failed ({error}); attempt {attempt} in {}ms",
                                delay.as_millis()
                            );
                            restart_at = Some(Instant::now() + delay);
                        }
                        RestartDecision::Exhausted => {
                            terminal_child_failure = Some(format!(
                                "restart failed: {error}; restart budget exhausted after {} rapid failures",
                                restart_tracker.attempts()
                            ));
                            break;
                        }
                    },
                }
            }
            completed = tasks.join_next(), if !tasks.is_empty() => {
                if let Some(Err(error)) = completed {
                    eprintln!("switchd-rs task: {error}");
                }
            }
        }
    }

    tasks.abort_all();
    while tasks.join_next().await.is_some() {}
    if let Some(backend) = go_backend {
        // Go's bounded teardown releases Rust-owned EAP exchanges. Keep only
        // the authenticated cleanup route alive while waiting for the child;
        // do not admit new calls, SIM writes or forwarded requests.
        let terminating = backend.terminate();
        tokio::pin!(terminating);
        let result = loop {
            tokio::select! {
                biased;
                result = &mut terminating => break result,
                accepted = listener.accept(), if tasks.len() < 16 => {
                    let (stream, _) = accepted.map_err(io_failure)?;
                    tasks.spawn(serve_shutdown_connection(
                        stream, state.clone(),
                    ));
                }
                _ = tasks.join_next(), if !tasks.is_empty() => {}
            }
        };
        tasks.abort_all();
        while tasks.join_next().await.is_some() {}
        result?;
    }
    if let Some(status) = terminal_child_failure {
        return Err(BackendFailure::GoBackendExited(status));
    }
    Ok(())
}

fn shutdown_rejection<B>(
    request: &Request<B>,
    token: Option<&str>,
) -> Option<Response<Full<Bytes>>> {
    if token.is_none() || !token_matches(request, token) {
        return Some(json_response(
            StatusCode::UNAUTHORIZED,
            &ErrorResponse {
                error: "需要 Switch App 本机凭证",
                code: "authentication_required",
                retryable: false,
            },
        ));
    }
    if request.method() == Method::POST
        && request.uri().path() == "/api/v2/vowifi/eap-exchange/release"
    {
        return None;
    }
    Some(json_response(
        StatusCode::SERVICE_UNAVAILABLE,
        &ErrorResponse {
            error: "Switch 正在退出，未提交新操作",
            code: "backend_stopping",
            retryable: false,
        },
    ))
}

async fn serve_shutdown_connection(stream: UnixStream, state: ServerState) {
    let _ = hyper::server::conn::http1::Builder::new()
        .keep_alive(false)
        .serve_connection(
            TokioIo::new(stream),
            service_fn(move |request| {
                let internal_cleanup =
                    multistandby_store::shutdown_write_allowed(request.uri().path())
                        || request.uri().path()
                            == "/api/internal/multistandby/reader-reservation/release";
                let internal_allowed = internal_cleanup
                    && multistandby_store::internal_authenticated(&request, &state.internal_token);
                let rejection = shutdown_rejection(&request, state.app_token.as_deref());
                let state = state.clone();
                async move {
                    if internal_allowed {
                        return Ok::<_, Infallible>(
                            if request.uri().path().starts_with(reader_reservation::PREFIX) {
                                reader_reservation::handle(request).await
                            } else {
                                multistandby_store::handle(request, state.multistandby_store, true)
                                    .await
                            },
                        );
                    }
                    Ok::<_, Infallible>(match rejection {
                        Some(response) => response,
                        None => eap_identity::handle(request, &state.eap_exchanges).await,
                    })
                }
            }),
        )
        .await;
}

fn parent_process_changed(initial: Pid, current: Pid) -> bool {
    current.as_raw() <= 1 || current != initial
}

fn benign_connection_error_message(message: &str) -> bool {
    matches!(
        message,
        "error shutting down connection" | "error writing a body to connection"
    )
}

async fn serve_connection(stream: UnixStream, state: ServerState) -> Result<(), hyper::Error> {
    hyper::server::conn::http1::Builder::new()
        .keep_alive(false)
        .serve_connection(
            TokioIo::new(stream),
            service_fn(move |request| handle_request(request, state.clone())),
        )
        .await
}

async fn handle_request(
    request: Request<Incoming>,
    state: ServerState,
) -> Result<Response<Full<Bytes>>, Infallible> {
    if request
        .uri()
        .path()
        .starts_with(multistandby_store::INTERNAL_PREFIX)
        || request.uri().path().starts_with(reader_reservation::PREFIX)
    {
        if !multistandby_store::internal_authenticated(&request, &state.internal_token) {
            return Ok(json_response(
                StatusCode::UNAUTHORIZED,
                &serde_json::json!({"code":"internal_authentication_required","error":"需要后端内部凭证"}),
            ));
        }
        if request.uri().path().starts_with(reader_reservation::PREFIX) {
            return Ok(reader_reservation::handle(request).await);
        }
        return Ok(
            multistandby_store::handle(request, state.multistandby_store.clone(), true).await,
        );
    }
    if !token_matches(&request, state.app_token.as_deref()) {
        return Ok(json_response(
            StatusCode::UNAUTHORIZED,
            &ErrorResponse {
                error: "需要 Switch App 本机凭证",
                code: "authentication_required",
                retryable: false,
            },
        ));
    }

    if multistandby_store::is_public_route(request.method(), request.uri().path()) {
        return Ok(
            multistandby_store::handle(request, state.multistandby_store.clone(), false).await,
        );
    }
    let declared_owner = ownership::route_owner(request.method().as_str(), request.uri().path());
    let response = match (request.method(), request.uri().path()) {
        (&Method::GET, "/api/v2/runtime") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            let runtime = state.availability.snapshot();
            let readiness = ownership::readiness();
            json_response(
                StatusCode::OK,
                &RuntimeResponse {
                    ok: true,
                    backend: "rust",
                    version: BACKEND_VERSION,
                    migration_stage: MigrationStage::MessageArchiveCutover.as_str(),
                    hardware_ownership: false,
                    hardware_owner: "go",
                    go_backend_connected: runtime.phase == BackendPhase::Ready
                        && socket_is_live(&state.go_socket_path),
                    go_backend_restarting: runtime.phase == BackendPhase::Restarting,
                    go_backend_phase: runtime.phase.as_str(),
                    go_backend_generation: runtime.generation,
                    go_restart_attempts: runtime.restart_attempts,
                    proxy_contract: PROXY_CONTRACT,
                    sim_protocol_contract: SIM_PROTOCOL_CONTRACT,
                    ike_wire_contract: IKE_WIRE_CONTRACT,
                    ims_protocol_contract: IMS_PROTOCOL_CONTRACT,
                    sms_protocol_contract: SMS_PROTOCOL_CONTRACT,
                    call_protocol_contract: CALL_PROTOCOL_CONTRACT,
                    store_contract: STORE_CONTRACT,
                    snapshot_schema_version: 2,
                    ownership_schema_version: ownership::OWNERSHIP_SCHEMA_VERSION,
                    default_runtime: "rust-gateway",
                    cutover_ready: readiness.ready_to_remove_go,
                },
            )
        }
        (&Method::GET, "/api/v2/migration-readiness") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            json_response(StatusCode::OK, &ownership::readiness())
        }
        (&Method::POST, "/api/v2/carrier/resolve") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(carrier::handle(request).await);
        }
        (&Method::POST, "/api/v2/call/dtmf") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(call_codec::handle(request).await);
        }
        (&Method::POST, "/api/v2/ims/validate-register-response") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(ims_codec::handle(request).await);
        }
        (&Method::POST, "/api/v2/reader/authenticate") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(reader_auth::handle(request).await);
        }
        (&Method::POST, "/api/v2/reader/eap-aka") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(reader_eap_aka::handle(request, &state.eap_exchanges).await);
        }
        (&Method::POST, "/api/v2/vowifi/eap-identity")
        | (&Method::POST, "/api/v2/vowifi/eap-exchange/release") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(eap_identity::handle(request, &state.eap_exchanges).await);
        }
        (&Method::POST, "/api/v2/reader/identity") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(reader_identity::handle(request).await);
        }
        (&Method::GET, "/api/reader/list") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(reader_inventory::handle(&state).await);
        }
        (&Method::POST, "/api/v2/vowifi/proxy-policy") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(proxy_policy::handle(request).await);
        }
        (&Method::POST, "/api/v2/vowifi/proxy-probe") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(proxy_probe::handle(request).await);
        }
        (&Method::POST, "/api/v2/vowifi/select-transport") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(transport_selection::handle(request).await);
        }
        (&Method::POST, "/api/v2/vowifi/session-policy") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(vowifi_session_policy::handle(request).await);
        }
        (&Method::POST, "/api/v2/vowifi/native-session-plan") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(native_vowifi_session::handle(
                request,
                &state.go_socket_path,
                state.app_token.as_deref(),
            )
            .await);
        }
        (
            &Method::POST,
            "/api/v2/sms/encode-rpdu"
            | "/api/v2/sms/decode-rpdu"
            | "/api/v2/sms/prepare-tpdu"
            | "/api/v2/sms/decode-incoming-tpdu",
        ) => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(sms_codec::handle(request).await);
        }
        (&Method::GET, "/api/calls")
        | (&Method::POST, "/api/calls/clear")
        | (&Method::POST, "/api/calls/delete") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(call_archive::handle(request).await);
        }
        (&Method::GET, "/api/v2/sms-archive")
        | (&Method::POST, "/api/v2/sms-archive/clear")
        | (&Method::POST, "/api/v2/sms-archive/delete")
        | (&Method::POST, "/api/v2/sms-archive/delete-sender") => {
            debug_assert_eq!(declared_owner, ownership::RouteOwner::Rust);
            return Ok(message_archive::handle(request).await);
        }
        _ => match recovery_admission(request.method(), effective_backend_phase(&state)) {
            RecoveryAdmission::RejectMutation => json_response(
                StatusCode::SERVICE_UNAVAILABLE,
                &ErrorResponse {
                    error: "Go 后端正在恢复；为避免重复执行，写入请求未自动重放",
                    code: "go_backend_restarting_write_not_replayed",
                    retryable: false,
                },
            ),
            RecoveryAdmission::WaitForRead
                if state
                    .availability
                    .wait_until_ready(READ_RECOVERY_WAIT)
                    .await =>
            {
                return handle_ready_request(request, &state).await;
            }
            RecoveryAdmission::WaitForRead => json_response(
                StatusCode::SERVICE_UNAVAILABLE,
                &ErrorResponse {
                    error: "Go 后端在读取等待窗口内未恢复",
                    code: "go_backend_recovery_timeout",
                    retryable: true,
                },
            ),
            RecoveryAdmission::Forward => return handle_ready_request(request, &state).await,
        },
    };
    Ok(response)
}

fn effective_backend_phase(state: &ServerState) -> BackendPhase {
    let phase = state.availability.snapshot().phase;
    if phase == BackendPhase::Ready && !socket_is_live(&state.go_socket_path) {
        state.availability.mark_starting();
        BackendPhase::Starting
    } else {
        phase
    }
}

fn recovery_admission(method: &Method, phase: BackendPhase) -> RecoveryAdmission {
    if phase == BackendPhase::Ready {
        RecoveryAdmission::Forward
    } else if method == Method::GET {
        RecoveryAdmission::WaitForRead
    } else {
        RecoveryAdmission::RejectMutation
    }
}

async fn handle_ready_request(
    request: Request<Incoming>,
    state: &ServerState,
) -> Result<Response<Full<Bytes>>, Infallible> {
    // Losing a mutation's response does not prove it was not applied. Only
    // reads may be advertised as safe to retry at the transport boundary.
    let retryable_transport = matches!(*request.method(), Method::GET | Method::HEAD);
    let response = match (request.method(), request.uri().path()) {
        (&Method::GET, "/api/v2/device-snapshot") => {
            debug_assert_eq!(
                ownership::route_owner(request.method().as_str(), request.uri().path()),
                ownership::RouteOwner::Rust
            );
            let snapshot = snapshot::capture(state).await;
            let status = if snapshot.ok {
                StatusCode::OK
            } else {
                StatusCode::SERVICE_UNAVAILABLE
            };
            json_response(status, &snapshot)
        }
        _ => match upstream::forward(request, &state.go_socket_path).await {
            Ok(response) => response,
            Err(error) => json_response(
                StatusCode::SERVICE_UNAVAILABLE,
                &ErrorResponse {
                    error: &error.to_string(),
                    code: "go_backend_unavailable",
                    retryable: retryable_transport,
                },
            ),
        },
    };
    Ok(response)
}

fn schedule_restart(
    tracker: &mut RestartTracker,
    previous_uptime: Duration,
    availability: &BackendAvailability,
) -> RestartDecision {
    let decision = tracker.record_failure(previous_uptime);
    availability.mark_restarting(u32::try_from(tracker.attempts()).unwrap_or(u32::MAX));
    decision
}

fn socket_is_live(path: &Path) -> bool {
    fs::symlink_metadata(path)
        .map(|metadata| metadata.file_type().is_socket())
        .unwrap_or(false)
}

fn json_response<T: serde::Serialize>(status: StatusCode, value: &T) -> Response<Full<Bytes>> {
    let body = serde_json::to_vec(value).unwrap_or_else(|_| {
        br#"{"error":"response serialization failed","code":"internal_error","retryable":false}"#
            .to_vec()
    });
    let mut response = Response::new(Full::new(Bytes::from(body)));
    *response.status_mut() = status;
    let headers = response.headers_mut();
    headers.insert(
        hyper::header::CONTENT_TYPE,
        HeaderValue::from_static("application/json; charset=utf-8"),
    );
    headers.insert(
        HeaderName::from_static("x-content-type-options"),
        HeaderValue::from_static("nosniff"),
    );
    headers.insert(
        HeaderName::from_static("x-frame-options"),
        HeaderValue::from_static("DENY"),
    );
    headers.insert(
        HeaderName::from_static("referrer-policy"),
        HeaderValue::from_static("no-referrer"),
    );
    response
}

fn normalized_app_token() -> Option<String> {
    env::var(APP_TOKEN_ENVIRONMENT)
        .ok()
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
}

fn token_matches<B>(request: &Request<B>, expected: Option<&str>) -> bool {
    let Some(expected) = expected else {
        return true;
    };
    let Some(provided) = request.headers().get(APP_TOKEN_HEADER) else {
        return false;
    };
    let Ok(provided) = provided.to_str() else {
        return false;
    };
    provided.len() == expected.len() && provided.as_bytes().ct_eq(expected.as_bytes()).into()
}

fn parse_listen_path(
    arguments: impl IntoIterator<Item = String>,
) -> Result<PathBuf, BackendFailure> {
    let mut arguments = arguments.into_iter();
    while let Some(argument) = arguments.next() {
        if argument == "-listen" || argument == "--listen" {
            let value = arguments
                .next()
                .ok_or(BackendFailure::InvalidListenAddress)?;
            return value
                .strip_prefix("unix:")
                .filter(|path| !path.trim().is_empty())
                .map(PathBuf::from)
                .ok_or(BackendFailure::InvalidListenAddress);
        }
    }
    Err(BackendFailure::InvalidListenAddress)
}

pub(crate) fn prepare_socket_path(path: &Path) -> Result<(), BackendFailure> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_socket() => {
            fs::remove_file(path).map_err(io_failure)
        }
        Ok(_) => Err(BackendFailure::ExistingPathIsNotSocket),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(io_failure(error)),
    }
}

pub(crate) fn io_failure(error: std::io::Error) -> BackendFailure {
    BackendFailure::Io(error.to_string())
}

#[cfg(test)]
mod tests {
    use super::{
        RecoveryAdmission, benign_connection_error_message, parent_process_changed,
        parse_listen_path, prepare_socket_path, recovery_admission, shutdown_rejection,
    };
    use crate::availability::BackendPhase;
    use hyper::Method;
    use nix::unistd::Pid;
    use std::fs;
    use std::os::unix::net::UnixListener as StdUnixListener;
    use std::path::PathBuf;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn test_directory(_name: &str) -> PathBuf {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        // Darwin's sockaddr_un has a short path limit. Keep the fixture under
        // TMPDIR so callers can contain every test artifact in their workspace.
        // Keep the leaf short for Darwin sockaddr_un.
        let path = std::env::temp_dir().join(format!(
            "sw-{}-{}",
            std::process::id(),
            nonce % 1_000_000_000
        ));
        fs::create_dir(&path).expect("create test directory");
        path
    }

    #[test]
    fn parses_go_compatible_listen_argument() {
        let path = parse_listen_path([
            "-listen".to_owned(),
            "unix:/tmp/proletarius.sock".to_owned(),
        ])
        .expect("listen argument");
        assert_eq!(path.to_string_lossy(), "/tmp/proletarius.sock");
    }

    #[test]
    fn suppresses_only_known_client_disconnect_noise() {
        assert!(benign_connection_error_message(
            "error shutting down connection"
        ));
        assert!(benign_connection_error_message(
            "error writing a body to connection"
        ));
        assert!(!benign_connection_error_message(
            "invalid HTTP request received"
        ));
    }

    #[test]
    fn exits_when_parent_is_replaced_or_reparented_to_launchd() {
        let app = Pid::from_raw(42);
        assert!(!parent_process_changed(app, app));
        assert!(parent_process_changed(app, Pid::from_raw(43)));
        assert!(parent_process_changed(app, Pid::from_raw(1)));
    }

    #[test]
    fn rejects_tcp_or_missing_listen_arguments() {
        assert!(parse_listen_path(["-listen".to_owned(), "127.0.0.1:8080".to_owned()]).is_err());
        assert!(parse_listen_path(Vec::<String>::new()).is_err());
    }

    #[test]
    fn recovery_waits_only_for_reads_and_never_replays_mutations() {
        assert_eq!(
            recovery_admission(&Method::GET, BackendPhase::Restarting),
            RecoveryAdmission::WaitForRead
        );
        for method in [Method::POST, Method::PUT, Method::PATCH, Method::DELETE] {
            assert_eq!(
                recovery_admission(&method, BackendPhase::Restarting),
                RecoveryAdmission::RejectMutation
            );
        }
        assert_eq!(
            recovery_admission(&Method::POST, BackendPhase::Ready),
            RecoveryAdmission::Forward
        );
    }

    #[test]
    fn shutdown_allows_only_authenticated_exchange_release() {
        for (method, path, token, expected) in [
            (
                Method::POST,
                "/api/v2/vowifi/eap-exchange/release",
                None,
                Some(hyper::StatusCode::UNAUTHORIZED),
            ),
            (
                Method::POST,
                "/api/v2/vowifi/eap-exchange/release",
                Some("wrong"),
                Some(hyper::StatusCode::UNAUTHORIZED),
            ),
            (
                Method::POST,
                "/api/v2/vowifi/eap-exchange/release",
                Some("fixture"),
                None,
            ),
            (
                Method::GET,
                "/api/v2/vowifi/eap-exchange/release",
                Some("fixture"),
                Some(hyper::StatusCode::SERVICE_UNAVAILABLE),
            ),
            (
                Method::POST,
                "/api/sms/send",
                Some("fixture"),
                Some(hyper::StatusCode::SERVICE_UNAVAILABLE),
            ),
        ] {
            let mut request = hyper::Request::builder().method(method).uri(path);
            if let Some(token) = token {
                request = request.header("x-proletarius-app-token", token);
            }
            let request = request.body(()).unwrap();
            assert_eq!(
                shutdown_rejection(&request, Some("fixture")).map(|response| response.status()),
                expected
            );
        }
    }

    #[tokio::test]
    async fn shutdown_listener_serves_authenticated_cleanup_over_socket() {
        use bytes::Bytes;
        use http_body_util::{BodyExt, Full};
        use hyper_util::rt::TokioIo;
        use std::sync::Arc;
        use tokio::net::UnixStream;
        for authenticated in [false, true] {
            let (client, server) = UnixStream::pair().unwrap();
            let server = tokio::spawn(super::serve_shutdown_connection(
                server,
                super::ServerState {
                    app_token: Some("fixture".to_owned()),
                    internal_token: "internal-fixture".to_owned(),
                    multistandby_store: super::multistandby_store::Store::open(
                        &std::env::temp_dir().join("switch-shutdown-test-unused"),
                    )
                    .unwrap(),
                    go_socket_path: PathBuf::new(),
                    availability: super::BackendAvailability::new(),
                    reader_discovery: Arc::new(std::sync::Mutex::new(
                        super::ReaderDiscovery::default(),
                    )),
                    snapshot_instance_id: "test".to_owned(),
                    snapshot_revision: Arc::new(super::AtomicU64::new(0)),
                    eap_exchanges: Arc::new(super::eap_identity::EapExchangeStore::default()),
                },
            ));
            let (mut sender, connection) =
                hyper::client::conn::http1::handshake(TokioIo::new(client))
                    .await
                    .unwrap();
            let connection = tokio::spawn(connection);
            let mut request = hyper::Request::builder()
                .method(hyper::Method::POST)
                .uri("/api/v2/vowifi/eap-exchange/release")
                .header("host", "localhost");
            if authenticated {
                request = request.header("x-proletarius-app-token", "fixture");
            }
            let body = format!("{{\"exchange_id\":\"{}\"}}", "0".repeat(64));
            let response = sender
                .send_request(request.body(Full::new(Bytes::from(body))).unwrap())
                .await
                .unwrap();
            assert_eq!(
                response.status(),
                if authenticated {
                    hyper::StatusCode::OK
                } else {
                    hyper::StatusCode::UNAUTHORIZED
                }
            );
            response.into_body().collect().await.unwrap();
            drop(sender);
            server.await.unwrap();
            let _ = connection.await;
        }
    }

    #[test]
    fn socket_preparation_preserves_regular_files() {
        let directory = test_directory("regular-file");
        let path = directory.join("important-data");
        fs::write(&path, b"keep").expect("write fixture");
        assert!(prepare_socket_path(&path).is_err());
        assert_eq!(fs::read(&path).expect("fixture remains"), b"keep");
        fs::remove_dir_all(directory).expect("cleanup");
    }

    #[test]
    fn socket_preparation_rejects_symlinks() {
        use std::os::unix::fs::symlink;

        let directory = test_directory("symlink");
        let target = directory.join("target");
        let link = directory.join("listen.sock");
        fs::write(&target, b"keep").expect("write target");
        symlink(&target, &link).expect("create symlink");
        assert!(prepare_socket_path(&link).is_err());
        assert!(fs::symlink_metadata(&link).is_ok());
        fs::remove_dir_all(directory).expect("cleanup");
    }

    #[test]
    fn socket_preparation_removes_only_stale_sockets() {
        let directory = test_directory("socket");
        let path = directory.join("listen.sock");
        let listener = StdUnixListener::bind(&path).expect("bind fixture socket");
        drop(listener);
        prepare_socket_path(&path).expect("remove stale socket");
        assert!(!path.exists());
        fs::remove_dir_all(directory).expect("cleanup");
    }
}
