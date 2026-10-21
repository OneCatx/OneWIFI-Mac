//! Immutable, carrier-derived IMS REGISTER policy.
//!
//! This module performs no network or card I/O.  It turns the already
//! validated carrier bundle plus an optional reader override into the exact
//! subset the native TCP/IMS runtime can safely own.  In particular, it does
//! not advertise an IPsec transform that the native installer cannot install.
//! A later session supervisor supplies volatile addresses and transaction IDs
//! when compiling an individual REGISTER. The carrier-derived, stable device
//! identity is resolved once and remains bound to that session.

use crate::{
    carrier::{ResolvedCarrier, SimIdentity},
    carrier_overrides::{CarrierOverride, ImsRegisterTemplateOverride},
};
use proletarius_ims::{
    PlainSecurityDisposition, SecAgreeMode, aka_authorization::AuthorizationHeader,
    register_request::RegisterRequest, security_offer::ClientSecurityMechanism,
};
use std::{
    fmt,
    net::{IpAddr, SocketAddr},
};
use zeroize::Zeroizing;

const DEFAULT_EXPIRES_SECONDS: u32 = 3_600;
const DEFAULT_LOCAL_PORT: u16 = 5_060;
const DEFAULT_TCP_KEEPALIVE_SECONDS: u64 = 30;
const DEFAULT_OPTIONS_PING_SECONDS: u64 = 30;
const DEFAULT_SUPPORTED: &str = "path,sec-agree,gruu";
const DEFAULT_ALLOW: &str = "INVITE,ACK,CANCEL,BYE,UPDATE,PRACK,MESSAGE,REFER,NOTIFY,INFO,OPTIONS";
const DEFAULT_ACCESS_TYPE: &str = "IEEE-802.11";
const MMTEL_ICSI: &str = "urn:urn-7:3gpp-service.ims.icsi.mmtel";

/// The reader-native userspace IMS stack currently owns reliable TCP only.
/// UDP is intentionally rejected during session preparation instead of being
/// accepted and then silently routed through an unsupported path.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NativeImsTransport {
    Tcp,
}

impl NativeImsTransport {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Tcp => "tcp",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NativeContactMode {
    AndroidDefault,
    Gsma,
    Legacy,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum InitialAuthorization {
    None,
    AkaEmpty,
    AkaEmptyUriFirst,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum NativeImsIdentitySource {
    Derived,
    Isim,
    Auto,
}

/// Subscriber identities resolved from the selected profile and the exact
/// reader lease snapshot. Values are zeroized when the registration attempt
/// finishes and deliberately do not implement Debug or Clone.
pub struct NativeImsSubscriberIdentity {
    carrier_id: String,
    domain: String,
    realm: String,
    identity_source: NativeImsIdentitySource,
    imsi: Zeroizing<String>,
    private_identity: Zeroizing<String>,
    public_identity: Zeroizing<String>,
}

impl NativeImsSubscriberIdentity {
    pub fn private_identity(&self) -> &str {
        self.private_identity.as_str()
    }

    pub fn public_identity(&self) -> &str {
        self.public_identity.as_str()
    }

    fn is_bound_to(&self, profile: &NativeImsRegisterProfile, imsi: &str) -> bool {
        self.carrier_id == profile.carrier_id
            && self.domain == profile.domain
            && self.realm == profile.realm
            && self.identity_source == profile.identity_source
            && self.imsi.as_str() == imsi
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NativeSecurityMechanism {
    algorithm: String,
    encryption: String,
    protocol: String,
    mode: String,
}

/// Retry and initial-fallback policy carried verbatim into the future native
/// session supervisor. `None` deliberately remains distinct from an explicit
/// empty list: the former retains the carrier default and the latter disables
/// that class of retry, matching the existing Go configuration contract.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct NativeRegisterRetryPolicy {
    temporary_status_codes: Option<Vec<u16>>,
    forbidden_status_codes: Option<Vec<u16>>,
    initial_reject_fallback_status_codes: Option<Vec<u16>>,
    temporary_retry_seconds: u64,
}

impl NativeRegisterRetryPolicy {
    pub fn temporary_status_codes(&self) -> Option<&[u16]> {
        self.temporary_status_codes.as_deref()
    }

    pub fn forbidden_status_codes(&self) -> Option<&[u16]> {
        self.forbidden_status_codes.as_deref()
    }

    pub fn initial_reject_fallback_status_codes(&self) -> Option<&[u16]> {
        self.initial_reject_fallback_status_codes.as_deref()
    }

    pub const fn temporary_retry_seconds(&self) -> u64 {
        self.temporary_retry_seconds
    }
}

impl NativeSecurityMechanism {
    pub fn algorithm(&self) -> &str {
        &self.algorithm
    }

    pub fn encryption(&self) -> &str {
        &self.encryption
    }

    pub fn protocol(&self) -> &str {
        &self.protocol
    }

    pub fn mode(&self) -> &str {
        &self.mode
    }
}

/// Immutable device-identity requirement selected with the carrier profile.
/// The actual persistent IMEI is supplied exactly once by the daemon session
/// owner, never generated per REGISTER attempt.
enum NativeDeviceIdentityPolicy {
    Disabled {
        user_agent: String,
    },
    PersistentImei {
        user_agent: String,
        configured_imei: Option<Zeroizing<String>>,
    },
}

/// Stable, zeroizing IMS terminal identity bound to one carrier profile and
/// one session. There is intentionally no public constructor, `Debug`,
/// `Clone`, or raw IMEI accessor.
pub struct NativeImsDeviceIdentity {
    carrier_id: String,
    user_agent: String,
    sip_instance: Option<Zeroizing<String>>,
}

impl NativeImsDeviceIdentity {
    pub fn user_agent(&self) -> &str {
        &self.user_agent
    }

    fn sip_instance(&self) -> Option<&str> {
        self.sip_instance.as_ref().map(|value| value.as_str())
    }

    pub const fn enabled(&self) -> bool {
        self.sip_instance.is_some()
    }

    fn matches_carrier(&self, carrier_id: &str) -> bool {
        self.carrier_id == carrier_id
    }
}

impl NativeDeviceIdentityPolicy {
    fn mode_name(&self) -> &'static str {
        match self {
            Self::Disabled { .. } => "disabled",
            Self::PersistentImei {
                configured_imei: Some(_),
                ..
            } => "configured_imei",
            Self::PersistentImei {
                configured_imei: None,
                ..
            } => "persistent_app_imei",
        }
    }

    fn resolve(
        &self,
        carrier_id: &str,
        persisted_imei: Option<&str>,
    ) -> Result<NativeImsDeviceIdentity, NativeImsProfileError> {
        let (user_agent, imei) = match self {
            Self::Disabled { user_agent } => (user_agent.clone(), None),
            Self::PersistentImei {
                user_agent,
                configured_imei,
            } => {
                let imei = configured_imei
                    .as_ref()
                    .map(|value| value.as_str())
                    .or_else(|| {
                        persisted_imei
                            .map(str::trim)
                            .filter(|value| !value.is_empty())
                    })
                    .ok_or(NativeImsProfileError::MissingPersistentDeviceIdentity)?;
                if !valid_imei_luhn(imei) {
                    return Err(NativeImsProfileError::InvalidDeviceIdentity);
                }
                (user_agent.clone(), Some(Zeroizing::new(imei.to_owned())))
            }
        };
        let sip_instance =
            imei.map(|imei| Zeroizing::new(format!("urn:gsma:imei:{}-0", imei.as_str())));
        Ok(NativeImsDeviceIdentity {
            carrier_id: carrier_id.to_owned(),
            user_agent,
            sip_instance,
        })
    }
}

/// A resolved template is deliberately separate from tunnel configuration:
/// the latter holds only IKE/ESP inputs and must not become an IMS state bag.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NativeImsRegisterTemplate {
    id: String,
    expires_seconds: u32,
    contact_mode: NativeContactMode,
    supported_header: String,
    allow_header: String,
    access_type: String,
    contact_param_order: Vec<String>,
    include_pani_authenticated: bool,
    sec_agree: SecAgreeMode,
    strict_security_server_offer: bool,
    enable_initial_reject_fallback: bool,
    retry_policy: NativeRegisterRetryPolicy,
    security_client_mechanisms: Vec<NativeSecurityMechanism>,
    initial_authorization: InitialAuthorization,
}

impl NativeImsRegisterTemplate {
    pub fn id(&self) -> &str {
        &self.id
    }

    pub const fn expires_seconds(&self) -> u32 {
        self.expires_seconds
    }

    pub const fn contact_mode(&self) -> NativeContactMode {
        self.contact_mode
    }

    pub const fn sec_agree(&self) -> SecAgreeMode {
        self.sec_agree
    }

    pub const fn strict_security_server_offer(&self) -> bool {
        self.strict_security_server_offer
    }

    pub const fn enable_initial_reject_fallback(&self) -> bool {
        self.enable_initial_reject_fallback
    }

    pub fn retry_policy(&self) -> &NativeRegisterRetryPolicy {
        &self.retry_policy
    }

    pub fn client_security_mechanisms(&self) -> Vec<ClientSecurityMechanism<'_>> {
        self.security_client_mechanisms
            .iter()
            .map(|mechanism| ClientSecurityMechanism {
                algorithm: mechanism.algorithm(),
                encryption: mechanism.encryption(),
                protocol: Some(mechanism.protocol()),
                mode: Some(mechanism.mode()),
            })
            .collect()
    }
}

/// Immutable carrier policy for a future native IMS owner.  It contains no
/// subscriber identity, key material, transaction identifier or socket.
pub struct NativeImsRegisterProfile {
    carrier_id: String,
    domain: String,
    realm: String,
    phone_context: String,
    configured_pcscf: Option<SocketAddr>,
    transport: NativeImsTransport,
    local_port: u16,
    tcp_keepalive_seconds: u64,
    options_ping_interval_seconds: u64,
    identity_source: NativeImsIdentitySource,
    device_identity: NativeDeviceIdentityPolicy,
    template: NativeImsRegisterTemplate,
}

impl NativeImsRegisterProfile {
    pub fn carrier_id(&self) -> &str {
        &self.carrier_id
    }

    pub fn domain(&self) -> &str {
        &self.domain
    }

    pub fn realm(&self) -> &str {
        &self.realm
    }

    pub fn phone_context(&self) -> &str {
        &self.phone_context
    }

    pub const fn configured_pcscf(&self) -> Option<SocketAddr> {
        self.configured_pcscf
    }

    pub const fn transport(&self) -> NativeImsTransport {
        self.transport
    }

    pub const fn local_port(&self) -> u16 {
        self.local_port
    }

    pub const fn tcp_keepalive_seconds(&self) -> u64 {
        self.tcp_keepalive_seconds
    }

    pub const fn options_ping_interval_seconds(&self) -> u64 {
        self.options_ping_interval_seconds
    }

    pub fn template(&self) -> &NativeImsRegisterTemplate {
        &self.template
    }

    /// Resolve IMPI/IMPU once from this immutable profile and the same
    /// generation-bound reader snapshot used for IKE AKA. `auto` matches the
    /// established Go behavior: use a complete ISIM pair, derive only when
    /// both values are absent, and reject a partial pair.
    pub fn resolve_subscriber_identity(
        &self,
        runtime: NativeImsRuntime<'_>,
    ) -> Result<NativeImsSubscriberIdentity, NativeImsProfileError> {
        self.resolve_subscriber_identity_from_snapshot(
            runtime.imsi,
            runtime.isim_private_identity,
            runtime.isim_public_identity,
        )
    }

    /// Bind the subscriber identity to a generation-bound SIM snapshot before
    /// transport startup. The returned pair owns zeroizing copies so the
    /// session actor can retain exactly the identity selected during prepare.
    pub fn resolve_subscriber_identity_from_snapshot(
        &self,
        imsi: &str,
        isim_private_identity: Option<&str>,
        isim_public_identity: Option<&str>,
    ) -> Result<NativeImsSubscriberIdentity, NativeImsProfileError> {
        if !numeric(imsi) {
            return Err(NativeImsProfileError::InvalidSimIdentity);
        }
        let private = isim_private_identity.unwrap_or_default().trim();
        let public = isim_public_identity.unwrap_or_default().trim();
        let use_isim = match self.identity_source {
            NativeImsIdentitySource::Derived => false,
            NativeImsIdentitySource::Isim if private.is_empty() || public.is_empty() => {
                return Err(NativeImsProfileError::IncompleteIsimIdentity);
            }
            NativeImsIdentitySource::Isim => true,
            NativeImsIdentitySource::Auto if private.is_empty() && public.is_empty() => false,
            NativeImsIdentitySource::Auto if !private.is_empty() && !public.is_empty() => true,
            NativeImsIdentitySource::Auto => {
                return Err(NativeImsProfileError::IncompleteIsimIdentity);
            }
        };
        let (private, public) = if use_isim {
            if !valid_impi(private) || !valid_impu(public) {
                return Err(NativeImsProfileError::InvalidIsimIdentity);
            }
            (private.to_owned(), public.to_owned())
        } else {
            if !numeric(imsi) || !valid_domain(&self.realm) || !valid_domain(&self.domain) {
                return Err(NativeImsProfileError::InvalidSimIdentity);
            }
            (
                format!("{}@{}", imsi, self.realm),
                format!("sip:{}@{}", imsi, self.domain),
            )
        };
        Ok(NativeImsSubscriberIdentity {
            carrier_id: self.carrier_id.clone(),
            domain: self.domain.clone(),
            realm: self.realm.clone(),
            identity_source: self.identity_source,
            imsi: Zeroizing::new(imsi.to_owned()),
            private_identity: Zeroizing::new(private),
            public_identity: Zeroizing::new(public),
        })
    }

    pub(crate) fn validate_subscriber_identity(
        &self,
        subscriber: &NativeImsSubscriberIdentity,
        imsi: &str,
    ) -> Result<(), NativeImsProfileError> {
        if subscriber.is_bound_to(self, imsi) {
            Ok(())
        } else {
            Err(NativeImsProfileError::SubscriberIdentityMismatch)
        }
    }

    /// Resolves the stable terminal identity once for the future session owner.
    /// An explicit carrier override wins over the app-persisted IMEI; neither
    /// value is exposed in plan diagnostics or logs.
    pub fn resolve_device_identity(
        &self,
        persisted_imei: Option<&str>,
    ) -> Result<NativeImsDeviceIdentity, NativeImsProfileError> {
        self.device_identity
            .resolve(&self.carrier_id, persisted_imei)
    }

    pub fn device_identity_mode(&self) -> &'static str {
        self.device_identity.mode_name()
    }

    /// Compiles one transaction into ordered, serializer-validated bytes.
    /// The identity must have been resolved from this profile before a session
    /// begins; generating one per registration would break the binding
    /// carriers use for terminating calls.
    pub fn compile(
        &self,
        device_identity: &NativeImsDeviceIdentity,
        runtime: NativeImsRuntime<'_>,
        attempt: NativeImsRegisterAttempt<'_>,
    ) -> Result<Zeroizing<Vec<u8>>, NativeImsProfileError> {
        let subscriber = self.resolve_subscriber_identity(runtime)?;
        self.compile_with_subscriber_identity(device_identity, runtime, &subscriber, attempt)
    }

    /// Compile a REGISTER using the identity resolved and retained by the
    /// enclosing generation-bound session. This prevents later REGISTER
    /// variants from rereading mutable SIM identity fields.
    pub(crate) fn compile_with_subscriber_identity(
        &self,
        device_identity: &NativeImsDeviceIdentity,
        runtime: NativeImsRuntime<'_>,
        subscriber: &NativeImsSubscriberIdentity,
        attempt: NativeImsRegisterAttempt<'_>,
    ) -> Result<Zeroizing<Vec<u8>>, NativeImsProfileError> {
        self.validate_runtime(device_identity, runtime)?;
        self.validate_subscriber_identity(subscriber, runtime.imsi)?;
        let (cseq, local_port, attempted_authorization, security_verify, route_port) =
            match &attempt {
                NativeImsRegisterAttempt::Initial {
                    cseq,
                    local_port,
                    endpoints,
                } => {
                    validate_endpoints(*endpoints)?;
                    (*cseq, *local_port, None, None, runtime.pcscf.port())
                }
                NativeImsRegisterAttempt::PlainAuthenticated {
                    cseq,
                    local_port,
                    authorization,
                    endpoints,
                    ..
                } => {
                    validate_endpoints(*endpoints)?;
                    (
                        *cseq,
                        *local_port,
                        Some(*authorization),
                        None,
                        runtime.pcscf.port(),
                    )
                }
                NativeImsRegisterAttempt::Protected {
                    cseq,
                    authorization,
                    endpoints,
                    security_verify,
                    remote_port_s,
                } => {
                    validate_endpoints(*endpoints)?;
                    if security_verify.trim().is_empty() || *remote_port_s == 0 {
                        return Err(NativeImsProfileError::InvalidProtectedContext);
                    }
                    (
                        *cseq,
                        endpoints.port_s,
                        Some(*authorization),
                        Some(*security_verify),
                        *remote_port_s,
                    )
                }
            };
        if cseq == 0 || local_port == 0 {
            return Err(NativeImsProfileError::InvalidTransaction);
        }

        let via = format!(
            "SIP/2.0/TCP {};branch={}",
            format_socket(runtime.local_ip, local_port),
            runtime.via_branch
        );
        let public_uri = subscriber.public_identity();
        let from = format!("<{public_uri}>;tag={}", runtime.from_tag);
        let contact = self.contact(device_identity, runtime, public_uri, local_port)?;
        let route_address = format_socket(runtime.pcscf.ip(), route_port);
        let route = if matches!(&attempt, NativeImsRegisterAttempt::Protected { .. }) {
            // The protected association is bound to port-s/TCP. The initial
            // route intentionally remains the carrier's generic loose route,
            // matching the deployed Go request shape.
            format!("<sip:{route_address};transport=tcp;lr>")
        } else {
            format!("<sip:{route_address};lr>")
        };
        let cseq_value = format!("{cseq} REGISTER");
        let mut dynamic = Vec::<(&str, String)>::with_capacity(20);
        dynamic.push(("Via", via));
        dynamic.push(("From", from));
        dynamic.push(("To", format!("<{public_uri}>")));
        dynamic.push(("Call-ID", runtime.call_id.to_owned()));
        dynamic.push(("CSeq", cseq_value));
        dynamic.push(("Max-Forwards", "70".to_owned()));
        dynamic.push(("Contact", contact));
        let bootstrap = if attempted_authorization.is_none() {
            self.bootstrap_authorization(subscriber.private_identity())?
        } else {
            None
        };
        if let Some(bootstrap) = bootstrap {
            // Initial bootstrap credentials are intentionally a template
            // header (rather than a computed AKA response). Keep it in the
            // deployed order: immediately after Contact.
            dynamic.push(("Authorization", bootstrap));
        }
        dynamic.push(("Route", route));
        dynamic.push(("Expires", self.template.expires_seconds.to_string()));
        if !self.template.supported_header.is_empty() {
            dynamic.push(("Supported", self.template.supported_header.clone()));
        }
        if !self.template.allow_header.is_empty() {
            dynamic.push(("Allow", self.template.allow_header.clone()));
        }

        let gsma = self.template.contact_mode == NativeContactMode::Gsma;
        if !gsma {
            dynamic.push(("P-Preferred-Identity", format!("<{public_uri}>")));
            dynamic.push(("P-Visited-Network-ID", format!("\"{}\"", self.domain)));
        }
        if self.template.include_pani_authenticated {
            dynamic.push(("P-Access-Network-Info", self.pani()));
        }
        let initial = matches!(&attempt, NativeImsRegisterAttempt::Initial { .. });
        if !gsma && (!initial || self.template.id != "apple_orange_uk") {
            dynamic.push(("Cellular-Network-Info", cellular_network_info(runtime)));
        }
        dynamic.push(("Accept-Contact", "*;+g.3gpp.smsip".to_owned()));
        dynamic.push((
            "Accept-Contact",
            format!("*;+g.3gpp.icsi-ref=\"{}\"", encoded_icsi()),
        ));
        let security_disposition = match &attempt {
            NativeImsRegisterAttempt::Initial { .. }
            | NativeImsRegisterAttempt::Protected { .. } => PlainSecurityDisposition::RetainOffer,
            NativeImsRegisterAttempt::PlainAuthenticated {
                security_disposition,
                ..
            } => *security_disposition,
        };
        if matches!(&attempt, NativeImsRegisterAttempt::Protected { .. })
            && self.template.sec_agree == SecAgreeMode::Disabled
        {
            return Err(NativeImsProfileError::InvalidProtectedContext);
        }
        if matches!(
            (&attempt, security_disposition),
            (
                NativeImsRegisterAttempt::PlainAuthenticated { .. },
                PlainSecurityDisposition::OmitOffer
            )
        ) && self.template.sec_agree == SecAgreeMode::Required
        {
            return Err(NativeImsProfileError::InvalidTransaction);
        }
        if matches!(security_disposition, PlainSecurityDisposition::RetainOffer)
            && self.template.sec_agree != SecAgreeMode::Disabled
        {
            let endpoints = match &attempt {
                NativeImsRegisterAttempt::Initial { endpoints, .. }
                | NativeImsRegisterAttempt::PlainAuthenticated { endpoints, .. }
                | NativeImsRegisterAttempt::Protected { endpoints, .. } => *endpoints,
            };
            dynamic.push(("Security-Client", self.security_client(endpoints)?));
            dynamic.push(("Require", "sec-agree".to_owned()));
            dynamic.push(("Proxy-Require", "sec-agree".to_owned()));
        }
        if let Some(security_verify) = security_verify {
            // The wire bytes from Security-Server are deliberately echoed
            // verbatim.  Reordering or reformatting this value has been
            // rejected by deployed O2/giffgaff P-CSCFs.
            dynamic.push(("Security-Verify", security_verify.to_owned()));
        }
        dynamic.push(("User-Agent", device_identity.user_agent().to_owned()));

        let headers: Vec<(&str, &str)> = dynamic
            .iter()
            .map(|(name, value)| (*name, value.as_str()))
            .collect();
        let uri = format!("sip:{}", self.domain);
        let encoded = RegisterRequest {
            uri: &uri,
            headers: &headers,
            body: b"",
        }
        .encode(attempted_authorization)
        .map_err(NativeImsProfileError::Sip)?;
        Ok(encoded)
    }

    fn bootstrap_authorization(
        &self,
        private_identity: &str,
    ) -> Result<Option<String>, NativeImsProfileError> {
        let value = match self.template.initial_authorization {
            InitialAuthorization::None => return Ok(None),
            InitialAuthorization::AkaEmpty => format!(
                "Digest username=\"{}\",realm=\"{}\",nonce=\"\",uri=\"sip:{}\",response=\"\",algorithm=AKAv1-MD5",
                private_identity, self.realm, self.domain
            ),
            InitialAuthorization::AkaEmptyUriFirst => format!(
                "Digest uri=\"sip:{}\",username=\"{}\",algorithm=AKAv1-MD5,response=\"\",realm=\"{}\",nonce=\"\"",
                self.domain, private_identity, self.realm
            ),
        };
        Ok(Some(value))
    }

    fn contact(
        &self,
        device_identity: &NativeImsDeviceIdentity,
        runtime: NativeImsRuntime<'_>,
        public_uri: &str,
        local_port: u16,
    ) -> Result<String, NativeImsProfileError> {
        let base = match self.template.contact_mode {
            NativeContactMode::AndroidDefault => format!(
                "sip:{}@{};transport=tcp",
                runtime.imsi,
                format_socket(runtime.local_ip, local_port)
            ),
            NativeContactMode::Gsma => {
                format!("sip:{}", format_socket(runtime.local_ip, local_port))
            }
            NativeContactMode::Legacy => public_uri.to_owned(),
        };
        let mut contact = format!("<{base}>");
        for parameter in &self.template.contact_param_order {
            match parameter.as_str() {
                "access_type" => {
                    let access_type = if self.template.access_type.is_empty() {
                        DEFAULT_ACCESS_TYPE
                    } else {
                        &self.template.access_type
                    };
                    contact.push_str(";+g.3gpp.accesstype=\"");
                    contact.push_str(access_type);
                    contact.push('"');
                }
                "sip_instance" => {
                    if let Some(sip_instance) = device_identity.sip_instance() {
                        contact.push_str(";+sip.instance=\"<");
                        contact.push_str(sip_instance);
                        contact.push_str(">\"");
                    }
                }
                "audio" => contact.push_str(";audio"),
                "smsip" => contact.push_str(";+g.3gpp.smsip"),
                "icsi_ref" => {
                    contact.push_str(";+g.3gpp.icsi-ref=\"");
                    contact.push_str(&encoded_icsi());
                    contact.push('"');
                }
                "mid_call" => contact.push_str(";+g.3gpp.mid-call"),
                "srvcc_alerting" => contact.push_str(";+g.3gpp.srvcc-alerting"),
                "ps2cs_srvcc_orig_pre_alerting" => {
                    contact.push_str(";+g.3gpp.ps2cs-srvcc-orig-pre-alerting")
                }
                _ => return Err(NativeImsProfileError::InvalidTemplate),
            }
        }
        if self.template.contact_mode != NativeContactMode::Gsma {
            contact.push_str(";expires=");
            contact.push_str(&self.template.expires_seconds.to_string());
        }
        Ok(contact)
    }

    fn security_client(
        &self,
        endpoints: NativeSecurityEndpoints,
    ) -> Result<String, NativeImsProfileError> {
        validate_endpoints(endpoints)?;
        if self.template.security_client_mechanisms.is_empty() {
            return Err(NativeImsProfileError::NoNativeSecurityMechanism);
        }
        Ok(self
            .template
            .security_client_mechanisms
            .iter()
            .map(|mechanism| {
                format!(
                    "ipsec-3gpp; alg={}; ealg={}; prot={}; mod={}; spi-c={}; spi-s={}; port-c={}; port-s={}",
                    mechanism.algorithm,
                    mechanism.encryption,
                    mechanism.protocol,
                    mechanism.mode,
                    endpoints.spi_c,
                    endpoints.spi_s,
                    endpoints.port_c,
                    endpoints.port_s,
                )
            })
            .collect::<Vec<_>>()
            .join(","))
    }

    fn pani(&self) -> String {
        if self.template.id == "apple_orange_uk" {
            "IEEE-802.11;country=GB".to_owned()
        } else if self.template.id == "giffgaff" {
            "IEEE-802.11;country=GB;i-wlan-node-id=000000000000".to_owned()
        } else {
            "IEEE-802.11;i-wlan-node-id=000000000000;network-provided".to_owned()
        }
    }

    fn validate_runtime(
        &self,
        device_identity: &NativeImsDeviceIdentity,
        runtime: NativeImsRuntime<'_>,
    ) -> Result<(), NativeImsProfileError> {
        if !device_identity.matches_carrier(&self.carrier_id) {
            return Err(NativeImsProfileError::InvalidDeviceIdentity);
        }
        if matches!(
            self.carrier_id.as_str(),
            "apple_o2_giffgaff_uk" | "apple_orange_uk"
        ) && device_identity.sip_instance().is_none()
        {
            // These reviewed UK profiles use the stable SIP instance in their
            // terminating-call binding. Generic carriers may explicitly opt
            // out of terminal identity, but this pair must fail closed.
            return Err(NativeImsProfileError::MissingPersistentDeviceIdentity);
        }
        if !numeric(runtime.imsi)
            || !numeric_width(runtime.mcc, 3)
            || !numeric(runtime.mnc)
            || !(2..=3).contains(&runtime.mnc.len())
            || runtime.local_ip.is_unspecified()
            || runtime.pcscf.ip().is_unspecified()
            || runtime.local_ip.is_ipv4() != runtime.pcscf.ip().is_ipv4()
            || [runtime.call_id, runtime.from_tag, runtime.via_branch]
                .iter()
                .any(|value| !valid_header_value(value))
            || runtime.cell_id.is_some_and(|value| !numeric(value))
        {
            return Err(NativeImsProfileError::InvalidRuntime);
        }
        if self
            .configured_pcscf
            .is_some_and(|configured| configured != runtime.pcscf)
        {
            return Err(NativeImsProfileError::ConfiguredPcscfMismatch);
        }
        Ok(())
    }
}

/// Per-transaction non-secret data.  The owner supplies fresh Call-ID,
/// From-tag and Via branch; tests pass stable synthetic values to permit a
/// deterministic wire comparison. Device identity deliberately does not
/// appear here: it is immutable for the enclosing reader/SIM session.
#[derive(Clone, Copy)]
pub struct NativeImsRuntime<'a> {
    pub imsi: &'a str,
    /// Optional cached ISIM identities from the same generation-bound reader
    /// lease. These values are only used for `isim` or `auto` policy.
    pub isim_private_identity: Option<&'a str>,
    pub isim_public_identity: Option<&'a str>,
    pub local_ip: IpAddr,
    pub pcscf: SocketAddr,
    pub call_id: &'a str,
    pub from_tag: &'a str,
    pub via_branch: &'a str,
    pub cell_id: Option<&'a str>,
    pub mcc: &'a str,
    pub mnc: &'a str,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct NativeSecurityEndpoints {
    pub spi_c: u32,
    pub spi_s: u32,
    pub port_c: u16,
    pub port_s: u16,
}

pub enum NativeImsRegisterAttempt<'a> {
    Initial {
        cseq: u32,
        local_port: u16,
        endpoints: NativeSecurityEndpoints,
    },
    PlainAuthenticated {
        cseq: u32,
        local_port: u16,
        endpoints: NativeSecurityEndpoints,
        authorization: &'a AuthorizationHeader,
        /// The decision taken from the preceding 401/407.  In `Auto` mode a
        /// missing Security-Server requires an authenticated plain retry with
        /// no `Security-*`, `Require` or `Proxy-Require` headers.
        security_disposition: PlainSecurityDisposition,
    },
    Protected {
        cseq: u32,
        endpoints: NativeSecurityEndpoints,
        authorization: &'a AuthorizationHeader,
        security_verify: &'a str,
        remote_port_s: u16,
    },
}

#[derive(Debug)]
pub enum NativeImsProfileError {
    InvalidSimIdentity,
    InvalidDomain,
    InvalidPcscf,
    ConfiguredPcscfMismatch,
    UnsupportedTransport,
    UnsupportedIdentitySource,
    SubscriberIdentityMismatch,
    InvalidIsimIdentity,
    IncompleteIsimIdentity,
    UnsupportedSecurityMechanism,
    NoNativeSecurityMechanism,
    InvalidTemplate,
    InvalidDeviceIdentity,
    MissingPersistentDeviceIdentity,
    InvalidRuntime,
    InvalidTransaction,
    InvalidEndpoints,
    InvalidProtectedContext,
    Sip(proletarius_ims::SipError),
}

impl fmt::Display for NativeImsProfileError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let message = match self {
            Self::InvalidSimIdentity => "native IMS requires a numeric SIM identity",
            Self::InvalidDomain => "native IMS domain or realm is invalid",
            Self::InvalidPcscf => {
                "ims_pcscf must be an inner numeric IP address with an optional port"
            }
            Self::ConfiguredPcscfMismatch => {
                "the selected P-CSCF does not match the configured ims_pcscf"
            }
            Self::UnsupportedTransport => "native reader IMS supports TCP only",
            Self::UnsupportedIdentitySource => {
                "native IMS identity source must be derived, isim, or auto"
            }
            Self::SubscriberIdentityMismatch => {
                "the resolved IMS subscriber identity does not match this SIM session"
            }
            Self::InvalidIsimIdentity => "the cached ISIM IMPI/IMPU is malformed",
            Self::IncompleteIsimIdentity => {
                "ISIM identity source requires a complete IMPI/IMPU pair"
            }
            Self::UnsupportedSecurityMechanism => {
                "native IMS can install only AES-CBC/HMAC-SHA1-96 ESP transport mode"
            }
            Self::NoNativeSecurityMechanism => {
                "sec-agree requires one native-installable security mechanism"
            }
            Self::InvalidTemplate => "native IMS register template is invalid",
            Self::InvalidDeviceIdentity => "native IMS device identity is invalid",
            Self::MissingPersistentDeviceIdentity => {
                "native IMS requires the existing persistent device identity"
            }
            Self::InvalidRuntime => "native IMS transaction input is invalid",
            Self::InvalidTransaction => "native IMS transaction values are invalid",
            Self::InvalidEndpoints => "native IMS security endpoints are invalid",
            Self::InvalidProtectedContext => "native IMS protected REGISTER context is invalid",
            Self::Sip(error) => {
                return write!(formatter, "native IMS REGISTER encoding failed: {error}");
            }
        };
        formatter.write_str(message)
    }
}

impl std::error::Error for NativeImsProfileError {}

impl ResolvedCarrier {
    /// Resolves every currently supported IMS field from the same carrier/SIM
    /// snapshot as `native_tunnel_config`.  Unsupported ISIM/UDP paths stop
    /// here, before any tunnel, DNS or TCP action can occur.
    pub fn native_ims_register_profile(
        &self,
        sim: &SimIdentity,
        override_value: Option<&CarrierOverride>,
    ) -> Result<NativeImsRegisterProfile, NativeImsProfileError> {
        if !numeric(&sim.imsi)
            || !numeric_width(&sim.mcc, 3)
            || !(2..=3).contains(&sim.mnc.len())
            || !numeric(&sim.mnc)
        {
            return Err(NativeImsProfileError::InvalidSimIdentity);
        }
        let fallback_domain = format!("ims.mnc{}.mcc{}.3gppnetwork.org", pad3(&sim.mnc), sim.mcc);
        let domain = override_value
            .and_then(|value| nonempty(&value.ims_domain))
            .unwrap_or(&fallback_domain)
            .to_ascii_lowercase();
        let realm = override_value
            .and_then(|value| nonempty(&value.ims_realm))
            .unwrap_or(&domain)
            .to_ascii_lowercase();
        let phone_context = override_value
            .and_then(|value| nonempty(&value.ims_phone_context))
            .unwrap_or(&domain)
            .to_ascii_lowercase();
        if !valid_domain(&domain) || !valid_domain(&realm) || !valid_domain(&phone_context) {
            return Err(NativeImsProfileError::InvalidDomain);
        }
        let configured_pcscf = match override_value.and_then(|value| nonempty(&value.ims_pcscf)) {
            Some(value) => Some(parse_numeric_pcscf(value)?),
            None => None,
        };
        let transport = match override_value
            .and_then(|value| value.ims_transport.as_deref())
            .unwrap_or("auto")
        {
            "auto" | "tcp" => NativeImsTransport::Tcp,
            "udp" => return Err(NativeImsProfileError::UnsupportedTransport),
            _ => return Err(NativeImsProfileError::UnsupportedTransport),
        };
        let identity_source = match override_value
            .and_then(|value| value.ims_identity_source.as_deref())
            .unwrap_or("derived")
        {
            "derived" => NativeImsIdentitySource::Derived,
            "isim" => NativeImsIdentitySource::Isim,
            "auto" => NativeImsIdentitySource::Auto,
            _ => return Err(NativeImsProfileError::UnsupportedIdentitySource),
        };
        let local_port = override_value
            .and_then(|value| value.ims_local_port)
            .unwrap_or(DEFAULT_LOCAL_PORT);
        if local_port == 0 {
            return Err(NativeImsProfileError::InvalidTransaction);
        }
        let tcp_keepalive_seconds = override_value
            .and_then(|value| value.ims_tcp_keepalive_seconds)
            .unwrap_or(DEFAULT_TCP_KEEPALIVE_SECONDS);
        let options_ping_interval_seconds = override_value
            .and_then(|value| value.ims_options_ping_interval_seconds)
            .unwrap_or(DEFAULT_OPTIONS_PING_SECONDS);
        if tcp_keepalive_seconds == 0 || options_ping_interval_seconds == 0 {
            return Err(NativeImsProfileError::InvalidTransaction);
        }
        let template = resolve_template(
            &self.id,
            override_value.and_then(|value| value.ims_register_template.as_ref()),
        )?;
        let device_identity = resolve_device_identity_policy(&self.id, override_value, &template)?;
        Ok(NativeImsRegisterProfile {
            carrier_id: self.id.clone(),
            domain,
            realm,
            phone_context,
            configured_pcscf,
            transport,
            local_port,
            tcp_keepalive_seconds,
            options_ping_interval_seconds,
            identity_source,
            device_identity,
            template,
        })
    }
}

fn resolve_template(
    carrier_id: &str,
    override_value: Option<&ImsRegisterTemplateOverride>,
) -> Result<NativeImsRegisterTemplate, NativeImsProfileError> {
    let mut template = default_template(carrier_id);
    if let Some(override_value) = override_value {
        if let Some(value) = nonempty(&override_value.id) {
            template.id = value.to_owned();
        }
        if let Some(value) = override_value.expires {
            template.expires_seconds =
                u32::try_from(value).map_err(|_| NativeImsProfileError::InvalidTemplate)?;
        }
        if let Some(value) = override_value.contact_mode.as_deref() {
            template.contact_mode = parse_contact_mode(value)?;
        }
        if let Some(value) = override_value.sec_agree_mode.as_deref() {
            template.sec_agree = parse_sec_agree(value)?;
        }
        if let Some(value) = nonempty(&override_value.supported_header) {
            template.supported_header = value.to_owned();
        }
        if let Some(value) = nonempty(&override_value.access_type) {
            template.access_type = value.to_owned();
        }
        if let Some(value) = &override_value.contact_param_order {
            template.contact_param_order = value.clone();
        }
        if let Some(value) = override_value.include_pani_authenticated {
            template.include_pani_authenticated = value;
        }
        if let Some(value) = override_value.strict_security_server_offer {
            template.strict_security_server_offer = value;
        }
        if let Some(value) = override_value.enable_initial_reject_fallback {
            template.enable_initial_reject_fallback = value;
        }
        if let Some(policy) = &override_value.register_policy {
            if let Some(value) = &policy.temporary_status_codes {
                template.retry_policy.temporary_status_codes = Some(value.clone());
            }
            if let Some(value) = &policy.forbidden_status_codes {
                template.retry_policy.forbidden_status_codes = Some(value.clone());
            }
            if let Some(value) = &policy.initial_reject_fallback_status_codes {
                template.retry_policy.initial_reject_fallback_status_codes = Some(value.clone());
            }
            if let Some(value) = policy.temporary_retry_seconds {
                template.retry_policy.temporary_retry_seconds = value;
            }
        }
        if let Some(mechanisms) = &override_value.security_client_mechanisms {
            template.security_client_mechanisms = mechanisms
                .iter()
                .map(native_security_mechanism)
                .collect::<Result<_, _>>()?;
        }
    }
    if template.expires_seconds == 0
        || template.id.trim().is_empty()
        || !valid_header_value(&template.supported_header)
        || !valid_header_value(&template.allow_header)
        || (template
            .contact_param_order
            .iter()
            .any(|name| name == "access_type")
            && !valid_header_value(&template.access_type))
    {
        return Err(NativeImsProfileError::InvalidTemplate);
    }
    // The native installer deliberately supports one audited suite only. A
    // non-strict offer selector could accept a different server mechanism and
    // only fail after the AKA exchange, so it is rejected at plan time.
    if template.sec_agree != SecAgreeMode::Disabled && !template.strict_security_server_offer {
        return Err(NativeImsProfileError::UnsupportedSecurityMechanism);
    }
    if template.sec_agree != SecAgreeMode::Disabled
        && template.security_client_mechanisms.is_empty()
    {
        return Err(NativeImsProfileError::NoNativeSecurityMechanism);
    }
    for name in &template.contact_param_order {
        if !matches!(
            name.as_str(),
            "access_type"
                | "sip_instance"
                | "audio"
                | "smsip"
                | "icsi_ref"
                | "mid_call"
                | "srvcc_alerting"
                | "ps2cs_srvcc_orig_pre_alerting"
        ) {
            return Err(NativeImsProfileError::InvalidTemplate);
        }
    }
    if matches!(carrier_id, "apple_o2_giffgaff_uk" | "apple_orange_uk")
        && ["icsi_ref", "sip_instance", "smsip"]
            .iter()
            .any(|required| {
                !template
                    .contact_param_order
                    .iter()
                    .any(|item| item == required)
            })
    {
        // These are part of the registered binding used for MT voice/SMS by
        // the two reviewed UK profiles.  A custom replacement that omits them
        // may look registered but is not a safe native migration candidate.
        return Err(NativeImsProfileError::InvalidTemplate);
    }
    if template.sec_agree == SecAgreeMode::Disabled
        && !matches!(carrier_id, "apple_o2_giffgaff_uk" | "apple_orange_uk")
    {
        // Go's generic template selects no bootstrap scheme once sec-agree is
        // disabled. The reviewed UK templates explicitly opt into aka_empty
        // and retain that carrier-specific behavior.
        template.initial_authorization = InitialAuthorization::None;
    }
    Ok(template)
}

fn resolve_device_identity_policy(
    carrier_id: &str,
    override_value: Option<&CarrierOverride>,
    template: &NativeImsRegisterTemplate,
) -> Result<NativeDeviceIdentityPolicy, NativeImsProfileError> {
    let model = override_value
        .and_then(|value| nonempty(&value.device_model))
        .unwrap_or("iPhone");
    if !valid_header_value(model) {
        return Err(NativeImsProfileError::InvalidDeviceIdentity);
    }
    let user_agent = if carrier_id == "apple_o2_giffgaff_uk" {
        // This exact terminal family string is what the deployed Go session
        // uses for giffgaff. Do not substitute the local Mac model.
        "iOS/18.6.2 iPhone".to_owned()
    } else {
        format!("iOS/26.6 {model}")
    };
    if !valid_header_value(&user_agent) {
        return Err(NativeImsProfileError::InvalidDeviceIdentity);
    }
    if override_value.and_then(|value| value.device_identity_enabled) == Some(false) {
        if matches!(carrier_id, "apple_o2_giffgaff_uk" | "apple_orange_uk")
            && template
                .contact_param_order
                .iter()
                .any(|parameter| parameter == "sip_instance")
        {
            return Err(NativeImsProfileError::InvalidDeviceIdentity);
        }
        return Ok(NativeDeviceIdentityPolicy::Disabled { user_agent });
    }
    let configured_imei =
        match override_value.and_then(|value| nonempty(&value.device_identity_imei)) {
            Some(imei) if valid_imei_luhn(imei) => Some(Zeroizing::new(imei.to_owned())),
            Some(_) => return Err(NativeImsProfileError::InvalidDeviceIdentity),
            None => None,
        };
    Ok(NativeDeviceIdentityPolicy::PersistentImei {
        user_agent,
        configured_imei,
    })
}

fn default_template(carrier_id: &str) -> NativeImsRegisterTemplate {
    let mut template = NativeImsRegisterTemplate {
        id: carrier_id.to_owned(),
        expires_seconds: DEFAULT_EXPIRES_SECONDS,
        contact_mode: NativeContactMode::AndroidDefault,
        supported_header: DEFAULT_SUPPORTED.to_owned(),
        allow_header: DEFAULT_ALLOW.to_owned(),
        access_type: DEFAULT_ACCESS_TYPE.to_owned(),
        contact_param_order: vec![
            "access_type".to_owned(),
            "audio".to_owned(),
            "smsip".to_owned(),
            "icsi_ref".to_owned(),
            "sip_instance".to_owned(),
        ],
        include_pani_authenticated: true,
        sec_agree: SecAgreeMode::Auto,
        strict_security_server_offer: true,
        enable_initial_reject_fallback: false,
        retry_policy: NativeRegisterRetryPolicy::default(),
        security_client_mechanisms: vec![native_safe_mechanism()],
        initial_authorization: InitialAuthorization::AkaEmptyUriFirst,
    };
    match carrier_id {
        "apple_o2_giffgaff_uk" => {
            template.id = "giffgaff".to_owned();
            template.contact_mode = NativeContactMode::Gsma;
            template.supported_header = "path,gruu".to_owned();
            template.allow_header =
                "REGISTER,INVITE,ACK,CANCEL,BYE,OPTIONS,MESSAGE,SUBSCRIBE,NOTIFY".to_owned();
            template.access_type.clear();
            template.contact_param_order = vec![
                "icsi_ref".to_owned(),
                "mid_call".to_owned(),
                "smsip".to_owned(),
                "sip_instance".to_owned(),
            ];
            template.initial_authorization = InitialAuthorization::AkaEmpty;
        }
        "apple_orange_uk" => {
            template.id = "apple_orange_uk".to_owned();
            template.contact_param_order = vec![
                "access_type".to_owned(),
                "audio".to_owned(),
                "smsip".to_owned(),
                "icsi_ref".to_owned(),
                "mid_call".to_owned(),
                "srvcc_alerting".to_owned(),
                "ps2cs_srvcc_orig_pre_alerting".to_owned(),
                "sip_instance".to_owned(),
            ];
            template.initial_authorization = InitialAuthorization::AkaEmpty;
        }
        _ => {}
    }
    template
}

fn native_safe_mechanism() -> NativeSecurityMechanism {
    NativeSecurityMechanism {
        algorithm: "hmac-sha-1-96".to_owned(),
        encryption: "aes-cbc".to_owned(),
        protocol: "esp".to_owned(),
        mode: "trans".to_owned(),
    }
}

fn native_security_mechanism(
    value: &crate::carrier_overrides::SecurityClientMechanism,
) -> Result<NativeSecurityMechanism, NativeImsProfileError> {
    let algorithm = value.alg.as_deref().unwrap_or("").trim();
    let encryption = value.ealg.as_deref().unwrap_or("").trim();
    let protocol = value.prot.as_deref().unwrap_or("esp").trim();
    let mode = value.mode.as_deref().unwrap_or("trans").trim();
    if !algorithm.eq_ignore_ascii_case("hmac-sha-1-96")
        || !encryption.eq_ignore_ascii_case("aes-cbc")
        || !protocol.eq_ignore_ascii_case("esp")
        || !mode.eq_ignore_ascii_case("trans")
    {
        return Err(NativeImsProfileError::UnsupportedSecurityMechanism);
    }
    Ok(native_safe_mechanism())
}

fn parse_contact_mode(value: &str) -> Result<NativeContactMode, NativeImsProfileError> {
    match value {
        "android_default" => Ok(NativeContactMode::AndroidDefault),
        "gsma" => Ok(NativeContactMode::Gsma),
        "legacy" => Ok(NativeContactMode::Legacy),
        _ => Err(NativeImsProfileError::InvalidTemplate),
    }
}

fn parse_sec_agree(value: &str) -> Result<SecAgreeMode, NativeImsProfileError> {
    match value {
        "auto" => Ok(SecAgreeMode::Auto),
        "required" => Ok(SecAgreeMode::Required),
        "disabled" => Ok(SecAgreeMode::Disabled),
        _ => Err(NativeImsProfileError::InvalidTemplate),
    }
}

fn parse_numeric_pcscf(value: &str) -> Result<SocketAddr, NativeImsProfileError> {
    let value = value.trim();
    if let Ok(address) = value.parse::<IpAddr>() {
        return Ok(SocketAddr::new(address, DEFAULT_LOCAL_PORT));
    }
    value
        .parse::<SocketAddr>()
        .map_err(|_| NativeImsProfileError::InvalidPcscf)
}

fn validate_endpoints(endpoints: NativeSecurityEndpoints) -> Result<(), NativeImsProfileError> {
    if endpoints.spi_c == 0
        || endpoints.spi_s == 0
        || endpoints.spi_c == endpoints.spi_s
        || endpoints.port_c == 0
        || endpoints.port_s == 0
    {
        return Err(NativeImsProfileError::InvalidEndpoints);
    }
    Ok(())
}

fn cellular_network_info(runtime: NativeImsRuntime<'_>) -> String {
    let mnc = runtime.mnc.trim_start_matches('0');
    let plmn = format!("{}{}", runtime.mcc, if mnc.is_empty() { "0" } else { mnc });
    let cell = runtime.cell_id.unwrap_or("0000000");
    format!("3GPP-E-UTRAN-FDD;utran-cell-id-3gpp={plmn}{cell};cell-info-age=0")
}

fn encoded_icsi() -> String {
    MMTEL_ICSI.replace(':', "%3A")
}

fn format_socket(address: IpAddr, port: u16) -> String {
    SocketAddr::new(address, port).to_string()
}

fn nonempty(value: &Option<String>) -> Option<&str> {
    value
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
}

fn valid_imei_luhn(value: &str) -> bool {
    if value.len() != 15 || !value.bytes().all(|digit| digit.is_ascii_digit()) {
        return false;
    }
    value
        .bytes()
        .enumerate()
        .map(|(index, byte)| {
            let mut digit = u32::from(byte - b'0');
            if index % 2 == 1 {
                digit *= 2;
                if digit > 9 {
                    digit -= 9;
                }
            }
            digit
        })
        .sum::<u32>()
        % 10
        == 0
}

fn numeric(value: &str) -> bool {
    !value.is_empty() && value.bytes().all(|byte| byte.is_ascii_digit())
}

fn numeric_width(value: &str, width: usize) -> bool {
    value.len() == width && numeric(value)
}

fn pad3(value: &str) -> String {
    format!("{value:0>3}")
}

fn valid_domain(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 253
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

fn valid_impi(value: &str) -> bool {
    if value.is_empty()
        || value.len() > 256
        || !safe_identity_bytes(value)
        || value.bytes().filter(|byte| *byte == b'@').count() != 1
    {
        return false;
    }
    let Some((user, domain)) = value.split_once('@') else {
        return false;
    };
    !user.is_empty() && valid_domain(domain)
}

fn valid_impu(value: &str) -> bool {
    if value.is_empty() || value.len() > 512 || !safe_identity_bytes(value) {
        return false;
    }
    if let Some(number) = value.strip_prefix("tel:") {
        return !number.is_empty()
            && number
                .bytes()
                .all(|byte| byte.is_ascii_digit() || b"+*#-.()".contains(&byte));
    }
    let Some(uri) = value
        .strip_prefix("sip:")
        .or_else(|| value.strip_prefix("sips:"))
    else {
        return false;
    };
    let Some((user, host_and_params)) = uri.split_once('@') else {
        return false;
    };
    if user.is_empty() {
        return false;
    }
    let host_port = host_and_params.split([';', '?']).next().unwrap_or_default();
    let host = host_port
        .rsplit_once(':')
        .filter(|(_, port)| !port.is_empty() && port.bytes().all(|byte| byte.is_ascii_digit()))
        .map_or(host_port, |(host, _)| host);
    valid_domain(host)
}

fn safe_identity_bytes(value: &str) -> bool {
    value
        .bytes()
        .all(|byte| (0x21..=0x7e).contains(&byte) && !matches!(byte, b'<' | b'>' | b'"' | b'\\'))
}

fn valid_header_value(value: &str) -> bool {
    !value.trim().is_empty() && !value.bytes().any(|byte| byte.is_ascii_control())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::carrier::{Registry, SimIdentity};

    const TEST_IMEI: &str = "353936932685355";

    fn sim(mcc: &str, mnc: &str, imsi: &str) -> SimIdentity {
        SimIdentity {
            mcc: mcc.to_owned(),
            mnc: mnc.to_owned(),
            imsi: imsi.to_owned(),
            ..Default::default()
        }
    }

    fn profile(sim: &SimIdentity) -> NativeImsRegisterProfile {
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(sim).unwrap().unwrap();
        carrier.native_ims_register_profile(sim, None).unwrap()
    }

    fn device_identity(profile: &NativeImsRegisterProfile) -> NativeImsDeviceIdentity {
        profile.resolve_device_identity(Some(TEST_IMEI)).unwrap()
    }

    fn runtime<'a>(imsi: &'a str, mcc: &'a str, mnc: &'a str) -> NativeImsRuntime<'a> {
        NativeImsRuntime {
            imsi,
            isim_private_identity: None,
            isim_public_identity: None,
            local_ip: "2001:db8::10".parse().unwrap(),
            pcscf: "[2001:db8::20]:5060".parse().unwrap(),
            call_id: "native-register-1",
            from_tag: "from-1",
            via_branch: "z9hG4bK-native-1",
            cell_id: Some("0000000"),
            mcc,
            mnc,
        }
    }

    fn endpoints() -> NativeSecurityEndpoints {
        NativeSecurityEndpoints {
            spi_c: 111,
            spi_s: 222,
            port_c: 5064,
            port_s: 5063,
        }
    }

    #[test]
    fn ctexcel_initial_register_preserves_terminating_call_contact_features() {
        let sim = sim("234", "33", "234330000000001");
        let profile = profile(&sim);
        let device_identity = device_identity(&profile);
        assert_eq!(profile.template().id(), "apple_orange_uk");
        let wire = profile
            .compile(
                &device_identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: 5060,
                    endpoints: endpoints(),
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        assert!(wire.contains("Authorization: Digest username=\"234330000000001@ims.mnc033.mcc234.3gppnetwork.org\",realm=\"ims.mnc033.mcc234.3gppnetwork.org\",nonce=\"\",uri=\"sip:ims.mnc033.mcc234.3gppnetwork.org\",response=\"\",algorithm=AKAv1-MD5\r\n"));
        assert!(wire.contains("P-Access-Network-Info: IEEE-802.11;country=GB\r\n"));
        assert!(wire.contains("Route: <sip:[2001:db8::20]:5060;lr>\r\n"));
        assert!(!wire.contains("Cellular-Network-Info:"));
        assert!(wire.contains("Contact: <sip:234330000000001@[2001:db8::10]:5060;transport=tcp>;+g.3gpp.accesstype=\"IEEE-802.11\";audio;+g.3gpp.smsip;+g.3gpp.icsi-ref=\"urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel\";+g.3gpp.mid-call;+g.3gpp.srvcc-alerting;+g.3gpp.ps2cs-srvcc-orig-pre-alerting;+sip.instance=\"<urn:gsma:imei:353936932685355-0>\";expires=3600\r\n"));
        assert!(wire.contains("User-Agent: iOS/26.6 iPhone\r\n"));
        assert!(wire.contains("Security-Client: ipsec-3gpp; alg=hmac-sha-1-96; ealg=aes-cbc; prot=esp; mod=trans; spi-c=111; spi-s=222; port-c=5064; port-s=5063\r\n"));
        assert!(wire.contains("Require: sec-agree\r\nProxy-Require: sec-agree\r\n"));
    }

    #[test]
    fn device_identity_matches_the_deployed_go_terminal_contract() {
        let ctexcel_sim = sim("234", "33", "234330000000001");
        let ctexcel = profile(&ctexcel_sim);
        let ctexcel_identity = device_identity(&ctexcel);
        assert_eq!(ctexcel_identity.user_agent(), "iOS/26.6 iPhone");
        assert_eq!(
            ctexcel_identity.sip_instance(),
            Some("urn:gsma:imei:353936932685355-0")
        );

        let mut giffgaff_sim = sim("234", "10", "234100000000001");
        giffgaff_sim.gid1 = "508FFFFF".to_owned();
        let giffgaff = profile(&giffgaff_sim);
        let giffgaff_identity = device_identity(&giffgaff);
        assert_eq!(giffgaff_identity.user_agent(), "iOS/18.6.2 iPhone");
        assert_eq!(
            giffgaff_identity.sip_instance(),
            Some("urn:gsma:imei:353936932685355-0")
        );
    }

    #[test]
    fn configured_identity_wins_over_persisted_identity_and_keeps_the_model() {
        let sim = sim("234", "33", "234330000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            br#"
carrier_overrides:
  "234/33":
    device_model: iphone18,1
    device_identity_imei: "351234567890124"
"#,
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        let identity = profile.resolve_device_identity(Some(TEST_IMEI)).unwrap();
        assert_eq!(profile.device_identity_mode(), "configured_imei");
        assert_eq!(identity.user_agent(), "iOS/26.6 iphone18,1");
        assert_eq!(
            identity.sip_instance(),
            Some("urn:gsma:imei:351234567890124-0")
        );
    }

    #[test]
    fn generic_profile_can_explicitly_disable_device_identity_without_uuid_fallback() {
        let sim = sim("234", "20", "234200000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/20': {device_identity_enabled: false, ims_register_template: {sec_agree_mode: disabled}}\n",
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "20"))
            .unwrap();
        let identity = profile.resolve_device_identity(None).unwrap();
        assert_eq!(profile.device_identity_mode(), "disabled");
        assert!(!identity.enabled());
        let wire = profile
            .compile(
                &identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: 5060,
                    endpoints: endpoints(),
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        assert!(!wire.contains("+sip.instance="));
        assert!(!wire.contains("urn:uuid:"));
    }

    #[test]
    fn reviewed_uk_profiles_require_the_existing_stable_identity() {
        let sim = sim("234", "33", "234330000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let disabled = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/33': {device_identity_enabled: false}\n",
        )
        .unwrap();
        assert!(matches!(
            carrier.native_ims_register_profile(&sim, disabled.get("234", "33")),
            Err(NativeImsProfileError::InvalidDeviceIdentity)
        ));

        let profile = profile(&sim);
        assert!(matches!(
            profile.resolve_device_identity(None),
            Err(NativeImsProfileError::MissingPersistentDeviceIdentity)
        ));
    }

    #[test]
    fn identity_is_bound_to_its_carrier_and_model_header_is_validated() {
        let ctexcel_sim = sim("234", "33", "234330000000001");
        let ctexcel = profile(&ctexcel_sim);
        let mut giffgaff_sim = sim("234", "10", "234100000000001");
        giffgaff_sim.gid1 = "508FFFFF".to_owned();
        let giffgaff = profile(&giffgaff_sim);
        let giffgaff_identity = device_identity(&giffgaff);
        assert!(matches!(
            ctexcel.compile(
                &giffgaff_identity,
                runtime(&ctexcel_sim.imsi, &ctexcel_sim.mcc, &ctexcel_sim.mnc),
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: 5060,
                    endpoints: endpoints(),
                },
            ),
            Err(NativeImsProfileError::InvalidDeviceIdentity)
        ));

        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&ctexcel_sim).unwrap().unwrap();
        let invalid_model = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/33': {device_model: \"iphone\\r\\nInjected: x\"}\n",
        )
        .unwrap();
        assert!(matches!(
            carrier.native_ims_register_profile(&ctexcel_sim, invalid_model.get("234", "33")),
            Err(NativeImsProfileError::InvalidDeviceIdentity)
        ));
    }

    #[test]
    fn giffgaff_gsma_contact_omits_incompatible_business_headers() {
        let sim = sim("234", "10", "234100000000001");
        let mut sim = sim;
        sim.gid1 = "508FFFFF".to_owned();
        let profile = profile(&sim);
        let device_identity = device_identity(&profile);
        assert_eq!(profile.template().id(), "giffgaff");
        let wire = profile
            .compile(
                &device_identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: 5060,
                    endpoints: endpoints(),
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        assert!(wire.contains("Authorization: Digest username=\"234100000000001@ims.mnc010.mcc234.3gppnetwork.org\",realm=\"ims.mnc010.mcc234.3gppnetwork.org\",nonce=\"\",uri=\"sip:ims.mnc010.mcc234.3gppnetwork.org\",response=\"\",algorithm=AKAv1-MD5\r\n"));
        assert!(wire.contains("Supported: path,gruu\r\n"));
        assert!(wire.contains(
            "P-Access-Network-Info: IEEE-802.11;country=GB;i-wlan-node-id=000000000000\r\n"
        ));
        assert!(wire.contains("Contact: <sip:[2001:db8::10]:5060>;+g.3gpp.icsi-ref=\"urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel\";+g.3gpp.mid-call;+g.3gpp.smsip;+sip.instance=\"<urn:gsma:imei:353936932685355-0>\"\r\n"));
        assert!(wire.contains("User-Agent: iOS/18.6.2 iPhone\r\n"));
        for forbidden in [
            "P-Preferred-Identity:",
            "P-Visited-Network-ID:",
            "Cellular-Network-Info:",
            "Contact: <sip:[2001:db8::10]:5060;transport=tcp>",
            "+sip.instance=\"<urn:gsma:imei:353936932685355-0>\";expires=",
        ] {
            assert!(
                !wire.contains(forbidden),
                "unexpected {forbidden} in {wire}"
            );
        }
    }

    #[test]
    fn protected_register_echoes_server_verify_and_uses_security_ports() {
        let sim = sim("234", "33", "234330000000001");
        let profile = profile(&sim);
        let device_identity = device_identity(&profile);
        let response = proletarius_ims::SipResponse::parse(
            format!(
                "SIP/2.0 401 Unauthorized\r\nCall-ID: native-register-1\r\nCSeq: 1 REGISTER\r\nWWW-Authenticate: Digest realm=\"ims.mnc033.mcc234.3gppnetwork.org\",nonce=\"{}\",algorithm=AKAv1-MD5\r\nContent-Length: 0\r\n\r\n",
                "01".repeat(32)
            )
            .as_bytes(),
        )
        .unwrap();
        let authorization = proletarius_ims::aka_authorization::authorize_response(
            &response,
            &proletarius_ims::aka_authorization::AuthorizationRequest {
                username: "234330000000001@ims.mnc033.mcc234.3gppnetwork.org",
                method: "REGISTER",
                uri: "sip:ims.mnc033.mcc234.3gppnetwork.org",
                body: b"",
                cnonce: "native-test-cnonce",
                nonce_count: 1,
            },
            proletarius_ims::aka_digest::AkaPassword::Res(&[7; 8]),
        )
        .unwrap();
        let verify = "ipsec-3gpp;q=0.98;alg=hmac-sha-1-96;ealg=aes-cbc;prot=esp;mod=trans;spi-c=333;spi-s=444;port-c=6064;port-s=6063";
        let wire = profile
            .compile(
                &device_identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::Protected {
                    cseq: 2,
                    endpoints: endpoints(),
                    authorization: &authorization,
                    security_verify: verify,
                    remote_port_s: 6063,
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        assert!(wire.contains("Via: SIP/2.0/TCP [2001:db8::10]:5063;branch=z9hG4bK-native-1\r\n"));
        assert!(wire.contains("Route: <sip:[2001:db8::20]:6063;transport=tcp;lr>\r\n"));
        assert!(wire.contains("Security-Verify: ipsec-3gpp;q=0.98;alg=hmac-sha-1-96;ealg=aes-cbc;prot=esp;mod=trans;spi-c=333;spi-s=444;port-c=6064;port-s=6063\r\n"));
        assert!(wire.contains(
            "Authorization: Digest username=\"234330000000001@ims.mnc033.mcc234.3gppnetwork.org\""
        ));
    }

    #[test]
    fn rejects_isim_udp_and_uninstallable_security_before_network() {
        let sim = sim("234", "33", "234330000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        for yaml in [
            "carrier_overrides:\n  '234/33': {ims_transport: udp}\n",
            "carrier_overrides:\n  '234/33': {ims_register_template: {security_client_mechanisms: [{alg: hmac-md5-96, ealg: des-ede3-cbc}]}}\n",
            "carrier_overrides:\n  '234/33': {ims_register_template: {strict_security_server_offer: false}}\n",
        ] {
            let overrides = crate::carrier_overrides::OverrideSet::parse(yaml.as_bytes()).unwrap();
            assert!(
                carrier
                    .native_ims_register_profile(&sim, overrides.get("234", "33"))
                    .is_err()
            );
        }
    }

    #[test]
    fn isim_identity_source_uses_the_cached_pair_in_register_and_bootstrap_auth() {
        let sim = sim("234", "33", "234330000000001");
        let carrier = Registry::embedded()
            .unwrap()
            .resolve(&sim)
            .unwrap()
            .unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/33': {ims_identity_source: isim}\n",
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        let device_identity = device_identity(&profile);
        let mut runtime = runtime(&sim.imsi, &sim.mcc, &sim.mnc);
        runtime.isim_private_identity = Some("subscriber@ims.example.test");
        runtime.isim_public_identity = Some("sip:+447700900123@ims.example.test");
        let wire = profile
            .compile(
                &device_identity,
                runtime,
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: profile.local_port(),
                    endpoints: endpoints(),
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        assert!(wire.contains("From: <sip:+447700900123@ims.example.test>;tag=from-1\r\n"));
        assert!(wire.contains("To: <sip:+447700900123@ims.example.test>\r\n"));
        assert!(wire.contains("P-Preferred-Identity: <sip:+447700900123@ims.example.test>\r\n"));
        assert!(wire.contains("Authorization: Digest username=\"subscriber@ims.example.test\""));
        assert!(!wire.contains("Authorization: Digest username=\"234330000000001@"));
    }

    #[test]
    fn auto_identity_uses_complete_isim_pair_derives_when_absent_and_rejects_partial_pair() {
        let sim = sim("234", "33", "234330000000001");
        let carrier = Registry::embedded()
            .unwrap()
            .resolve(&sim)
            .unwrap()
            .unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/33': {ims_identity_source: auto}\n",
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        let mut runtime = runtime(&sim.imsi, &sim.mcc, &sim.mnc);
        let derived = profile.resolve_subscriber_identity(runtime).unwrap();
        assert_eq!(
            derived.private_identity(),
            "234330000000001@ims.mnc033.mcc234.3gppnetwork.org"
        );
        runtime.isim_private_identity = Some("subscriber@ims.example.test");
        assert!(matches!(
            profile.resolve_subscriber_identity(runtime),
            Err(NativeImsProfileError::IncompleteIsimIdentity)
        ));
        runtime.isim_public_identity = Some("sip:subscriber@ims.example.test");
        let isim = profile.resolve_subscriber_identity(runtime).unwrap();
        assert_eq!(isim.private_identity(), "subscriber@ims.example.test");
        assert_eq!(isim.public_identity(), "sip:subscriber@ims.example.test");
    }

    #[test]
    fn resolved_subscriber_identity_is_bound_to_its_profile_and_sim() {
        let ctexcel_sim = sim("234", "33", "234330000000001");
        let ctexcel = profile(&ctexcel_sim);
        let resolved = ctexcel
            .resolve_subscriber_identity(runtime(
                &ctexcel_sim.imsi,
                &ctexcel_sim.mcc,
                &ctexcel_sim.mnc,
            ))
            .unwrap();
        assert!(
            ctexcel
                .validate_subscriber_identity(&resolved, &ctexcel_sim.imsi)
                .is_ok()
        );
        assert!(matches!(
            ctexcel.validate_subscriber_identity(&resolved, "234330123456789"),
            Err(NativeImsProfileError::SubscriberIdentityMismatch)
        ));

        let giffgaff_sim = sim("234", "10", "234100000000001");
        let giffgaff = profile(&giffgaff_sim);
        assert!(matches!(
            giffgaff.validate_subscriber_identity(&resolved, &ctexcel_sim.imsi),
            Err(NativeImsProfileError::SubscriberIdentityMismatch)
        ));
    }

    #[test]
    fn rejects_isim_header_injection_and_invalid_identity_schemes() {
        for (private, public) in [
            (
                "subscriber@ims.example.test\r\nInjected: yes",
                "sip:user@ims.example.test",
            ),
            (
                "subscriber@ims.example.test",
                "sip:user@ims.example.test>\r\nInjected: yes",
            ),
            (
                "subscriber@ims.example.test",
                "https://ims.example.test/user",
            ),
        ] {
            assert!(!(valid_impi(private) && valid_impu(public)));
        }
    }

    #[test]
    fn override_values_are_kept_in_the_same_immutable_profile_snapshot() {
        let sim = sim("234", "33", "234330000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            br#"
carrier_overrides:
  "234/33":
    ims_domain: ims.example.test
    ims_realm: realm.example.test
    ims_phone_context: phone.example.test
    ims_pcscf: "2001:db8::20"
    ims_local_port: 5070
    ims_tcp_keepalive_seconds: 45
    ims_options_ping_interval_seconds: 55
    ims_register_template:
      id: ctexcel_native
      expires: 600
      contact_param_order: [access_type, sip_instance, smsip, icsi_ref]
      enable_initial_reject_fallback: true
      register_policy:
        temporary_status_codes: [408, 503]
        forbidden_status_codes: []
        initial_reject_fallback_status_codes: [403]
        temporary_retry_seconds: 60
      security_client_mechanisms:
        - {alg: hmac-sha-1-96, ealg: aes-cbc, prot: esp, mode: trans}
"#,
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        assert_eq!(profile.domain(), "ims.example.test");
        assert_eq!(profile.realm(), "realm.example.test");
        assert_eq!(profile.phone_context(), "phone.example.test");
        assert_eq!(
            profile.configured_pcscf(),
            Some("[2001:db8::20]:5060".parse().unwrap())
        );
        assert_eq!(profile.local_port(), 5070);
        assert_eq!(profile.tcp_keepalive_seconds(), 45);
        assert_eq!(profile.options_ping_interval_seconds(), 55);
        assert_eq!(profile.template().id(), "ctexcel_native");
        assert_eq!(profile.template().expires_seconds(), 600);
        assert!(profile.template().enable_initial_reject_fallback());
        let retry = profile.template().retry_policy();
        assert_eq!(retry.temporary_status_codes(), Some(&[408, 503][..]));
        assert_eq!(retry.forbidden_status_codes(), Some(&[][..]));
        assert_eq!(
            retry.initial_reject_fallback_status_codes(),
            Some(&[403][..])
        );
        assert_eq!(retry.temporary_retry_seconds(), 60);
    }

    #[test]
    fn explicit_pcscf_must_be_used_by_the_runtime() {
        let sim = sim("234", "33", "234330000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/33': {ims_pcscf: '2001:db8::30'}\n",
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "33"))
            .unwrap();
        let device_identity = device_identity(&profile);
        assert!(matches!(
            profile.compile(
                &device_identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: 5060,
                    endpoints: endpoints(),
                },
            ),
            Err(NativeImsProfileError::ConfiguredPcscfMismatch)
        ));
    }

    #[test]
    fn generic_disabled_sec_agree_has_no_bootstrap_or_security_headers() {
        let sim = sim("234", "20", "234200000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/20': {ims_register_template: {sec_agree_mode: disabled}}\n",
        )
        .unwrap();
        let profile = carrier
            .native_ims_register_profile(&sim, overrides.get("234", "20"))
            .unwrap();
        let device_identity = device_identity(&profile);
        let wire = profile
            .compile(
                &device_identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::Initial {
                    cseq: 1,
                    local_port: 5060,
                    endpoints: endpoints(),
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        for forbidden in [
            "Authorization:",
            "Security-Client:",
            "Require: sec-agree",
            "Proxy-Require: sec-agree",
        ] {
            assert!(
                !wire.contains(forbidden),
                "unexpected {forbidden} in {wire}"
            );
        }
    }

    #[test]
    fn auto_plain_register_without_security_server_omits_sec_agree_headers() {
        let sim = sim("234", "10", "234100000000001");
        let mut sim = sim;
        sim.gid1 = "508FFFFF".to_owned();
        let profile = profile(&sim);
        let device_identity = device_identity(&profile);
        assert_eq!(profile.template().sec_agree(), SecAgreeMode::Auto);
        let response = proletarius_ims::SipResponse::parse(
            format!(
                "SIP/2.0 401 Unauthorized\r\nCall-ID: native-register-1\r\nCSeq: 1 REGISTER\r\nWWW-Authenticate: Digest realm=\"ims.mnc010.mcc234.3gppnetwork.org\",nonce=\"{}\",algorithm=AKAv1-MD5\r\nContent-Length: 0\r\n\r\n",
                "01".repeat(32)
            )
            .as_bytes(),
        )
        .unwrap();
        let authorization = proletarius_ims::aka_authorization::authorize_response(
            &response,
            &proletarius_ims::aka_authorization::AuthorizationRequest {
                username: "234100000000001@ims.mnc010.mcc234.3gppnetwork.org",
                method: "REGISTER",
                uri: "sip:ims.mnc010.mcc234.3gppnetwork.org",
                body: b"",
                cnonce: "native-test-cnonce",
                nonce_count: 1,
            },
            proletarius_ims::aka_digest::AkaPassword::Res(&[7; 8]),
        )
        .unwrap();
        let wire = profile
            .compile(
                &device_identity,
                runtime(&sim.imsi, &sim.mcc, &sim.mnc),
                NativeImsRegisterAttempt::PlainAuthenticated {
                    cseq: 2,
                    local_port: 5060,
                    endpoints: endpoints(),
                    authorization: &authorization,
                    security_disposition: PlainSecurityDisposition::OmitOffer,
                },
            )
            .unwrap();
        let wire = String::from_utf8(wire.to_vec()).unwrap();
        assert!(wire.contains(
            "Authorization: Digest username=\"234100000000001@ims.mnc010.mcc234.3gppnetwork.org\""
        ));
        for forbidden in [
            "Security-Client:",
            "Security-Verify:",
            "Require: sec-agree",
            "Proxy-Require: sec-agree",
        ] {
            assert!(
                !wire.contains(forbidden),
                "unexpected {forbidden} in {wire}"
            );
        }
    }

    #[test]
    fn reviewed_uk_templates_reject_contact_overrides_that_break_mt_binding() {
        let sim = sim("234", "33", "234330000000001");
        let registry = Registry::embedded().unwrap();
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let overrides = crate::carrier_overrides::OverrideSet::parse(
            b"carrier_overrides:\n  '234/33': {ims_register_template: {contact_param_order: [audio]}}\n",
        )
        .unwrap();
        assert!(
            carrier
                .native_ims_register_profile(&sim, overrides.get("234", "33"))
                .is_err()
        );
    }
}
