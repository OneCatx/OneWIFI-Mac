use std::collections::HashMap;
use std::future::Future;
use std::net::{IpAddr, SocketAddr};
use std::path::Path;
use std::process::Stdio;
use std::time::Duration;

use tokio::io::AsyncReadExt;
use tokio::process::Command;
use tokio::time::timeout;

const SCUTIL_PATH: &str = "/usr/sbin/scutil";
const SURGE_CLI_PATH: &str = "/Applications/Surge.app/Contents/Applications/surge-cli";
const COMMAND_TIMEOUT: Duration = Duration::from_secs(3);
// Surge discovery is necessarily sequential: `scutil`, feature state, then
// the effective profile. 750 ms is too short for a cold local Surge CLI and
// made automatic System Proxy selection flaky. The caller keeps the overall
// transport choice bounded and grants the remaining time to the automatic
// SOCKS probe; an explicit user-entered SOCKS endpoint still gets its full
// independent probe budget.
const SYSTEM_DISCOVERY_BUDGET: Duration = Duration::from_millis(2_400);
const SCUTIL_OUTPUT_LIMIT: usize = 64 * 1024;
const SURGE_PROFILE_OUTPUT_LIMIT: usize = 2 * 1024 * 1024;

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SystemProxy {
    pub(crate) endpoint: String,
    pub(crate) source: &'static str,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub(crate) struct SystemProxyDiscovery {
    pub(crate) proxy: Option<SystemProxy>,
    // This is intentionally a fixed, safe-to-display code. HTTP and PAC
    // system proxy settings cannot be reinterpreted as IKE/ESP UDP transport.
    pub(crate) diagnostic: Option<&'static str>,
}

/// Reads the effective macOS proxy state without guessing a port. A verified
/// Surge System Proxy wins over a generic SOCKS setting only after its enabled
/// System Proxy and effective HTTP listener identify the same loopback port;
/// the SOCKS listener is then taken from that same bounded effective profile.
/// Otherwise an enabled system SOCKS setting is used. HTTP/PAC-only settings
/// are reported but never guessed or treated as SOCKS.
pub(crate) async fn discover() -> SystemProxyDiscovery {
    discover_with_budget(SYSTEM_DISCOVERY_BUDGET, discover_inner()).await
}

async fn discover_with_budget<F>(deadline: Duration, work: F) -> SystemProxyDiscovery
where
    F: Future<Output = SystemProxyDiscovery>,
{
    timeout(deadline, work)
        .await
        .unwrap_or(SystemProxyDiscovery {
            proxy: None,
            diagnostic: Some("system_proxy_discovery_timeout"),
        })
}

async fn discover_inner() -> SystemProxyDiscovery {
    let output = run_bounded(
        SCUTIL_PATH,
        &["--proxy"],
        SCUTIL_OUTPUT_LIMIT,
        COMMAND_TIMEOUT,
    )
    .await;
    let Some(output) = output else {
        return SystemProxyDiscovery::default();
    };
    let fields = parse_proxy_fields(&output);
    let surge = surge_proxy(&fields).await;
    discovery_from_fields(&fields, surge)
}

fn discovery_from_fields(
    fields: &HashMap<String, String>,
    surge_endpoint: Option<String>,
) -> SystemProxyDiscovery {
    // Prefer a positively verified Surge ownership proof. This is not a port
    // heuristic: `surge_proxy` has already checked its CLI feature state and
    // matched the effective HTTP listener to the active macOS System Proxy.
    if let Some(endpoint) = surge_endpoint {
        return SystemProxyDiscovery {
            proxy: Some(SystemProxy {
                endpoint,
                source: "surge-effective-profile",
            }),
            diagnostic: None,
        };
    }
    if let Some(endpoint) = enabled_endpoint(fields, "SOCKS") {
        return SystemProxyDiscovery {
            proxy: Some(SystemProxy {
                endpoint,
                source: "macos-socks",
            }),
            diagnostic: None,
        };
    }
    SystemProxyDiscovery {
        proxy: None,
        diagnostic: unsupported_system_proxy_diagnostic(fields),
    }
}

fn parse_proxy_fields(output: &str) -> HashMap<String, String> {
    output
        .lines()
        .filter_map(|line| {
            let (key, value) = line.split_once(':')?;
            Some((key.trim().to_owned(), value.trim().to_owned()))
        })
        .collect()
}

fn enabled_endpoint(fields: &HashMap<String, String>, kind: &str) -> Option<String> {
    (fields.get(&format!("{kind}Enable"))?.as_str() == "1").then_some(())?;
    let host = fields.get(&format!("{kind}Proxy"))?.trim();
    let port = parse_port(fields.get(&format!("{kind}Port"))?)?;
    format_endpoint(host, port)
}

fn unsupported_system_proxy_diagnostic(fields: &HashMap<String, String>) -> Option<&'static str> {
    if is_enabled(fields, "ProxyAutoConfigEnable") || is_enabled(fields, "ProxyAutoDiscoveryEnable")
    {
        return Some("system_proxy_pac_unsupported");
    }
    (enabled_endpoint(fields, "HTTP").is_some() || enabled_endpoint(fields, "HTTPS").is_some())
        .then_some("system_proxy_http_unsupported")
}

fn is_enabled(fields: &HashMap<String, String>, key: &str) -> bool {
    fields.get(key).is_some_and(|value| value == "1")
}

async fn surge_proxy(fields: &HashMap<String, String>) -> Option<String> {
    let system_http =
        enabled_endpoint(fields, "HTTP").or_else(|| enabled_endpoint(fields, "HTTPS"))?;
    if !is_loopback_endpoint(&system_http) || !Path::new(SURGE_CLI_PATH).is_file() {
        return None;
    }
    let feature = run_bounded(
        SURGE_CLI_PATH,
        &["feature", "get", "system-proxy"],
        SCUTIL_OUTPUT_LIMIT,
        COMMAND_TIMEOUT,
    )
    .await?;
    if !feature_is_on(&feature, "system-proxy") {
        return None;
    }
    let profile = run_bounded(
        SURGE_CLI_PATH,
        &["dump", "profile", "effective"],
        SURGE_PROFILE_OUTPUT_LIMIT,
        COMMAND_TIMEOUT,
    )
    .await?;
    surge_socks_endpoint(&system_http, &profile)
}

fn feature_is_on(output: &str, feature: &str) -> bool {
    output
        .lines()
        .any(|line| line.trim().eq_ignore_ascii_case(&format!("{feature}: on")))
}

fn surge_socks_endpoint(system_http: &str, profile: &str) -> Option<String> {
    if !is_loopback_endpoint(system_http) {
        return None;
    }
    let system_port = endpoint_port(system_http)?;
    let mut listeners = HashMap::new();
    for line in profile.lines() {
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        let key = key.trim().to_ascii_lowercase();
        if matches!(key.as_str(), "http-listen" | "socks5-listen") {
            listeners.insert(key, value.trim());
        }
    }
    let (http_host, http_port) = safe_local_listener(listeners.get("http-listen")?)?;
    let (socks_host, socks_port) = safe_local_listener(listeners.get("socks5-listen")?)?;
    (http_port == system_port && listener_serves_system_endpoint(system_http, http_host))
        .then(|| local_listener_endpoint(socks_host, socks_port))
}

// A matching port alone is not evidence that the effective Surge listener is
// the listener configured in macOS. In particular, 127.0.0.1:P and [::1]:P
// can be different processes. Treat `localhost` conservatively: only an
// identical `localhost` listener is accepted because its chosen family is not
// represented in scutil output.
fn listener_serves_system_endpoint(system_endpoint: &str, listener_host: &str) -> bool {
    let Some((system_host, _)) = split_host_port(system_endpoint) else {
        return false;
    };
    if system_host == "localhost" || listener_host == "localhost" {
        return system_host == listener_host;
    }
    let (Ok(system), Ok(listener)) = (
        system_host.parse::<IpAddr>(),
        listener_host.parse::<IpAddr>(),
    ) else {
        return false;
    };
    match (system, listener) {
        (IpAddr::V4(system), IpAddr::V4(listener)) => {
            listener == system || listener.is_unspecified()
        }
        (IpAddr::V6(system), IpAddr::V6(listener)) => {
            listener == system || listener.is_unspecified()
        }
        _ => false,
    }
}

fn safe_local_listener(value: &str) -> Option<(&str, u16)> {
    let (host, port) = split_host_port(value.trim())?;
    let port = parse_port(port)?;
    let local = matches!(host, "localhost" | "0.0.0.0" | "::")
        || host
            .parse::<IpAddr>()
            .is_ok_and(|address| address.is_loopback());
    local.then_some((host, port))
}

// Preserve a listener's address family. `::` and `0.0.0.0` are bind
// wildcards, not valid peer destinations, so map each to its own loopback
// family. In particular, an IPv6-only Surge SOCKS listener must not be
// rewritten to 127.0.0.1.
fn local_listener_endpoint(host: &str, port: u16) -> String {
    match host {
        "0.0.0.0" => format!("127.0.0.1:{port}"),
        "::" => format!("[::1]:{port}"),
        _ => format_endpoint(host, port)
            .expect("safe local listener host must format as a local endpoint"),
    }
}

pub(crate) fn is_loopback_endpoint(value: &str) -> bool {
    let Some((host, _)) = split_host_port(value.trim()) else {
        return false;
    };
    host == "localhost"
        || host
            .parse::<IpAddr>()
            .is_ok_and(|address| address.is_loopback())
}

fn endpoint_port(value: &str) -> Option<u16> {
    split_host_port(value).and_then(|(_, port)| parse_port(port))
}

fn split_host_port(value: &str) -> Option<(&str, &str)> {
    if let Some(rest) = value.strip_prefix('[') {
        let (host, port) = rest.split_once("]:")?;
        return Some((host, port));
    }
    let (host, port) = value.rsplit_once(':')?;
    (!host.is_empty() && !host.contains(':')).then_some((host, port))
}

fn parse_port(value: &str) -> Option<u16> {
    value.trim().parse::<u16>().ok().filter(|port| *port != 0)
}

fn format_endpoint(host: &str, port: u16) -> Option<String> {
    let host = host.trim();
    if host.is_empty() || host.contains(['\0', '/', '\\']) {
        return None;
    }
    if let Ok(address) = host.parse::<IpAddr>() {
        return Some(SocketAddr::new(address, port).to_string());
    }
    let valid_hostname = host == "localhost"
        || (host.len() <= 253
            && host.is_ascii()
            && host.split('.').all(|label| {
                !label.is_empty()
                    && label.len() <= 63
                    && !label.starts_with('-')
                    && !label.ends_with('-')
                    && label
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
            }));
    valid_hostname.then(|| format!("{host}:{port}"))
}

async fn run_bounded(
    program: &str,
    arguments: &[&str],
    limit: usize,
    deadline: Duration,
) -> Option<String> {
    let mut command = Command::new(program);
    command
        .args(arguments)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .kill_on_drop(true);
    let mut child = command.spawn().ok()?;
    let stdout = child.stdout.take()?;
    let work = async {
        let mut data = Vec::new();
        stdout
            .take(u64::try_from(limit).ok()?.saturating_add(1))
            .read_to_end(&mut data)
            .await
            .ok()?;
        let status = child.wait().await.ok()?;
        (status.success() && data.len() <= limit)
            .then(|| String::from_utf8(data).ok())
            .flatten()
    };
    timeout(deadline, work).await.ok().flatten()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn explicit_system_socks_requires_enabled_valid_endpoint() {
        let mut fields =
            parse_proxy_fields("SOCKSEnable : 1\nSOCKSProxy : 127.0.0.1\nSOCKSPort : 6153\n");
        assert_eq!(
            enabled_endpoint(&fields, "SOCKS").as_deref(),
            Some("127.0.0.1:6153")
        );
        fields.insert("SOCKSPort".into(), "70000".into());
        assert!(enabled_endpoint(&fields, "SOCKS").is_none());
        fields.insert("SOCKSPort".into(), "6153".into());
        fields.insert("SOCKSEnable".into(), "0".into());
        assert!(enabled_endpoint(&fields, "SOCKS").is_none());
    }

    #[test]
    fn only_verified_surge_can_claim_system_socks_ownership() {
        let fields = parse_proxy_fields(
            "SOCKSEnable : 1\nSOCKSProxy : 127.0.0.1\nSOCKSPort : 6153\nHTTPEnable : 1\nHTTPProxy : 127.0.0.1\nHTTPPort : 6152\n",
        );
        let generic = discovery_from_fields(&fields, None);
        assert_eq!(
            generic.proxy.as_ref().map(|value| value.source),
            Some("macos-socks")
        );

        let verified = discovery_from_fields(&fields, Some("[::1]:6153".into()));
        assert_eq!(
            verified.proxy.as_ref().map(|value| value.source),
            Some("surge-effective-profile")
        );
        assert_eq!(
            verified.proxy.as_ref().map(|value| value.endpoint.as_str()),
            Some("[::1]:6153")
        );
    }

    #[test]
    fn surge_listener_requires_matching_local_http_port() {
        let profile = "http-listen = 0.0.0.0:6152\nsocks5-listen = 0.0.0.0:6153\n";
        assert_eq!(
            surge_socks_endpoint("127.0.0.1:6152", profile).as_deref(),
            Some("127.0.0.1:6153")
        );
        assert!(surge_socks_endpoint("127.0.0.1:8080", profile).is_none());
        assert!(surge_socks_endpoint("192.0.2.1:6152", profile).is_none());
        assert!(
            surge_socks_endpoint(
                "127.0.0.1:6152",
                "http-listen = 0.0.0.0:6152\nsocks5-listen = 192.0.2.1:6153\n"
            )
            .is_none()
        );
    }

    #[test]
    fn surge_ipv6_only_socks_listener_keeps_ipv6_endpoint() {
        let profile = "http-listen = 127.0.0.1:6152\nsocks5-listen = [::1]:6153\n";
        assert_eq!(
            surge_socks_endpoint("127.0.0.1:6152", profile).as_deref(),
            Some("[::1]:6153")
        );
        assert_eq!(local_listener_endpoint("::", 6153), "[::1]:6153");
        assert_eq!(local_listener_endpoint("0.0.0.0", 6153), "127.0.0.1:6153");
    }

    #[test]
    fn surge_http_listener_must_serve_the_system_proxy_family() {
        let ipv6_only_http = "http-listen = [::1]:6152\nsocks5-listen = [::1]:6153\n";
        assert!(surge_socks_endpoint("127.0.0.1:6152", ipv6_only_http).is_none());

        let ipv4_wildcard_http = "http-listen = 0.0.0.0:6152\nsocks5-listen = [::1]:6153\n";
        assert_eq!(
            surge_socks_endpoint("127.0.0.1:6152", ipv4_wildcard_http).as_deref(),
            Some("[::1]:6153")
        );
    }

    #[test]
    fn http_and_pac_are_explicitly_not_socks() {
        let http = parse_proxy_fields("HTTPEnable : 1\nHTTPProxy : 127.0.0.1\nHTTPPort : 6152\n");
        assert_eq!(
            unsupported_system_proxy_diagnostic(&http),
            Some("system_proxy_http_unsupported")
        );
        let pac = parse_proxy_fields(
            "ProxyAutoConfigEnable : 1\nProxyAutoConfigURLString : https://proxy.example/pac\n",
        );
        assert_eq!(
            unsupported_system_proxy_diagnostic(&pac),
            Some("system_proxy_pac_unsupported")
        );
    }

    #[tokio::test]
    async fn stalled_discovery_returns_a_bounded_safe_diagnostic() {
        let discovery = discover_with_budget(Duration::from_millis(1), async {
            tokio::time::sleep(Duration::from_millis(20)).await;
            SystemProxyDiscovery {
                proxy: Some(SystemProxy {
                    endpoint: "127.0.0.1:6153".into(),
                    source: "surge-effective-profile",
                }),
                diagnostic: None,
            }
        })
        .await;
        assert_eq!(discovery.proxy, None);
        assert_eq!(discovery.diagnostic, Some("system_proxy_discovery_timeout"));
    }

    #[tokio::test]
    async fn live_surge_discovery_is_bounded_and_usable_when_enabled() {
        if std::env::var("PROLETARIUS_TEST_REAL_SURGE").as_deref() != Ok("1") {
            return;
        }
        let proxy = discover()
            .await
            .proxy
            .expect("running Surge System Proxy must yield its effective SOCKS endpoint");
        assert_eq!(proxy.source, "surge-effective-profile");
        crate::proxy_probe::probe_endpoint(&proxy.endpoint)
            .await
            .expect("discovered Surge SOCKS endpoint must complete a real handshake");
    }
}
