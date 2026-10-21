//! Resolves and selects the ePDG transport inside the Rust control plane.
//! The compatibility process receives only the selected mode and destination;
//! it no longer duplicates DNS, route, fake-IP, or SOCKS5 policy decisions.

use std::collections::HashSet;
use std::future::Future;
use std::net::{IpAddr, SocketAddr};
use std::time::Duration;

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use proletarius_vowifi::{
    ProxyEvidence, ProxyMode, ProxyPolicyError, RouteEvidence, select_proxy_mode,
};
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::net::lookup_host;
use tokio::process::Command;
use tokio::task::JoinSet;
use tokio::time::{Instant, timeout};

const BODY_LIMIT: usize = 8_192;
const LOOKUP_TIMEOUT: Duration = Duration::from_secs(3);
const ROUTE_TIMEOUT: Duration = Duration::from_secs(3);
// Explicit SOCKS probes keep their six-second budget. This outer bound leaves
// a small Unix-socket/JSON margin below Go's seven-second startup context;
// automatic system discovery has its own shorter budget in system_proxy.
const TRANSPORT_SELECTION_BUDGET: Duration = Duration::from_millis(6_800);

#[derive(Deserialize)]
struct SelectionInput {
    #[serde(default)]
    proxy: String,
    epdg: String,
}

/// Immutable transport decision consumed by the future native session owner.
/// `proxy` is present only after a real SOCKS handshake resolved the endpoint;
/// transparent/direct modes never need to retain it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SelectedTransport {
    pub mode: ProxyMode,
    pub proxy: Option<SocketAddr>,
    pub proxy_error: Option<&'static str>,
    pub proxy_source: Option<&'static str>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SessionSelectionError {
    InvalidEpdg,
    TimedOut,
    Policy {
        error: ProxyPolicyError,
        proxy_error: Option<&'static str>,
        proxy_source: Option<&'static str>,
    },
    MissingVerifiedProxy,
}

struct TransportDecision {
    selection: Result<ProxyMode, ProxyPolicyError>,
    proxy: Option<SocketAddr>,
    proxy_error: Option<&'static str>,
    proxy_source: Option<&'static str>,
}

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let body = match timeout(
        Duration::from_secs(2),
        Limited::new(request.into_body(), BODY_LIMIT).collect(),
    )
    .await
    {
        Ok(Ok(value)) => value.to_bytes(),
        Ok(Err(_)) => return failure(StatusCode::BAD_REQUEST, "invalid_selection_request"),
        Err(_) => return failure(StatusCode::REQUEST_TIMEOUT, "request_timeout"),
    };
    let Ok(input) = serde_json::from_slice::<SelectionInput>(&body) else {
        return failure(StatusCode::BAD_REQUEST, "invalid_selection_request");
    };
    if !valid_hostname(&input.epdg) {
        return failure(StatusCode::UNPROCESSABLE_ENTITY, "invalid_epdg_hostname");
    }

    match select_for_session(input.proxy.trim(), input.epdg.trim()).await {
        Ok(selected) => {
            let proxy_binding_valid =
                matches!(selected.mode, ProxyMode::Socks5UdpDomain) == selected.proxy.is_some();
            if !proxy_binding_valid {
                return failure(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    "transport_binding_invalid",
                );
            }
            selection_response(
                Ok(selected.mode),
                selected.proxy,
                selected.proxy_error,
                selected.proxy_source,
            )
        }
        Err(SessionSelectionError::Policy {
            error,
            proxy_error,
            proxy_source,
        }) => selection_response(Err(error), None, proxy_error, proxy_source),
        Err(SessionSelectionError::InvalidEpdg) => {
            failure(StatusCode::UNPROCESSABLE_ENTITY, "invalid_epdg_hostname")
        }
        Err(SessionSelectionError::TimedOut) => crate::json_response(
            StatusCode::OK,
            &json!({"mode":"rejected","error_code":"transport_selection_timeout"}),
        ),
        Err(SessionSelectionError::MissingVerifiedProxy) => selection_response(
            Err(ProxyPolicyError::Socks5UdpUnavailable),
            None,
            Some("proxy_address_unavailable"),
            None,
        ),
    }
}

#[cfg(test)]
async fn select_transport(proxy: &str, epdg: &str) -> Response<Full<Bytes>> {
    let decision = decide_transport_with_source(proxy, epdg, None).await;
    selection_response(
        decision.selection,
        decision.proxy,
        decision.proxy_error,
        decision.proxy_source,
    )
}

pub(crate) async fn select_for_session(
    requested_proxy: &str,
    epdg: &str,
) -> Result<SelectedTransport, SessionSelectionError> {
    let deadline = Instant::now() + TRANSPORT_SELECTION_BUDGET;
    within_selection_budget(
        remaining_selection_budget(deadline),
        select_for_session_unbounded(requested_proxy, epdg, deadline),
    )
    .await
}

fn remaining_selection_budget(deadline: Instant) -> Duration {
    deadline.saturating_duration_since(Instant::now())
}

async fn within_selection_budget<F>(
    budget: Duration,
    work: F,
) -> Result<SelectedTransport, SessionSelectionError>
where
    F: Future<Output = Result<SelectedTransport, SessionSelectionError>>,
{
    timeout(budget, work)
        .await
        .unwrap_or(Err(SessionSelectionError::TimedOut))
}

async fn select_for_session_unbounded(
    requested_proxy: &str,
    epdg: &str,
    selection_deadline: Instant,
) -> Result<SelectedTransport, SessionSelectionError> {
    if !valid_hostname(epdg) {
        return Err(SessionSelectionError::InvalidEpdg);
    }
    let decision = decide_transport(requested_proxy.trim(), epdg.trim(), selection_deadline).await;
    let mode = decision
        .selection
        .map_err(|error| SessionSelectionError::Policy {
            error,
            proxy_error: decision.proxy_error,
            proxy_source: decision.proxy_source,
        })?;
    let proxy = match mode {
        ProxyMode::Socks5UdpDomain => Some(
            decision
                .proxy
                .ok_or(SessionSelectionError::MissingVerifiedProxy)?,
        ),
        _ => None,
    };
    Ok(SelectedTransport {
        mode,
        proxy,
        proxy_error: decision.proxy_error,
        proxy_source: decision.proxy_source,
    })
}

async fn decide_transport(
    requested_proxy: &str,
    epdg: &str,
    selection_deadline: Instant,
) -> TransportDecision {
    let discovery = if requested_proxy.is_empty() {
        Some(crate::system_proxy::discover().await)
    } else {
        None
    };
    let discovered_proxy = discovery.as_ref().and_then(|result| result.proxy.as_ref());
    let effective_proxy = discovered_proxy
        .as_ref()
        .map_or(requested_proxy, |proxy| proxy.endpoint.as_str());
    let proxy_source = if requested_proxy.is_empty() {
        discovered_proxy.map(|proxy| proxy.source)
    } else {
        Some("explicit")
    };
    let diagnostic = discovery.as_ref().and_then(|result| result.diagnostic);
    attach_discovery_diagnostic(
        decide_transport_with_source_and_probe_budget(
            effective_proxy,
            epdg,
            proxy_source,
            probe_budget_for_selection(requested_proxy, selection_deadline),
        )
        .await,
        diagnostic,
    )
}

fn probe_budget_for_selection(requested_proxy: &str, selection_deadline: Instant) -> Duration {
    if requested_proxy.is_empty() {
        remaining_selection_budget(selection_deadline).min(Duration::from_secs(6))
    } else {
        // Never shorten explicit SOCKS5 UDP verification merely because the
        // automatic System Proxy path is also bounded.
        Duration::from_secs(6)
    }
}

fn attach_discovery_diagnostic(
    mut decision: TransportDecision,
    diagnostic: Option<&'static str>,
) -> TransportDecision {
    if decision.proxy_error.is_none() {
        decision.proxy_error = diagnostic;
    }
    decision
}

#[cfg(test)]
async fn decide_transport_with_source(
    proxy: &str,
    epdg: &str,
    proxy_source: Option<&'static str>,
) -> TransportDecision {
    decide_transport_with_source_and_probe_budget(proxy, epdg, proxy_source, Duration::from_secs(6))
        .await
}

async fn decide_transport_with_source_and_probe_budget(
    proxy: &str,
    epdg: &str,
    proxy_source: Option<&'static str>,
    proxy_budget: Duration,
) -> TransportDecision {
    // A verified SOCKS5 UDP association must retain the ePDG hostname as a
    // proxy-side domain target. Do not start a local ePDG lookup or route
    // inspection until SOCKS is known to be unavailable; either one would
    // leak or preempt the proxy's DNS/policy decision.
    let proxy_probe = if proxy.is_empty() {
        None
    } else {
        Some(probe_proxy(proxy, proxy_budget).await)
    };
    decide_from_proxy_probe(
        proxy_probe,
        proxy_source,
        configured_proxy_is_loopback(proxy),
        || async {
            let addresses = resolve_epdg(epdg).await.unwrap_or_default();
            let routes = collect_routes(&addresses).await;
            (addresses, routes)
        },
    )
    .await
}

async fn decide_from_proxy_probe<F, Fut>(
    proxy_probe: Option<Result<crate::proxy_probe::EndpointProbe, &'static str>>,
    proxy_source: Option<&'static str>,
    configured_proxy_is_loopback: bool,
    resolve_evidence: F,
) -> TransportDecision
where
    F: FnOnce() -> Fut,
    Fut: Future<Output = (Vec<IpAddr>, Vec<RouteEvidence>)>,
{
    let proxy_error = proxy_probe
        .as_ref()
        .and_then(|result| result.as_ref().err().copied());
    let proxy_address = proxy_probe
        .as_ref()
        .and_then(|result| result.as_ref().ok().map(|probe| probe.address));
    if proxy_probe.as_ref().is_some_and(|result| {
        result
            .as_ref()
            .is_ok_and(|probe| probe.result.udp_associate)
    }) {
        return TransportDecision {
            selection: Ok(ProxyMode::Socks5UdpDomain),
            proxy: proxy_address,
            proxy_error,
            proxy_source,
        };
    }

    let (addresses, routes) = resolve_evidence().await;
    // A local TUN can carry fake-IP UDP even if its HTTP/SOCKS port does not
    // support UDP ASSOCIATE. Verify the actual route before selecting fake-IP;
    // a stale fake DNS answer alone is not evidence of a working TUN.
    let verified_fake_route = routes.iter().any(|route| {
        proletarius_vowifi::is_fake_ip(route.address)
            && route
                .interface
                .as_deref()
                .is_some_and(|interface| interface.starts_with("utun"))
    });
    let proxy_is_loopback = verified_fake_route && configured_proxy_is_loopback;
    let evidence = ProxyEvidence {
        proxy_is_loopback,
        epdg_addresses: addresses,
        routes,
        socks5_udp_associate: false,
    };
    TransportDecision {
        selection: select_proxy_mode(&evidence),
        proxy: proxy_address,
        proxy_error,
        proxy_source,
    }
}

fn configured_proxy_is_loopback(proxy: &str) -> bool {
    let endpoint = proxy
        .split_once("://")
        .map(|(_, endpoint)| endpoint)
        .unwrap_or(proxy);
    crate::system_proxy::is_loopback_endpoint(endpoint)
}

async fn resolve_epdg(host: &str) -> Result<Vec<IpAddr>, ()> {
    let resolved = timeout(LOOKUP_TIMEOUT, lookup_host((host, 500)))
        .await
        .map_err(|_| ())?
        .map_err(|_| ())?;
    let mut seen = HashSet::new();
    Ok(resolved
        .map(|address| address.ip())
        .filter(|address| seen.insert(*address))
        .collect())
}

async fn probe_proxy(
    proxy: &str,
    budget: Duration,
) -> Result<crate::proxy_probe::EndpointProbe, &'static str> {
    if proxy.is_empty() {
        return Err("proxy_not_configured");
    }
    crate::proxy_probe::probe_endpoint_with_budget(proxy, budget).await
}

async fn collect_routes(addresses: &[IpAddr]) -> Vec<RouteEvidence> {
    collect_routes_with(addresses, route_interface).await
}

async fn collect_routes_with<F, Fut>(addresses: &[IpAddr], lookup: F) -> Vec<RouteEvidence>
where
    F: Fn(IpAddr) -> Fut + Clone + Send + 'static,
    Fut: Future<Output = Option<String>> + Send + 'static,
{
    let mut tasks = JoinSet::new();
    for (index, address) in addresses.iter().copied().enumerate() {
        let lookup = lookup.clone();
        tasks.spawn(async move { (index, address, lookup(address).await) });
    }
    let mut ordered = vec![None; addresses.len()];
    while let Some(result) = tasks.join_next().await {
        if let Ok((index, address, Some(interface))) = result {
            ordered[index] = Some(RouteEvidence {
                address,
                interface: Some(interface),
            });
        }
    }
    ordered.into_iter().flatten().collect()
}

async fn route_interface(address: IpAddr) -> Option<String> {
    let output = match timeout(
        ROUTE_TIMEOUT,
        Command::new("/sbin/route")
            .args(["-n", "get", &address.to_string()])
            .kill_on_drop(true)
            .output(),
    )
    .await
    {
        Ok(Ok(output)) if output.status.success() => output,
        _ => return None,
    };
    parse_route_interface(&String::from_utf8_lossy(&output.stdout))
}

fn parse_route_interface(output: &str) -> Option<String> {
    output.lines().find_map(|line| {
        let mut fields = line.split_whitespace();
        match (fields.next(), fields.next(), fields.next()) {
            (Some("interface:"), Some(interface), None) if !interface.is_empty() => {
                Some(interface.to_owned())
            }
            _ => None,
        }
    })
}

pub(crate) fn valid_hostname(value: &str) -> bool {
    let value = value.trim().trim_end_matches('.');
    !value.is_empty()
        && value.len() <= 253
        && value.is_ascii()
        && value.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && !label.starts_with('-')
                && !label.ends_with('-')
                && label
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
}

fn selection_response(
    selection: Result<ProxyMode, proletarius_vowifi::ProxyPolicyError>,
    proxy: Option<SocketAddr>,
    proxy_error: Option<&str>,
    proxy_source: Option<&str>,
) -> Response<Full<Bytes>> {
    // A SOCKS endpoint is a capability binding, not general diagnostics. It
    // is emitted only for the one mode that needs it, after `probe_proxy`
    // completed its UDP ASSOCIATE handshake. Returning the resolved socket
    // also ensures the Go compatibility transport does not perform a second
    // DNS resolution after Rust has already verified the endpoint.
    let include_proxy_endpoint = matches!(&selection, Ok(ProxyMode::Socks5UdpDomain));
    let mut value = match selection {
        Ok(ProxyMode::EnhancedFakeIp(address)) => {
            json!({"mode":"enhanced-mode-fake-ip","address":address.to_string()})
        }
        Ok(ProxyMode::EnhancedUtun { address, interface }) => json!({
            "mode":"enhanced-mode-utun",
            "address":address.to_string(),
            "interface":interface,
        }),
        Ok(ProxyMode::Socks5UdpDomain) => json!({"mode":"socks5-udp-domain"}),
        Ok(ProxyMode::Direct(address)) => {
            json!({"mode":"direct","address":address.to_string()})
        }
        Err(error) => json!({
            "mode":"rejected",
            "error_code":match error {
                proletarius_vowifi::ProxyPolicyError::Socks5UdpUnavailable => "socks5_udp_unavailable",
                proletarius_vowifi::ProxyPolicyError::UnverifiedPublicRoute => "unverified_public_route",
            },
        }),
    };
    if let (Some(error), Value::Object(object)) = (proxy_error, &mut value) {
        object.insert("proxy_error".to_owned(), Value::String(error.to_owned()));
    }
    if let (Some(source), Value::Object(object)) = (proxy_source, &mut value) {
        object.insert("proxy_source".to_owned(), Value::String(source.to_owned()));
    }
    if include_proxy_endpoint {
        if let (Some(proxy), Value::Object(object)) = (proxy, &mut value) {
            object.insert(
                "proxy_endpoint".to_owned(),
                Value::String(proxy.to_string()),
            );
        }
    }
    crate::json_response(StatusCode::OK, &value)
}

fn failure(status: StatusCode, code: &str) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"ePDG 传输参数无效","code":code,"retryable":false}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    };
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::{TcpListener, UdpSocket};
    use tokio::sync::Barrier;

    #[test]
    fn validates_epdg_hostname_without_accepting_socket_or_url_syntax() {
        assert!(valid_hostname("epdg.epc.mnc020.mcc234.pub.3gppnetwork.org"));
        assert!(valid_hostname("epdg.example.org."));
        assert!(!valid_hostname("https://epdg.example.org"));
        assert!(!valid_hostname("epdg.example.org:500"));
        assert!(!valid_hostname("-epdg.example.org"));
    }

    #[test]
    fn parses_only_a_single_route_interface_field() {
        assert_eq!(
            parse_route_interface("route to: 87.194.8.8\ninterface: utun8\n"),
            Some("utun8".to_owned())
        );
        assert_eq!(parse_route_interface("interface:\n"), None);
    }

    #[tokio::test]
    async fn route_lookups_run_concurrently_but_keep_dns_order() {
        let addresses = [
            "192.0.2.1".parse().unwrap(),
            "192.0.2.2".parse().unwrap(),
            "192.0.2.3".parse().unwrap(),
        ];
        let barrier = Arc::new(Barrier::new(addresses.len()));
        let routes = timeout(
            Duration::from_secs(1),
            collect_routes_with(&addresses, move |address| {
                let barrier = Arc::clone(&barrier);
                async move {
                    barrier.wait().await;
                    if address == "192.0.2.1".parse::<IpAddr>().unwrap() {
                        tokio::time::sleep(Duration::from_millis(20)).await;
                    }
                    Some("utun8".to_owned())
                }
            }),
        )
        .await
        .expect("route probes should not run serially");
        assert_eq!(
            routes.iter().map(|route| route.address).collect::<Vec<_>>(),
            addresses
        );
    }

    #[test]
    fn loopback_proxy_detection_does_not_depend_on_socks_handshake() {
        assert!(configured_proxy_is_loopback("127.0.0.1:7897"));
        assert!(configured_proxy_is_loopback("http://127.0.0.1:6153"));
        assert!(!configured_proxy_is_loopback("10.10.10.21:7890"));
        assert!(!configured_proxy_is_loopback("127.0.0.1"));
    }

    #[tokio::test]
    async fn verified_socks_does_not_resolve_epdg_locally() {
        let resolved = Arc::new(AtomicBool::new(false));
        let observed = Arc::clone(&resolved);
        let proxy = "127.0.0.1:7897".parse().unwrap();

        let decision = decide_from_proxy_probe(
            Some(Ok(crate::proxy_probe::EndpointProbe {
                address: proxy,
                result: proletarius_vowifi::Socks5Probe {
                    handshake_latency_ms: 1,
                    udp_associate: true,
                },
            })),
            Some("explicit"),
            true,
            move || async move {
                observed.store(true, Ordering::SeqCst);
                (vec!["87.194.8.8".parse().unwrap()], Vec::new())
            },
        )
        .await;

        assert_eq!(decision.selection, Ok(ProxyMode::Socks5UdpDomain));
        assert_eq!(decision.proxy, Some(proxy));
        assert!(!resolved.load(Ordering::SeqCst));
    }

    #[test]
    fn automatic_http_or_pac_diagnostic_is_preserved_without_inventing_socks() {
        let decision = attach_discovery_diagnostic(
            TransportDecision {
                selection: Ok(ProxyMode::Direct("87.194.8.8".parse().unwrap())),
                proxy: None,
                proxy_error: None,
                proxy_source: None,
            },
            Some("system_proxy_pac_unsupported"),
        );
        assert_eq!(decision.proxy_error, Some("system_proxy_pac_unsupported"));
        assert_eq!(decision.proxy_source, None);
        assert_eq!(
            decision.selection,
            Ok(ProxyMode::Direct("87.194.8.8".parse().unwrap()))
        );
    }

    #[test]
    fn only_automatic_discovery_spends_remaining_probe_budget() {
        let deadline = Instant::now() + Duration::from_millis(250);
        assert_eq!(
            probe_budget_for_selection("127.0.0.1:7897", deadline),
            Duration::from_secs(6),
            "an explicit SOCKS endpoint must retain its full UDP verification budget"
        );
        let automatic = probe_budget_for_selection("", deadline);
        assert!(
            automatic <= Duration::from_millis(250),
            "automatic probe must fit the same bounded startup selection"
        );
    }

    #[tokio::test]
    async fn selection_budget_bounds_slow_automatic_discovery_without_shortening_explicit_probe() {
        let result = within_selection_budget(Duration::from_millis(1), async {
            tokio::time::sleep(Duration::from_millis(20)).await;
            Ok(SelectedTransport {
                mode: ProxyMode::Socks5UdpDomain,
                proxy: Some("127.0.0.1:7897".parse().unwrap()),
                proxy_error: None,
                proxy_source: Some("explicit"),
            })
        })
        .await;
        assert_eq!(result, Err(SessionSelectionError::TimedOut));
    }

    #[tokio::test]
    async fn direct_response_retains_nonfatal_proxy_failure() {
        let response = selection_response(
            Ok(ProxyMode::Direct("87.194.8.8".parse().unwrap())),
            None,
            Some("socks5_handshake_failed"),
            None,
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["mode"], "direct");
        assert_eq!(value["proxy_error"], "socks5_handshake_failed");
    }

    #[tokio::test]
    async fn socks_response_exposes_only_the_verified_proxy_endpoint() {
        let endpoint: SocketAddr = "127.0.0.1:7897".parse().unwrap();
        let socks = selection_response(
            Ok(ProxyMode::Socks5UdpDomain),
            Some(endpoint),
            None,
            Some("macos-socks"),
        );
        let body = socks.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["proxy_endpoint"], "127.0.0.1:7897");
        assert_eq!(value["proxy_source"], "macos-socks");

        let fake = selection_response(
            Ok(ProxyMode::EnhancedFakeIp("198.18.0.14".parse().unwrap())),
            Some(endpoint),
            None,
            Some("macos-socks"),
        );
        let body = fake.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert!(value.get("proxy_endpoint").is_none());
    }

    #[tokio::test]
    async fn verified_socks5_uses_domain_when_no_vif_route_exists() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let proxy = listener.local_addr().unwrap();
        let relay = UdpSocket::bind("127.0.0.1:0").await.unwrap();
        let relay_port = relay.local_addr().unwrap().port();
        let server = tokio::spawn(async move {
            for connection in 0..2 {
                let (mut stream, _) = listener.accept().await.unwrap();
                let mut greeting = [0_u8; 3];
                stream.read_exact(&mut greeting).await.unwrap();
                assert_eq!(greeting, proletarius_vowifi::NO_AUTH_GREETING);
                stream.write_all(&[5, 0]).await.unwrap();
                if connection == 1 {
                    let mut associate = [0_u8; 10];
                    stream.read_exact(&mut associate).await.unwrap();
                    assert_eq!(associate, proletarius_vowifi::UDP_ASSOCIATE_REQUEST);
                    let mut answer = vec![5, 0, 0, 1, 127, 0, 0, 1];
                    answer.extend_from_slice(&relay_port.to_be_bytes());
                    stream.write_all(&answer).await.unwrap();
                    let mut closed = [0_u8; 1];
                    let _ = stream.read(&mut closed).await;
                }
            }
        });

        // localhost has no VIF route, so a successful UDP ASSOCIATE must win
        // over the otherwise direct loopback address. Do not use .invalid
        // here: an active Surge Enhanced Mode intentionally maps it to a
        // 198.18/15 fake IP and would make this test machine-dependent.
        let selected = select_for_session(&proxy.to_string(), "localhost")
            .await
            .unwrap();
        assert_eq!(selected.mode, ProxyMode::Socks5UdpDomain);
        assert_eq!(selected.proxy, Some(proxy));
        assert_eq!(selected.proxy_error, None);
        assert_eq!(selected.proxy_source, Some("explicit"));
        server.await.unwrap();
    }

    #[tokio::test]
    async fn real_local_proxy_selects_a_verified_non_direct_transport_when_enabled() {
        if std::env::var("PROLETARIUS_TEST_REAL_PROXY").as_deref() != Ok("1") {
            return;
        }
        let proxy = std::env::var("PROLETARIUS_TEST_PROXY_ENDPOINT")
            .unwrap_or_else(|_| "127.0.0.1:7897".to_owned());
        // With a TUN, .invalid may intentionally become a verified fake-IP;
        // without a TUN, a working SOCKS5 UDP association must retain the
        // domain. Neither path may silently fall back to direct public UDP.
        let response = select_transport(&proxy, "epdg.invalid").await;
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert!(matches!(
            value["mode"].as_str(),
            Some("enhanced-mode-fake-ip" | "enhanced-mode-utun" | "socks5-udp-domain")
        ));
        assert!(value.get("proxy_error").is_none());
    }

    #[tokio::test]
    async fn real_system_proxy_discovery_feeds_non_direct_transport_when_enabled() {
        if std::env::var("PROLETARIUS_TEST_REAL_SURGE").as_deref() != Ok("1") {
            return;
        }
        crate::system_proxy::discover()
            .await
            .proxy
            .expect("running Surge System Proxy must be discovered");
        let selected = select_for_session("", "epdg.invalid").await.unwrap();
        assert!(matches!(
            selected.mode,
            ProxyMode::EnhancedFakeIp(_)
                | ProxyMode::EnhancedUtun { .. }
                | ProxyMode::Socks5UdpDomain
        ));
        assert_eq!(selected.proxy_source, Some("surge-effective-profile"));
    }
}
