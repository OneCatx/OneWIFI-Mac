//! Offline home-PLMN selection. This layer never resolves DNS or reads hardware.
use serde::Deserialize;
use serde_json::Value;
use std::collections::BTreeMap;
use zeroize::Zeroizing;

#[derive(Default, Deserialize)]
pub struct SimIdentity {
    pub mcc: String,
    pub mnc: String,
    #[serde(default)]
    pub imsi: String,
    #[serde(default)]
    pub iccid: String,
    #[serde(default)]
    pub gid1: String,
    #[serde(default)]
    pub gid2: String,
    #[serde(default)]
    pub spn: String,
}

#[derive(Deserialize)]
pub struct Registry {
    schema_version: u32,
    pub revision: String,
    selectors: BTreeMap<String, Manifest>,
    profiles: BTreeMap<String, Value>,
}
#[derive(Deserialize)]
struct Manifest {
    plmn: String,
    profiles: Vec<Candidate>,
}
#[derive(Deserialize)]
struct Candidate {
    id: String,
    path: String,
    #[serde(default)]
    generic: bool,
    #[serde(default)]
    selectors: Vec<Selector>,
}
#[derive(Deserialize)]
struct Selector {
    raw: String,
    #[serde(default)]
    conditions: Vec<Condition>,
}
#[derive(Deserialize)]
struct Condition {
    field: String,
    #[serde(rename = "match")]
    kind: String,
    value: String,
}

pub struct ResolvedCarrier {
    pub id: String,
    pub bundle: String,
    pub selector: String,
    pub configuration: Value,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NativeAlgorithmProposals {
    pub ike: Vec<String>,
    pub esp: Vec<String>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct NativeTunnelOverrides {
    pub epdg: Option<String>,
    pub epdg_port: Option<u16>,
    pub apn: Option<String>,
    pub ip_stack: Option<String>,
    pub ike_proposals: Option<Vec<String>>,
    pub esp_proposals: Option<Vec<String>>,
    pub aka_challenge_mode: Option<String>,
}

/// Immutable carrier-derived inputs for one native IKE session. Subscriber
/// identity is zeroized and deliberately has no Debug/Serialize surface.
pub struct NativeCarrierTunnelConfig {
    pub epdg: String,
    pub epdg_port: u16,
    pub epdg_source: &'static str,
    pub apn: String,
    pub ip_stack: crate::RequestedIpStack,
    pub proposals: NativeAlgorithmProposals,
    pub aka_challenge_mode: proletarius_ike::eap_aka::AkaCheckcodeMode,
    identity: Zeroizing<String>,
}

impl NativeCarrierTunnelConfig {
    pub fn identity(&self) -> &[u8] {
        self.identity.as_bytes()
    }
}

impl ResolvedCarrier {
    pub fn native_tunnel_config(
        &self,
        sim: &SimIdentity,
        overrides: &NativeTunnelOverrides,
    ) -> Result<NativeCarrierTunnelConfig, String> {
        if sim.imsi.is_empty() || !sim.imsi.bytes().all(|byte| byte.is_ascii_digit()) {
            return Err("native tunnel requires a numeric IMSI".into());
        }
        let (epdg, epdg_source) = if let Some(epdg) = nonempty_override(&overrides.epdg)? {
            (epdg.to_owned(), "custom")
        } else if let Some(address) = template_epdg_address(&self.configuration)? {
            (address.to_owned(), "carrier-template")
        } else {
            (
                format!(
                    "epdg.epc.mnc{}.mcc{}.pub.3gppnetwork.org",
                    pad3(&sim.mnc),
                    pad3(&sim.mcc)
                ),
                "3gpp-standard",
            )
        };
        if !valid_domain(&epdg) {
            return Err("native tunnel ePDG is not a valid hostname".into());
        }
        let apn = match nonempty_override(&overrides.apn)? {
            Some(value) => value.to_owned(),
            None => optional_string(&self.configuration, "/ims/apn")?
                .unwrap_or("ims")
                .to_owned(),
        };
        if !valid_apn(&apn) {
            return Err("native tunnel APN is invalid".into());
        }
        let ip_stack = match nonempty_override(&overrides.ip_stack)? {
            Some(value) => value,
            None => optional_string(&self.configuration, "/ims/ip_stack")?.unwrap_or("ipv4v6"),
        };
        let ip_stack = match ip_stack.to_ascii_lowercase().as_str() {
            "ipv4" => crate::RequestedIpStack::Ipv4,
            "ipv6" => crate::RequestedIpStack::Ipv6,
            "ipv4v6" => crate::RequestedIpStack::Dual,
            _ => return Err("native tunnel ip_stack is invalid".into()),
        };
        let template = optional_string(&self.configuration, "/ims/ike/local_identifier")?
            .unwrap_or("0$imsi@nai.epc.mnc$mnc.mcc$mcc.3gppnetwork.org");
        let identity = template
            .replace("$imsi", &sim.imsi)
            .replace("$mcc", &pad3(&sim.mcc))
            .replace("$mnc", &pad3(&sim.mnc));
        if identity.is_empty() || identity.len() > 255 || identity.contains('\0') {
            return Err("native tunnel identity is invalid".into());
        }
        let mut proposals = self.native_algorithm_proposals()?;
        if let Some(ike) = &overrides.ike_proposals {
            if ike.is_empty() {
                return Err("native tunnel explicit IKE proposals are empty".into());
            }
            proletarius_ike::build_native_proposals(
                ike,
                proletarius_ike::ProposalProtocol::Ike,
                &[],
            )
            .map_err(|error| format!("unsupported native IKE proposal: {error:?}"))?;
            proposals.ike.clone_from(ike);
        }
        if let Some(esp) = &overrides.esp_proposals {
            if esp.is_empty() {
                return Err("native tunnel explicit ESP proposals are empty".into());
            }
            proletarius_ike::build_native_proposals(
                esp,
                proletarius_ike::ProposalProtocol::Esp,
                &[0, 0, 0, 1],
            )
            .map_err(|error| format!("unsupported native ESP proposal: {error:?}"))?;
            proposals.esp.clone_from(esp);
        }
        let challenge_mode = nonempty_override(&overrides.aka_challenge_mode)?
            .unwrap_or("minimal")
            .to_ascii_lowercase();
        let aka_challenge_mode = match challenge_mode.as_str() {
            "minimal" => proletarius_ike::eap_aka::AkaCheckcodeMode::Minimal,
            "omit" => proletarius_ike::eap_aka::AkaCheckcodeMode::Omit,
            "checkcode" => proletarius_ike::eap_aka::AkaCheckcodeMode::Checkcode,
            "recompute" => proletarius_ike::eap_aka::AkaCheckcodeMode::Recompute,
            _ => return Err("native tunnel aka_challenge_mode is invalid".into()),
        };
        let epdg_port = overrides.epdg_port.unwrap_or(500);
        if epdg_port == 0 {
            return Err("native tunnel ePDG port is invalid".into());
        }
        Ok(NativeCarrierTunnelConfig {
            epdg,
            epdg_port,
            epdg_source,
            apn,
            ip_stack,
            proposals,
            aka_challenge_mode,
            identity: Zeroizing::new(identity),
        })
    }

    /// Converts the selected Apple carrier template into an exact, non-empty
    /// native proposal list. Missing proposal arrays use the narrow standard
    /// profile defaults applied by the Go carrier expander; malformed or
    /// multi-valued transform alternatives remain fail-closed.
    pub fn native_algorithm_proposals(&self) -> Result<NativeAlgorithmProposals, String> {
        let ike = match optional_proposal_objects(&self.configuration, "/ims/ike/proposals")? {
            Some(proposals) => proposals
                .iter()
                .map(|proposal| {
                    let encryption = single_token(proposal, "encryption")?;
                    let integrity = optional_single_token(proposal, "integrity")?;
                    let prf = single_token(proposal, "prf")?;
                    let group = single_number(proposal, "dh_groups")?;
                    let mut tokens = vec![encryption];
                    if let Some(integrity) = integrity {
                        tokens.push(integrity);
                    }
                    tokens.push(if prf.starts_with("prf") {
                        prf
                    } else {
                        format!("prf{prf}")
                    });
                    tokens.push(match group {
                        14 => "modp2048".into(),
                        other => {
                            return Err(format!("native IKE does not support DH group {other}"));
                        }
                    });
                    Ok(tokens.join("-"))
                })
                .collect::<Result<Vec<_>, String>>()?,
            None => vec!["aes128-sha256-prfsha256-modp2048".into()],
        };
        let esp =
            match optional_proposal_objects(&self.configuration, "/ims/ike/child_sa/proposals")? {
                Some(proposals) => proposals
                    .iter()
                    .map(|proposal| {
                        if proposal
                            .get("esn")
                            .and_then(Value::as_str)
                            .is_some_and(|value| !matches!(value, "auto" | "no" | "none" | "noesn"))
                        {
                            return Err(
                                "native initial CHILD SA does not support required ESN".into()
                            );
                        }
                        let mut tokens = vec![single_token(proposal, "encryption")?];
                        if let Some(integrity) = optional_single_token(proposal, "integrity")? {
                            tokens.push(integrity);
                        }
                        Ok(tokens.join("-"))
                    })
                    .collect::<Result<Vec<_>, String>>()?,
                None => vec!["aes128-sha256".into()],
            };
        proletarius_ike::build_native_proposals(&ike, proletarius_ike::ProposalProtocol::Ike, &[])
            .map_err(|error| format!("unsupported native IKE proposal: {error:?}"))?;
        proletarius_ike::build_native_proposals(
            &esp,
            proletarius_ike::ProposalProtocol::Esp,
            &[0, 0, 0, 1],
        )
        .map_err(|error| format!("unsupported native ESP proposal: {error:?}"))?;
        Ok(NativeAlgorithmProposals { ike, esp })
    }
}

fn nonempty_override(value: &Option<String>) -> Result<Option<&str>, String> {
    match value {
        None => Ok(None),
        Some(value) if value.trim().is_empty() => Err("native tunnel override is empty".into()),
        Some(value) => Ok(Some(value.trim())),
    }
}

fn optional_string<'a>(configuration: &'a Value, pointer: &str) -> Result<Option<&'a str>, String> {
    match configuration.pointer(pointer) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if !value.trim().is_empty() => Ok(Some(value.trim())),
        Some(_) => Err(format!("carrier template has an invalid {pointer}")),
    }
}

/// Resolves the carrier-published ePDG forms in the same order as the Go
/// bridge. `address` is the modern direct field; some older bundles only have
/// a candidate array or a certificate hostname. No DNS happens here.
fn template_epdg_address(configuration: &Value) -> Result<Option<&str>, String> {
    if let Some(address) = optional_epdg_string(configuration, "/ims/epdg/address")? {
        return Ok(Some(address));
    }
    if let Some(candidate) = first_epdg_candidate(configuration)? {
        return Ok(Some(candidate));
    }
    optional_epdg_string(configuration, "/ims/epdg/remote_certificate_hostname")
}

fn optional_epdg_string<'a>(
    configuration: &'a Value,
    pointer: &str,
) -> Result<Option<&'a str>, String> {
    match configuration.pointer(pointer) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value.trim()).filter(|value| !value.is_empty())),
        Some(_) => Err(format!("carrier template has an invalid {pointer}")),
    }
}

fn first_epdg_candidate(configuration: &Value) -> Result<Option<&str>, String> {
    match configuration.pointer("/ims/epdg/candidates") {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.is_empty() => Ok(None),
        Some(Value::Array(values)) => match values[0].get("address") {
            None | Some(Value::Null) => Ok(None),
            Some(Value::String(value)) => Ok(Some(value.trim()).filter(|value| !value.is_empty())),
            Some(_) => Err("carrier template has an invalid first ePDG candidate".into()),
        },
        Some(_) => Err("carrier template has invalid ePDG candidates".into()),
    }
}

fn pad3(value: &str) -> String {
    format!("{value:0>3}")
}

fn valid_domain(value: &str) -> bool {
    let value = value.trim().trim_end_matches('.');
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

fn valid_apn(value: &str) -> bool {
    value.len() <= 100
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

fn optional_proposal_objects<'a>(
    configuration: &'a Value,
    pointer: &str,
) -> Result<Option<&'a [Value]>, String> {
    match configuration.pointer(pointer) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(items)) if items.is_empty() => Ok(None),
        Some(Value::Array(items)) => Ok(Some(items.as_slice())),
        Some(_) => Err(format!("carrier template has an invalid {pointer}")),
    }
}

fn single_token(proposal: &Value, key: &str) -> Result<String, String> {
    let values = proposal
        .get(key)
        .and_then(Value::as_array)
        .filter(|values| values.len() == 1)
        .ok_or_else(|| format!("carrier proposal requires exactly one {key} transform"))?;
    values[0]
        .as_str()
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_ascii_lowercase)
        .ok_or_else(|| format!("carrier proposal has an invalid {key} transform"))
}

fn optional_single_token(proposal: &Value, key: &str) -> Result<Option<String>, String> {
    match proposal.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) if values.is_empty() => Ok(None),
        Some(_) => single_token(proposal, key).map(Some),
    }
}

fn single_number(proposal: &Value, key: &str) -> Result<u64, String> {
    let values = proposal
        .get(key)
        .and_then(Value::as_array)
        .filter(|values| values.len() == 1)
        .ok_or_else(|| format!("carrier proposal requires exactly one {key}"))?;
    values[0]
        .as_u64()
        .ok_or_else(|| format!("carrier proposal has an invalid {key}"))
}

impl Registry {
    pub fn embedded() -> Result<Self, String> {
        let registry: Self =
            serde_json::from_str(include_str!("../../../carrier-registry.json"))
                .map_err(|error| format!("invalid embedded carrier registry: {error}"))?;
        if registry.schema_version != 1 {
            return Err("unsupported carrier registry schema".into());
        }
        Ok(registry)
    }

    pub fn resolve(&self, sim: &SimIdentity) -> Result<Option<ResolvedCarrier>, String> {
        if sim.mcc.len() != 3
            || !(2..=3).contains(&sim.mnc.len())
            || !sim
                .mcc
                .bytes()
                .chain(sim.mnc.bytes())
                .all(|b| b.is_ascii_digit())
        {
            return Err("home MCC/MNC must retain their original numeric widths".into());
        }
        let plmn = format!("{}{}", sim.mcc, sim.mnc);
        let Some(manifest) = self.selectors.get(&plmn) else {
            return Ok(None);
        };
        if manifest.plmn != plmn {
            return Err("carrier manifest PLMN mismatch".into());
        }
        let mut matches = Vec::new();
        let mut generic = BTreeMap::new();
        for candidate in &manifest.profiles {
            if candidate.generic {
                generic.insert(&candidate.id, (candidate, ""));
            }
            for selector in &candidate.selectors {
                if selector.conditions.is_empty() {
                    generic
                        .entry(&candidate.id)
                        .or_insert((candidate, selector.raw.as_str()));
                } else if let Some(score) = selector.score(sim) {
                    matches.push((score, candidate, selector.raw.as_str()));
                }
            }
        }
        let selected = if let Some(best) = matches.iter().map(|m| m.0).max() {
            let mut top = matches.iter().filter(|m| m.0 == best);
            let (_, candidate, raw) = top.next().expect("maximum came from matches");
            if top.any(|(_, other, other_raw)| other.id != candidate.id || other_raw != raw) {
                return Err("ambiguous carrier selectors; refusing to guess".into());
            }
            (*candidate, *raw)
        } else {
            if generic.len() > 1 {
                return Err("ambiguous generic carrier profiles".into());
            }
            let Some((candidate, raw)) = generic.into_values().next() else {
                return Ok(None);
            };
            (candidate, raw)
        };
        let (candidate, raw) = selected;
        let key = candidate
            .path
            .strip_prefix("profiles/")
            .and_then(|p| p.strip_suffix(".yaml"))
            .filter(|p| !p.contains('/') && !p.contains(".."))
            .ok_or("invalid carrier profile path")?;
        let document = self.profiles.get(key).ok_or("carrier profile missing")?;
        if document["id"].as_str() != Some(candidate.id.as_str())
            || !document["supported_plmns"]
                .as_array()
                .is_some_and(|values| values.iter().any(|v| v.as_str() == Some(&plmn)))
        {
            return Err("carrier profile identity mismatch".into());
        }
        let mut configuration = document["common"].clone();
        if configuration.is_null() {
            configuration = Value::Object(Default::default());
        }
        if !configuration.is_object() {
            return Err("carrier common configuration must be an object".into());
        }
        merge(&mut configuration, &document["plmn_overrides"][&plmn]);
        if !raw.is_empty() {
            merge(&mut configuration, &document["selector_overrides"][raw]);
        }
        Ok(Some(ResolvedCarrier {
            id: candidate.id.clone(),
            bundle: document["bundle"].as_str().unwrap_or_default().into(),
            selector: raw.into(),
            configuration,
        }))
    }
}

impl Selector {
    fn score(&self, sim: &SimIdentity) -> Option<(usize, usize, usize)> {
        let mut exact = 0;
        let mut length = 0;
        for condition in &self.conditions {
            let actual = match condition.field.as_str() {
                "imsi" => &sim.imsi,
                "iccid" => &sim.iccid,
                "gid1" => &sim.gid1,
                "gid2" => &sim.gid2,
                "spn" => &sim.spn,
                _ => return None,
            };
            let normalize = |value: &str| -> Option<String> {
                let value = value.trim();
                if value.is_empty() {
                    return None;
                }
                match condition.field.as_str() {
                    "gid1" | "gid2" if !value.bytes().all(|b| b.is_ascii_hexdigit()) => None,
                    "imsi" | "iccid" if !value.bytes().all(|b| b.is_ascii_digit()) => None,
                    _ => Some(value.to_lowercase()),
                }
            };
            let actual = normalize(actual)?;
            let expected = normalize(&condition.value)?;
            match condition.kind.as_str() {
                "exact" | "exact_casefold" if actual == expected => exact += 1,
                "prefix" if actual.starts_with(&expected) => (),
                _ => return None,
            }
            length += expected.len();
        }
        Some((self.conditions.len(), exact, length))
    }
}

fn merge(target: &mut Value, overlay: &Value) {
    if let (Some(target), Some(overlay)) = (target.as_object_mut(), overlay.as_object()) {
        for (key, value) in overlay {
            if value.is_object() && target.get(key).is_some_and(Value::is_object) {
                merge(target.get_mut(key).unwrap(), value);
            } else {
                target.insert(key.clone(), value.clone());
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn epdg_fixture(epdg: Value) -> ResolvedCarrier {
        ResolvedCarrier {
            id: "fixture".into(),
            bundle: "Fixture.bundle".into(),
            selector: "fixture".into(),
            configuration: serde_json::json!({"ims": {"epdg": epdg}}),
        }
    }

    fn epdg_fixture_sim() -> SimIdentity {
        SimIdentity {
            mcc: "234".into(),
            mnc: "33".into(),
            imsi: "234330123456789".into(),
            ..Default::default()
        }
    }

    #[test]
    fn pinned_registry_matches_go_synthetic_selection_cases() {
        #[derive(Deserialize)]
        struct Case {
            sim: SimIdentity,
            id: String,
            selector: String,
            rejected: bool,
        }
        let cases: Vec<Case> =
            serde_json::from_str(include_str!("../../../carrier-cases.json")).unwrap();
        assert!(cases.len() >= 853);
        let registry = Registry::embedded().unwrap();
        let mut mismatches = Vec::new();
        for (index, case) in cases.iter().enumerate() {
            let matched = match registry.resolve(&case.sim) {
                Err(_) => case.rejected,
                Ok(None) => !case.rejected && case.id.is_empty(),
                Ok(Some(profile)) => {
                    !case.rejected && profile.id == case.id && profile.selector == case.selector
                }
            };
            if !matched {
                mismatches.push(index);
            }
        }
        assert!(
            mismatches.is_empty(),
            "selection mismatch at synthetic case indices {mismatches:?}"
        );
    }
    #[test]
    fn loads_every_pinned_template_and_resolves_giffgaff() {
        let registry = Registry::embedded().unwrap();
        assert_eq!(registry.selectors.len(), 853);
        assert_eq!(registry.profiles.len(), 683);
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "10".into(),
            gid1: "508fffff00".into(),
            ..Default::default()
        };
        let profile = registry.resolve(&sim).unwrap().unwrap();
        assert_eq!(profile.id, "apple_o2_giffgaff_uk");
        assert_eq!(
            profile.native_algorithm_proposals().unwrap(),
            NativeAlgorithmProposals {
                ike: vec!["aes256-sha512-prfsha512-modp2048".into()],
                esp: vec!["aes256-sha512".into()],
            }
        );
    }

    #[test]
    fn gid2_selector_outranks_the_matching_generic_bundle() {
        let registry = Registry::embedded().unwrap();
        let sim = SimIdentity {
            mcc: "204".into(),
            mnc: "04".into(),
            iccid: "8914801234567890123".into(),
            gid2: "A7FF".into(),
            ..Default::default()
        };
        let profile = registry.resolve(&sim).unwrap().unwrap();
        assert_eq!(profile.id, "apple_verizon_charter_lte_us");
        assert_eq!(profile.selector, "20404_GID2-A7_ID-891480");
    }
    #[test]
    fn missing_gid_does_not_guess_mvno_from_name() {
        let registry = Registry::embedded().unwrap();
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "10".into(),
            spn: "giffgaff".into(),
            ..Default::default()
        };
        assert_ne!(
            registry.resolve(&sim).unwrap().unwrap().id,
            "apple_o2_giffgaff_uk"
        );
    }
    #[test]
    fn equally_specific_profiles_fail_instead_of_selecting_first() {
        let mut registry = Registry::embedded().unwrap();
        registry
            .selectors
            .get_mut("23410")
            .unwrap()
            .profiles
            .push(Candidate {
                id: "conflicting_fixture".into(),
                path: "profiles/fixture.yaml".into(),
                generic: false,
                selectors: vec![Selector {
                    raw: "fixture".into(),
                    conditions: vec![Condition {
                        field: "gid1".into(),
                        kind: "prefix".into(),
                        value: "508FFFFF".into(),
                    }],
                }],
            });
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "10".into(),
            gid1: "508FFFFF00".into(),
            ..Default::default()
        };
        assert!(registry.resolve(&sim).err().unwrap().contains("ambiguous"));
    }

    #[test]
    fn overlay_merges_objects_but_replaces_arrays() {
        let mut common =
            serde_json::json!({"ims":{"apn":"ims","ike":{"proposals":[1,2],"timer":20}}});
        merge(
            &mut common,
            &serde_json::json!({"ims":{"ike":{"proposals":[3]}}}),
        );
        assert_eq!(
            common,
            serde_json::json!({"ims":{"apn":"ims","ike":{"proposals":[3],"timer":20}}})
        );
    }

    #[test]
    fn ctexcel_loads_plmn_only_configuration() {
        let registry = Registry::embedded().unwrap();
        let result = registry
            .resolve(&SimIdentity {
                mcc: "234".into(),
                mnc: "33".into(),
                ..Default::default()
            })
            .unwrap()
            .unwrap();
        assert_eq!(result.configuration["ims"]["ip_stack"], "ipv6");
        assert_eq!(
            result.configuration["ims"]["ike"]["dpd"]["interval_seconds"],
            600
        );
        assert_eq!(
            result.native_algorithm_proposals().unwrap(),
            NativeAlgorithmProposals {
                ike: vec!["aes128-sha256-prfsha256-modp2048".into()],
                esp: vec!["aes128-sha256".into()],
            }
        );
        let config = result
            .native_tunnel_config(
                &SimIdentity {
                    mcc: "234".into(),
                    mnc: "33".into(),
                    imsi: "234330123456789".into(),
                    ..Default::default()
                },
                &NativeTunnelOverrides::default(),
            )
            .unwrap();
        assert_eq!(config.epdg, "epdg.epc.mnc033.mcc234.pub.3gppnetwork.org");
        assert_eq!(config.epdg_source, "3gpp-standard");
        assert_eq!(config.apn, "ims");
        assert_eq!(config.ip_stack, crate::RequestedIpStack::Ipv6);
        assert_eq!(
            config.identity(),
            b"0234330123456789@nai.epc.mnc033.mcc234.3gppnetwork.org"
        );
    }

    #[test]
    fn native_tunnel_overrides_are_exact_and_fail_closed() {
        let registry = Registry::embedded().unwrap();
        let sim = SimIdentity {
            mcc: "234".into(),
            mnc: "33".into(),
            imsi: "234330123456789".into(),
            ..Default::default()
        };
        let carrier = registry.resolve(&sim).unwrap().unwrap();
        let config = carrier
            .native_tunnel_config(
                &sim,
                &NativeTunnelOverrides {
                    epdg: Some("custom.epdg.example".into()),
                    epdg_port: Some(4500),
                    apn: Some("ims.private".into()),
                    ip_stack: Some("ipv4".into()),
                    ike_proposals: Some(vec!["aes128-sha256-prfsha256-modp2048".into()]),
                    esp_proposals: Some(vec!["aes128-sha256".into()]),
                    aka_challenge_mode: Some("checkcode".into()),
                },
            )
            .unwrap();
        assert_eq!(config.epdg, "custom.epdg.example");
        assert_eq!(config.epdg_source, "custom");
        assert_eq!(config.epdg_port, 4500);
        assert_eq!(config.apn, "ims.private");
        assert_eq!(config.ip_stack, crate::RequestedIpStack::Ipv4);
        assert_eq!(
            config.aka_challenge_mode,
            proletarius_ike::eap_aka::AkaCheckcodeMode::Checkcode
        );

        for overrides in [
            NativeTunnelOverrides {
                epdg: Some("https://epdg.example".into()),
                ..Default::default()
            },
            NativeTunnelOverrides {
                epdg_port: Some(0),
                ..Default::default()
            },
            NativeTunnelOverrides {
                ike_proposals: Some(vec![]),
                ..Default::default()
            },
            NativeTunnelOverrides {
                ip_stack: Some("dual".into()),
                ..Default::default()
            },
        ] {
            assert!(carrier.native_tunnel_config(&sim, &overrides).is_err());
        }
    }

    #[test]
    fn native_tunnel_prefers_direct_epdg_address_over_legacy_forms() {
        let sim = epdg_fixture_sim();
        let config = epdg_fixture(serde_json::json!({
            "address": "direct.epdg.example",
            "candidates": [
                {"address": "first.candidate.example"},
                {"address": "second.candidate.example"}
            ],
            "remote_certificate_hostname": "legacy.epdg.example"
        }))
        .native_tunnel_config(&sim, &NativeTunnelOverrides::default())
        .unwrap();
        assert_eq!(config.epdg, "direct.epdg.example");
        assert_eq!(config.epdg_source, "carrier-template");
    }

    #[test]
    fn native_tunnel_uses_first_epdg_candidate_before_legacy_hostname() {
        let sim = epdg_fixture_sim();
        let config = epdg_fixture(serde_json::json!({
            "candidates": [
                {"address": "first.candidate.example"},
                {"address": "second.candidate.example"}
            ],
            "remote_certificate_hostname": "legacy.epdg.example"
        }))
        .native_tunnel_config(&sim, &NativeTunnelOverrides::default())
        .unwrap();
        assert_eq!(config.epdg, "first.candidate.example");
        assert_eq!(config.epdg_source, "carrier-template");
    }

    #[test]
    fn native_tunnel_uses_legacy_remote_certificate_hostname() {
        let sim = epdg_fixture_sim();
        let config = epdg_fixture(serde_json::json!({
            "remote_certificate_hostname": "legacy.epdg.example"
        }))
        .native_tunnel_config(&sim, &NativeTunnelOverrides::default())
        .unwrap();
        assert_eq!(config.epdg, "legacy.epdg.example");
        assert_eq!(config.epdg_source, "carrier-template");
    }

    #[test]
    fn native_tunnel_custom_epdg_wins_over_every_carrier_form() {
        let sim = epdg_fixture_sim();
        let config = epdg_fixture(serde_json::json!({
            "address": "direct.epdg.example",
            "candidates": [{"address": "first.candidate.example"}],
            "remote_certificate_hostname": "legacy.epdg.example"
        }))
        .native_tunnel_config(
            &sim,
            &NativeTunnelOverrides {
                epdg: Some("custom.epdg.example".into()),
                ..Default::default()
            },
        )
        .unwrap();
        assert_eq!(config.epdg, "custom.epdg.example");
        assert_eq!(config.epdg_source, "custom");
    }

    #[test]
    fn native_tunnel_preserves_verizon_style_nonstandard_epdg_host() {
        let sim = epdg_fixture_sim();
        let config = epdg_fixture(serde_json::json!({
            "remote_certificate_hostname": "wo.vzwwo.com"
        }))
        .native_tunnel_config(&sim, &NativeTunnelOverrides::default())
        .unwrap();
        assert_eq!(config.epdg, "wo.vzwwo.com");
        assert_eq!(config.epdg_source, "carrier-template");
    }

    #[test]
    fn hutchison_uses_standard_ike_default_without_weakening_explicit_child_sa() {
        let registry = Registry::embedded().unwrap();
        let result = registry
            .resolve(&SimIdentity {
                mcc: "234".into(),
                mnc: "20".into(),
                ..Default::default()
            })
            .unwrap()
            .unwrap();
        assert_eq!(result.id, "apple_hutchison_uk");
        assert!(result.configuration.pointer("/ims/ike/proposals").is_none());
        assert_eq!(
            result.native_algorithm_proposals().unwrap(),
            NativeAlgorithmProposals {
                ike: vec!["aes128-sha256-prfsha256-modp2048".into()],
                esp: vec!["aes128-sha256".into()],
            }
        );
    }

    #[test]
    fn malformed_proposal_container_does_not_fall_back_to_defaults() {
        let carrier = ResolvedCarrier {
            id: "fixture".into(),
            bundle: String::new(),
            selector: String::new(),
            configuration: serde_json::json!({
                "ims": {"ike": {"proposals": "aes128", "child_sa": {"proposals": []}}}
            }),
        };
        assert!(
            carrier
                .native_algorithm_proposals()
                .unwrap_err()
                .contains("invalid /ims/ike/proposals")
        );
    }

    #[test]
    fn home_plmn_width_is_not_silently_rewritten() {
        let registry = Registry::embedded().unwrap();
        assert!(
            registry
                .resolve(&SimIdentity {
                    mcc: "23-4".into(),
                    mnc: "10".into(),
                    ..Default::default()
                })
                .is_err()
        );
    }
}
