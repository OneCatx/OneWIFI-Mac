//! Profile-scoped durable catalog, inbox and outbox. Only this Rust module
//! writes the snapshot; all related indexes commit in the same atomic file.
use std::collections::BTreeMap;
use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};

use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use proletarius_sms::stable_digest_hex;
use proletarius_store::{ArchiveStore, StoreError};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use subtle::ConstantTimeEq;

pub(crate) const INTERNAL_PREFIX: &str = "/api/internal/multistandby/store/";
pub(crate) const INTERNAL_HEADER: &str = "x-switch-internal-token";
const MAX_BODY: usize = 1024 * 1024;
const MAX_TEXT: usize = 64 * 1024;
const MAX_EVENTS: usize = 100_000;
const PAGE_SIZE: usize = 100;
const MULTIPART_WINDOW_MS: u64 = 120_000;

#[derive(Clone)]
pub(crate) struct Store(ArchiveStore);

#[derive(Default, Deserialize, Serialize)]
struct Snapshot {
    #[serde(default = "schema_version")]
    schema_version: u16,
    revision: u64,
    #[serde(default)]
    profiles: BTreeMap<String, Value>,
    #[serde(default)]
    desired: BTreeMap<String, Desired>,
    #[serde(default)]
    messages: BTreeMap<String, Value>,
    #[serde(default)]
    events: BTreeMap<String, Event>,
    #[serde(default)]
    requests: BTreeMap<String, Event>,
    #[serde(default)]
    inbound_dedup: BTreeMap<String, Event>,
    #[serde(default)]
    changes: Vec<Value>,
}
fn schema_version() -> u16 {
    1
}

#[derive(Clone, Deserialize, Serialize)]
struct Desired {
    desired_state: String,
    revision: u64,
}
#[derive(Clone, Deserialize, Serialize)]
struct Event {
    hash: String,
    response: Value,
}

fn failure(code: &str) -> StoreError {
    StoreError::Configuration(code.to_owned())
}
fn invalid() -> StoreError {
    failure("invalid_request")
}
fn conflict() -> StoreError {
    failure("state_conflict")
}
fn id(value: &Value, key: &str) -> Result<String, StoreError> {
    let text = value.get(key).and_then(Value::as_str).ok_or_else(invalid)?;
    if text.is_empty()
        || text.len() > 128
        || text.trim() != text
        || text.chars().any(char::is_control)
    {
        return Err(invalid());
    }
    Ok(text.to_owned())
}
fn text(value: &Value, key: &str, limit: usize) -> Result<String, StoreError> {
    let text = value.get(key).and_then(Value::as_str).ok_or_else(invalid)?;
    if text.len() > limit {
        return Err(invalid());
    }
    Ok(text.to_owned())
}
fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .min(u128::from(u64::MAX)) as u64
}
fn digest(value: &Value) -> Result<String, StoreError> {
    Ok(stable_digest_hex(
        &serde_json::to_vec(value).map_err(StoreError::Json)?,
    ))
}
fn scope(profile: &str, item: &str) -> String {
    format!("{profile}\0{item}")
}
fn read_snapshot(items: &[Value]) -> Result<Snapshot, StoreError> {
    if items.is_empty() {
        return Ok(Snapshot {
            schema_version: 1,
            ..Default::default()
        });
    }
    if items.len() != 1 {
        return Err(failure("invalid_store_schema"));
    }
    let state: Snapshot = serde_json::from_value(items[0].clone()).map_err(StoreError::Json)?;
    if state.schema_version != 1 {
        return Err(failure("invalid_store_schema"));
    }
    Ok(state)
}
fn persist_snapshot(items: &mut Vec<Value>, state: Snapshot) -> Result<(), StoreError> {
    *items = vec![serde_json::to_value(state).map_err(StoreError::Json)?];
    Ok(())
}
fn next_revision(state: &mut Snapshot) -> Result<u64, StoreError> {
    state.revision = state
        .revision
        .checked_add(1)
        .ok_or_else(|| failure("store_capacity"))?;
    Ok(state.revision)
}
fn change(state: &mut Snapshot, profile: &str, message: &str) {
    let entry = state.messages.get(&scope(profile, message));
    state.changes.push(json!({"revision":state.revision,"profile_id":profile,"message_id":message,"direction":entry.map(|m|m["direction"].clone()),"kind":"message","state":entry.map(|m|m["state"].clone()),"deleted":entry.map(|m|m["deleted"].clone())}));
}
fn require_profile(state: &Snapshot, profile: &str, live: bool) -> Result<(), StoreError> {
    let catalog = state
        .profiles
        .get(profile)
        .ok_or_else(|| failure("profile_not_found"))?;
    if live && catalog["tombstone"].as_bool().unwrap_or(false) {
        return Err(failure("profile_deleted"));
    }
    Ok(())
}

impl Store {
    pub(crate) fn open(root: &Path) -> Result<Self, StoreError> {
        Ok(Self(ArchiveStore::with_limits(
            root.join("multistandby").join("store-v1.json"),
            64 * 1024 * 1024,
            1,
        )?))
    }

    /// A previous process may have crossed the network boundary after claiming.
    /// Never automatically resubmit these intents following process restart.
    pub(crate) fn recover(&self) -> Result<(), StoreError> {
        self.0.mutate(|items| {
            let mut state = read_snapshot(items)?;
            let keys: Vec<_> = state
                .messages
                .iter()
                .filter(|(_, m)| m["state"] == "submitting")
                .map(|(key, _)| key.clone())
                .collect();
            for key in keys {
                let rev = next_revision(&mut state)?;
                let message = state.messages.get_mut(&key).expect("collected message");
                message["state"] = json!("unknown");
                message["revision"] = json!(rev);
                message["error"] = json!("backend_restarted_during_submission");
                let profile = message["profile_id"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned();
                let message_id = message["message_id"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned();
                change(&mut state, &profile, &message_id);
            }
            persist_snapshot(items, state)
        })
    }

    pub(crate) fn apply(&self, action: &str, body: Value) -> Result<Value, StoreError> {
        let event_id = id(&body, "event_id")?;
        let hash = digest(&json!({"action":action,"body":body}))?;
        self.0.mutate(|items| {
            let mut state = read_snapshot(items)?;
            if let Some(prior) = state.events.get(&event_id) {
                if prior.hash != hash {
                    return Err(conflict());
                }
                // Cached claim success cannot authorize a new network submission
                // after process recovery or after the desired-state fence moved.
                if action == "claim" {
                    let profile = id(&body, "profile_id")?;
                    let message = id(&body, "message_id")?;
                    let attempt = id(&body, "attempt_token")?;
                    let current = state
                        .messages
                        .get(&scope(&profile, &message))
                        .ok_or_else(conflict)?;
                    let desired = state.desired.get(&profile).ok_or_else(conflict)?;
                    if current["state"] != "submitting"
                        || current["attempt_token"] != attempt
                        || desired.desired_state != "keep_online"
                        || body
                            .get("expected_revision")
                            .is_some_and(|v| v.as_u64() != Some(desired.revision))
                    {
                        return Err(conflict());
                    }
                }
                return Ok(prior.response.clone());
            }
            if state.events.len() >= MAX_EVENTS {
                return Err(failure("store_capacity"));
            }
            let response = self.apply_new(&mut state, action, &body)?;
            state.events.insert(
                event_id,
                Event {
                    hash,
                    response: response.clone(),
                },
            );
            persist_snapshot(items, state)?;
            Ok(response)
        })
    }

    fn apply_new(
        &self,
        state: &mut Snapshot,
        action: &str,
        body: &Value,
    ) -> Result<Value, StoreError> {
        if action == "catalog" {
            let profiles = body["profiles"].as_array().ok_or_else(invalid)?;
            if profiles.len() > 4096 {
                return Err(invalid());
            }
            for profile in profiles {
                let profile_id = id(profile, "profile_id")?;
                id(profile, "card_id")?;
                // Only explicit catalog fields may enter the store. Reject secrets
                // accidentally embedded in a live runtime object.
                let allowed = [
                    "profile_id",
                    "card_id",
                    "eid",
                    "iccid",
                    "display_name",
                    "phone_number",
                    "tombstone",
                ];
                let object = profile.as_object().ok_or_else(invalid)?;
                if object.keys().any(|k| !allowed.contains(&k.as_str())) {
                    return Err(invalid());
                }
                for key in ["eid", "iccid", "display_name", "phone_number"] {
                    if profile.get(key).is_some() {
                        text(profile, key, 512)?;
                    }
                }
                state.profiles.insert(profile_id, profile.clone());
            }
            let revision = next_revision(state)?;
            return Ok(json!({"revision":revision,"updated":profiles.len()}));
        }
        let profile = id(body, "profile_id")?;
        require_profile(
            state,
            &profile,
            matches!(action, "desired" | "queue" | "claim"),
        )?;
        match action {
            "desired" => {
                let desired = text(body, "desired_state", 32)?;
                if !matches!(desired.as_str(), "keep_online" | "stopped") {
                    return Err(invalid());
                }
                let revision = next_revision(state)?;
                state.desired.insert(
                    profile.clone(),
                    Desired {
                        desired_state: desired.clone(),
                        revision,
                    },
                );
                Ok(json!({"profile_id":profile,"desired_state":desired,"revision":revision}))
            }
            "queue" => {
                let request = id(body, "client_request_id")?;
                let phone = text(body, "phone", 256)?;
                let content = text(body, "body", MAX_TEXT)?;
                if phone.trim().is_empty() || content.is_empty() {
                    return Err(invalid());
                }
                let payload_hash = digest(&json!({"phone":phone,"body":content}))?;
                let request_key = scope(&profile, &request);
                if let Some(prior) = state.requests.get(&request_key) {
                    return if prior.hash == payload_hash {
                        Ok(prior.response.clone())
                    } else {
                        Err(conflict())
                    };
                }
                if state
                    .desired
                    .get(&profile)
                    .map(|d| d.desired_state.as_str())
                    != Some("keep_online")
                {
                    return Err(failure("profile_stopped"));
                }
                let message_id = format!("msg-{}", random_token()?);
                let revision = next_revision(state)?;
                let message = json!({"message_id":message_id,"profile_id":profile,"direction":"outbound","peer":phone,"phone":phone,"body":content,"created_at_ms":now_ms(),"state":"queued","client_request_id":request,"revision":revision,"deleted":false});
                state.messages.insert(scope(&profile, &message_id), message);
                let response =
                    json!({"message_id":message_id,"state":"queued","revision":revision});
                state.requests.insert(
                    request_key,
                    Event {
                        hash: payload_hash,
                        response: response.clone(),
                    },
                );
                change(state, &profile, &message_id);
                Ok(response)
            }
            "claim" => {
                let message_id = id(body, "message_id")?;
                let attempt = id(body, "attempt_token")?;
                let desired = state
                    .desired
                    .get(&profile)
                    .ok_or_else(|| failure("profile_stopped"))?;
                if desired.desired_state != "keep_online" {
                    return Err(failure("profile_stopped"));
                }
                if let Some(expected) = body.get("expected_revision")
                    && expected.as_u64() != Some(desired.revision)
                {
                    return Err(conflict());
                }
                let key = scope(&profile, &message_id);
                let message = state
                    .messages
                    .get(&key)
                    .ok_or_else(|| failure("message_not_found"))?;
                if message["direction"] != "outbound" || message["deleted"] == true {
                    return Err(conflict());
                }
                if message["state"] == "submitting" && message["attempt_token"] == attempt {
                    return Ok(
                        json!({"message_id":message_id,"state":"submitting","revision":message["revision"]}),
                    );
                }
                if message["state"] != "queued" {
                    return Err(conflict());
                }
                let revision = next_revision(state)?;
                let message = state.messages.get_mut(&key).expect("validated message");
                message["state"] = json!("submitting");
                message["attempt_token"] = json!(attempt);
                message["revision"] = json!(revision);
                change(state, &profile, &message_id);
                Ok(json!({"message_id":message_id,"state":"submitting","revision":revision}))
            }
            "outcome" => {
                let message_id = id(body, "message_id")?;
                let attempt = id(body, "attempt_token")?;
                let outcome = text(body, "state", 32)?;
                if !matches!(
                    outcome.as_str(),
                    "accepted" | "delivered" | "failed" | "unknown"
                ) {
                    return Err(invalid());
                }
                let key = scope(&profile, &message_id);
                let message = state
                    .messages
                    .get(&key)
                    .ok_or_else(|| failure("message_not_found"))?;
                if message["direction"] != "outbound" || message["attempt_token"] != attempt {
                    return Err(conflict());
                }
                let current = message["state"].as_str().unwrap_or_default();
                if current == "queued"
                    || (matches!(current, "delivered" | "failed") && current != outcome)
                    || (current == "accepted" && outcome == "unknown")
                {
                    return Err(conflict());
                }
                let revision = next_revision(state)?;
                let message = state.messages.get_mut(&key).expect("validated message");
                message["state"] = json!(outcome);
                message["revision"] = json!(revision);
                for field in ["sip_code", "rp_cause", "error"] {
                    if let Some(value) = body.get(field) {
                        message[field] = value.clone();
                    }
                }
                change(state, &profile, &message_id);
                Ok(json!({"message_id":message_id,"state":outcome,"revision":revision}))
            }
            "inbound" => self.inbound(state, &profile, body),
            _ => Err(failure("route_not_found")),
        }
    }

    fn inbound(
        &self,
        state: &mut Snapshot,
        profile: &str,
        body: &Value,
    ) -> Result<Value, StoreError> {
        let message_id = id(body, "message_id")?;
        let dedup = id(body, "dedup_key")?;
        let key = scope(profile, &dedup);
        let mut payload = body.clone();
        payload
            .as_object_mut()
            .ok_or_else(invalid)?
            .remove("event_id");
        payload
            .as_object_mut()
            .ok_or_else(invalid)?
            .remove("received_at_ms");
        let hash = digest(&payload)?;
        if let Some(prior) = state.inbound_dedup.get(&key) {
            return if prior.hash == hash {
                Ok(prior.response.clone())
            } else {
                Err(conflict())
            };
        }
        let kind = body
            .get("kind")
            .and_then(Value::as_str)
            .unwrap_or("message");
        if !matches!(kind, "message" | "status_report" | "undecoded") {
            return Err(invalid());
        }
        for field in ["raw_rpdu_hex", "raw_tpdu_hex"] {
            if let Some(value) = body.get(field) {
                let hex = value.as_str().ok_or_else(invalid)?;
                if hex.len() > MAX_TEXT
                    || hex.len() % 2 != 0
                    || !hex.bytes().all(|c| c.is_ascii_hexdigit())
                {
                    return Err(invalid());
                }
            }
        }
        let revision = next_revision(state)?;
        let response = if matches!(kind, "status_report" | "undecoded") {
            if kind == "undecoded"
                && body.get("raw_rpdu_hex").is_none()
                && body.get("raw_tpdu_hex").is_none()
            {
                return Err(invalid());
            }
            let record_state = if kind == "undecoded" {
                "undecoded"
            } else {
                "unmatched"
            };
            // Durable unmatched reports are not user messages and never guess by
            // an eight-bit TP-MR. A later explicit attempt-bound outcome may link it.
            let report_key = scope(profile, &format!("report:{dedup}"));
            state.messages.insert(report_key,json!({"message_id":message_id,"profile_id":profile,"direction":"report","kind":kind,"state":record_state,"created_at_ms":now_ms(),"revision":revision,"report":body.get("report").cloned().unwrap_or(Value::Null),"raw_rpdu_hex":body.get("raw_rpdu_hex"),"raw_tpdu_hex":body.get("raw_tpdu_hex")}));
            json!({"message_id":message_id,"state":record_state,"revision":revision,"complete":false})
        } else {
            Self::inbound_message(state, profile, body, &message_id, &dedup, revision)?
        };
        state.inbound_dedup.insert(
            key,
            Event {
                hash,
                response: response.clone(),
            },
        );
        Ok(response)
    }

    /// Auto grouping is chosen inside the same durable transaction as the raw
    /// fragment and network dedup record. A completed group never absorbs a new
    /// reference reuse; ambiguous incomplete groups quarantine later fragments.
    fn inbound_message(
        state: &mut Snapshot,
        profile: &str,
        body: &Value,
        incoming_id: &str,
        dedup: &str,
        revision: u64,
    ) -> Result<Value, StoreError> {
        let sender = text(body, "sender", 256)?;
        let content = text(body, "body", MAX_TEXT)?;
        let received = body["received_at_ms"].as_u64().ok_or_else(invalid)?;
        let (index, total, reference, auto_group) = if let Some(part) = body.get("part") {
            let index = part["index"].as_u64().ok_or_else(invalid)?;
            let total = part["total"].as_u64().ok_or_else(invalid)?;
            if total == 0 || total > 255 || index == 0 || index > total {
                return Err(invalid());
            }
            (
                index,
                total,
                text(part, "reference", 128)?,
                part["auto_group"] == true && total > 1,
            )
        } else {
            (1, 1, String::new(), false)
        };
        let part = json!({"body":content,"raw_rpdu_hex":body.get("raw_rpdu_hex"),"raw_tpdu_hex":body.get("raw_tpdu_hex")});
        let index_key = index.to_string();
        let mut message_id = incoming_id.to_owned();
        if auto_group {
            // The reference includes its width (8/16 bit). Compare the complete
            // observed timestamp span so a chain of late fragments cannot extend
            // one group indefinitely beyond the supported correlation window.
            let candidates: Vec<String> = state
                .messages
                .iter()
                .filter(|(_, message)| {
                    let first = message["group_first_at_ms"].as_u64().unwrap_or(0);
                    let last = message["group_last_at_ms"].as_u64().unwrap_or(first);
                    message["profile_id"] == profile
                        && message["peer"] == sender
                        && message["auto_group"] == true
                        && message["parts_total"] == total
                        && message["part_reference"] == reference
                        && message["deleted"] != true
                        && matches!(message["state"].as_str(), Some("partial" | "ambiguous"))
                        && last.max(received).saturating_sub(first.min(received))
                            <= MULTIPART_WINDOW_MS
                })
                .map(|(key, _)| key.clone())
                .collect();
            let ambiguous = candidates.len() > 1
                || candidates.first().is_some_and(|key| {
                    let message = &state.messages[key];
                    message["state"] == "ambiguous"
                        || message["parts"]
                            .get(&index_key)
                            .is_some_and(|old| old != &part)
                });
            if ambiguous {
                for key in &candidates {
                    let message = state.messages.get_mut(key).expect("candidate exists");
                    message["state"] = json!("ambiguous");
                    message["revision"] = json!(revision);
                    message["ambiguity_reason"] = json!("multipart_reference_collision");
                }
                // Preserve this transaction independently. Existing candidate
                // parts stay durable, but neither source is a public message.
                let raw_key = scope(profile, &format!("undecoded:{dedup}"));
                state.messages.insert(raw_key, json!({"message_id":incoming_id,"profile_id":profile,
                    "direction":"report","kind":"undecoded","state":"undecoded","peer":sender,
                    "body":content,"created_at_ms":received,"revision":revision,
                    "reason":"multipart_group_ambiguous","part":body.get("part"),
                    "raw_rpdu_hex":body.get("raw_rpdu_hex"),"raw_tpdu_hex":body.get("raw_tpdu_hex")}));
                return Ok(
                    json!({"message_id":incoming_id,"state":"undecoded","complete":false,"revision":revision}),
                );
            }
            if let Some(key) = candidates.first() {
                message_id = state.messages[key]["message_id"]
                    .as_str()
                    .ok_or_else(invalid)?
                    .to_owned();
            } else {
                message_id = format!("concat_{}", digest(&json!([profile, dedup]))?);
            }
        }
        let message_key = scope(profile, &message_id);
        let message = state.messages.entry(message_key).or_insert_with(|| json!({"message_id":message_id,
            "profile_id":profile,"direction":"inbound","peer":sender,"body":"","created_at_ms":received,
            "state":"partial","parts":{},"parts_total":total,"part_reference":reference,"deleted":false,
            "auto_group":auto_group,"group_first_at_ms":received,"group_last_at_ms":received}));
        if message["direction"] != "inbound"
            || message["peer"] != sender
            || message["parts_total"] != total
            || message["part_reference"] != reference
        {
            return Err(conflict());
        }
        let parts = message["parts"].as_object_mut().ok_or_else(invalid)?;
        if let Some(old) = parts.get(&index_key)
            && old != &part
        {
            return Err(conflict());
        }
        parts.insert(index_key, part);
        let complete = parts.len() == total as usize;
        let assembled = if complete {
            (1..=total)
                .map(|i| parts[&i.to_string()]["body"].as_str().unwrap_or_default())
                .collect::<String>()
        } else {
            String::new()
        };
        if assembled.len() > MAX_TEXT {
            return Err(invalid());
        }
        let was_complete = message["state"] == "received";
        message["body"] = json!(assembled);
        message["state"] = json!(if complete { "received" } else { "partial" });
        message["revision"] = json!(revision);
        message["group_first_at_ms"] = json!(
            message["group_first_at_ms"]
                .as_u64()
                .unwrap_or(received)
                .min(received)
        );
        message["group_last_at_ms"] = json!(
            message["group_last_at_ms"]
                .as_u64()
                .unwrap_or(received)
                .max(received)
        );
        if complete && !was_complete && message["deleted"] != true {
            change(state, profile, &message_id);
        }
        Ok(
            json!({"message_id":message_id,"state":if complete {"received"} else {"partial"},"complete":complete,"revision":revision}),
        )
    }

    fn read(&self, action: &str, profile: Option<&str>, cursor: u64) -> Result<Value, StoreError> {
        let state = read_snapshot(&self.0.load()?)?;
        match action {
            "catalog" => Ok(
                json!({"profiles":state.profiles.values().collect::<Vec<_>>(),"desired":state.desired.iter().map(|(id,d)|json!({"profile_id":id,"desired_state":d.desired_state,"revision":d.revision})).collect::<Vec<_>>(),"revision":state.revision}),
            ),
            "pending" => Ok(
                json!({"messages":state.messages.values().filter(|m| m["state"]=="queued" && m["deleted"]!=true && profile.is_none_or(|p|m["profile_id"]==p)).collect::<Vec<_>>(),"revision":state.revision}),
            ),
            "messages" => {
                let profile = profile.ok_or_else(invalid)?;
                require_profile(&state, profile, false)?;
                let mut messages: Vec<_> = state
                    .messages
                    .values()
                    .filter(|m| {
                        m["profile_id"] == profile
                            && matches!(m["direction"].as_str(), Some("inbound" | "outbound"))
                            && !matches!(m["state"].as_str(), Some("partial" | "ambiguous"))
                            && m["deleted"] != true
                            && m["revision"].as_u64().unwrap_or(0) > cursor
                    })
                    .cloned()
                    .collect();
                messages.sort_by_key(|m| m["revision"].as_u64().unwrap_or(0));
                messages.truncate(PAGE_SIZE);
                let next = messages
                    .last()
                    .and_then(|m| m["revision"].as_u64())
                    .unwrap_or(state.revision);
                // Public data deliberately excludes raw protocol payloads and private attempt tokens.
                for m in &mut messages {
                    if let Some(map) = m.as_object_mut() {
                        for key in [
                            "parts",
                            "attempt_token",
                            "auto_group",
                            "group_first_at_ms",
                            "group_last_at_ms",
                            "ambiguity_reason",
                        ] {
                            map.remove(key);
                        }
                    }
                }
                Ok(json!({"messages":messages,"next_cursor":next,"revision":state.revision}))
            }
            "changes" => {
                let changes: Vec<_> = state
                    .changes
                    .iter()
                    .filter(|v| v["revision"].as_u64().unwrap_or(0) > cursor)
                    .take(PAGE_SIZE)
                    .cloned()
                    .collect();
                let next = changes
                    .last()
                    .and_then(|c| c["revision"].as_u64())
                    .unwrap_or(state.revision);
                Ok(json!({"changes":changes,"next_cursor":next,"revision":state.revision}))
            }
            _ => Err(failure("route_not_found")),
        }
    }

    fn delete(&self, profile: &str, message_id: &str) -> Result<Value, StoreError> {
        self.0.mutate(|items| {
            let mut state = read_snapshot(items)?;
            require_profile(&state, profile, false)?;
            let key = scope(profile, message_id);
            let Some(m) = state.messages.get(&key) else {
                return Err(failure("message_not_found"));
            };
            if matches!(m["state"].as_str(), Some("queued" | "submitting")) {
                return Err(conflict());
            }
            if m["deleted"] == true {
                return Ok(json!({"deleted":true,"revision":state.revision}));
            }
            let revision = next_revision(&mut state)?;
            let m = state.messages.get_mut(&key).expect("validated message");
            m["deleted"] = json!(true);
            m["revision"] = json!(revision);
            if let Some(record) = m.as_object_mut() {
                for key in [
                    "body",
                    "peer",
                    "phone",
                    "parts",
                    "raw_rpdu_hex",
                    "raw_tpdu_hex",
                ] {
                    record.remove(key);
                }
            }
            change(&mut state, profile, message_id);
            persist_snapshot(items, state)?;
            Ok(json!({"deleted":true,"revision":revision}))
        })
    }
}

pub(crate) fn random_token() -> Result<String, StoreError> {
    let mut bytes = [0u8; 32];
    getrandom::fill(&mut bytes).map_err(|_| failure("entropy_unavailable"))?;
    Ok(bytes.iter().map(|b| format!("{b:02x}")).collect())
}
pub(crate) fn internal_authenticated<B>(request: &Request<B>, expected: &str) -> bool {
    if expected.is_empty() {
        return false;
    }
    let Some(value) = request
        .headers()
        .get(INTERNAL_HEADER)
        .and_then(|v| v.to_str().ok())
    else {
        return false;
    };
    value.len() == expected.len() && bool::from(value.as_bytes().ct_eq(expected.as_bytes()))
}
pub(crate) fn is_public_route(method: &Method, path: &str) -> bool {
    (method == Method::GET
        && matches!(
            path,
            "/api/multistandby/messages" | "/api/multistandby/changes"
        ))
        || (method == Method::DELETE && path.starts_with("/api/multistandby/messages/"))
}
pub(crate) fn shutdown_write_allowed(path: &str) -> bool {
    matches!(
        path,
        "/api/internal/multistandby/store/inbound" | "/api/internal/multistandby/store/outcome"
    )
}
fn error_response(error: StoreError) -> Response<Full<Bytes>> {
    let code = match &error {
        StoreError::Configuration(code) => code.as_str(),
        _ => "store_io_failed",
    };
    let status = match code {
        "invalid_request" => StatusCode::BAD_REQUEST,
        "profile_not_found" | "message_not_found" | "route_not_found" => StatusCode::NOT_FOUND,
        "state_conflict" | "profile_stopped" | "profile_deleted" => StatusCode::CONFLICT,
        _ => StatusCode::INTERNAL_SERVER_ERROR,
    };
    crate::json_response(
        status,
        &json!({"error":"多待存储操作未完成","code":code,"retryable":false}),
    )
}
fn query(request: &Request<Incoming>) -> Result<BTreeMap<String, String>, StoreError> {
    let mut out = BTreeMap::new();
    for pair in request
        .uri()
        .query()
        .unwrap_or("")
        .split('&')
        .filter(|s| !s.is_empty())
    {
        let (key, value) = pair.split_once('=').ok_or_else(invalid)?;
        // IDs and cursors are opaque URL-safe tokens in this contract.
        if !value
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b"-_.:".contains(&c))
            || out.insert(key.to_owned(), value.to_owned()).is_some()
        {
            return Err(invalid());
        }
    }
    Ok(out)
}
pub(crate) async fn handle(
    mut request: Request<Incoming>,
    store: Store,
    internal: bool,
) -> Response<Full<Bytes>> {
    let method = request.method().clone();
    let path = request.uri().path().to_owned();
    let params = match query(&request) {
        Ok(v) => v,
        Err(e) => return error_response(e),
    };
    let cursor = match params.get("cursor").map(|s| s.parse::<u64>()).transpose() {
        Ok(v) => v.unwrap_or(0),
        Err(_) => return error_response(invalid()),
    };
    let profile = params.get("profile_id").cloned();
    if method == Method::GET {
        let action = if internal {
            path.strip_prefix(INTERNAL_PREFIX).unwrap_or("")
        } else if path.ends_with("/changes") {
            "changes"
        } else {
            "messages"
        }
        .to_owned();
        if (internal && !matches!(action.as_str(), "catalog" | "pending"))
            || (!internal && !is_public_route(&method, &path))
        {
            return error_response(failure("route_not_found"));
        }
        return match crate::archive_io::run(move || store.read(&action, profile.as_deref(), cursor))
            .await
        {
            Ok(v) => crate::json_response(StatusCode::OK, &v),
            Err(e) => error_response(e),
        };
    }
    if !internal && method == Method::DELETE {
        let Some(profile) = profile else {
            return error_response(invalid());
        };
        let message_id = path
            .trim_start_matches("/api/multistandby/messages/")
            .to_owned();
        return match crate::archive_io::run(move || store.delete(&profile, &message_id)).await {
            Ok(v) => crate::json_response(StatusCode::OK, &v),
            Err(e) => error_response(e),
        };
    }
    if !internal || method != Method::POST {
        return error_response(failure("route_not_found"));
    }
    let mut bytes = Vec::new();
    while let Some(frame) = request.body_mut().frame().await {
        let frame = match frame {
            Ok(f) => f,
            Err(_) => return error_response(invalid()),
        };
        if let Ok(data) = frame.into_data() {
            if bytes.len().saturating_add(data.len()) > MAX_BODY {
                return crate::json_response(
                    StatusCode::PAYLOAD_TOO_LARGE,
                    &json!({"code":"body_too_large"}),
                );
            }
            bytes.extend_from_slice(&data);
        }
    }
    let body = match serde_json::from_slice::<Value>(&bytes) {
        Ok(v) => v,
        Err(_) => return error_response(invalid()),
    };
    let action = path.strip_prefix(INTERNAL_PREFIX).unwrap_or("").to_owned();
    match crate::archive_io::run(move || store.apply(&action, body)).await {
        Ok(v) => crate::json_response(StatusCode::OK, &v),
        Err(e) => error_response(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    struct Fixture {
        root: std::path::PathBuf,
        store: Store,
    }
    impl Fixture {
        fn new() -> Self {
            let root = std::env::temp_dir().join(format!("sw-store-{}", random_token().unwrap()));
            let store = Store::open(&root).unwrap();
            store.recover().unwrap();
            store.apply("catalog",json!({"event_id":"catalog-1","profiles":[{"profile_id":"A","card_id":"C","eid":"E","iccid":"1"},{"profile_id":"B","card_id":"C","eid":"E","iccid":"2"}]})).unwrap();
            for p in ["A", "B"] {
                store.apply("desired",json!({"event_id":format!("start-{p}"),"profile_id":p,"desired_state":"keep_online"})).unwrap();
            }
            Self { root, store }
        }
        fn queue(&self, p: &str, id: &str) -> Value {
            self.store.apply("queue",json!({"event_id":format!("queue-{p}-{id}"),"profile_id":p,"client_request_id":id,"phone":"+1000","body":"hello"})).unwrap()
        }
    }
    impl Drop for Fixture {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.root).unwrap();
        }
    }
    fn incoming(p: &str, event: &str, key: &str, message: &str, body: &str) -> Value {
        json!({"event_id":event,"dedup_key":key,"profile_id":p,"message_id":message,"sender":"+1000","body":body,"received_at_ms":123})
    }

    #[test]
    fn profiles_and_network_dedup_remain_isolated_across_reload() {
        let f = Fixture::new();
        for p in ["A", "B"] {
            f.store
                .apply(
                    "inbound",
                    incoming(
                        p,
                        &format!("event-{p}"),
                        "same-wire-id",
                        "same-message",
                        "hello",
                    ),
                )
                .unwrap();
        }
        let mut retry = incoming("A", "retry", "same-wire-id", "same-message", "hello");
        retry["received_at_ms"] = json!(456);
        assert!(
            f.store.apply("inbound", retry).unwrap()["complete"]
                .as_bool()
                .unwrap()
        );
        let reloaded = Store::open(&f.root).unwrap();
        assert_eq!(
            reloaded.read("messages", Some("A"), 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        reloaded.delete("A", "same-message").unwrap();
        assert!(
            reloaded.read("messages", Some("A"), 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            reloaded.read("messages", Some("B"), 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        reloaded.apply("catalog",json!({"event_id":"removed-A","profiles":[{"profile_id":"A","card_id":"C","tombstone":true}]})).unwrap();
        assert!(reloaded.read("messages", Some("A"), 0).is_ok());
    }

    #[test]
    fn multipart_journal_survives_restart_before_completion_and_reports_do_not_notify() {
        let f = Fixture::new();
        let mut first = incoming("A", "part-1", "wire-1", "multi", "hello ");
        first["part"] = json!({"reference":"r1","index":1,"total":2});
        assert_eq!(f.store.apply("inbound", first).unwrap()["state"], "partial");
        assert!(
            f.store.read("messages", Some("A"), 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        assert!(
            f.store.read("changes", None, 0).unwrap()["changes"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        let reloaded = Store::open(&f.root).unwrap();
        reloaded.recover().unwrap();
        let mut second = incoming("A", "part-2", "wire-2", "multi", "world");
        second["part"] = json!({"reference":"r1","index":2,"total":2});
        assert_eq!(
            reloaded.apply("inbound", second).unwrap()["state"],
            "received"
        );
        assert_eq!(
            reloaded.read("messages", Some("A"), 0).unwrap()["messages"][0]["body"],
            "hello world"
        );
        reloaded.apply("inbound",json!({"event_id":"report","dedup_key":"report-1","profile_id":"A","message_id":"rp1","kind":"status_report","raw_tpdu_hex":"00ff","report":{"tp_mr":1}})).unwrap();
        assert_eq!(
            reloaded.read("changes", None, 0).unwrap()["changes"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            read_snapshot(&reloaded.0.load().unwrap())
                .unwrap()
                .messages
                .values()
                .filter(|m| m["state"] == "unmatched")
                .count(),
            1
        );
    }

    fn auto_part(event: &str, index: u64, total: u64, received: u64, body: &str) -> Value {
        let mut value = incoming(
            "A",
            event,
            &format!("wire-{event}"),
            &format!("tx-{event}"),
            body,
        );
        value["received_at_ms"] = json!(received);
        value["raw_tpdu_hex"] = json!(stable_digest_hex(event.as_bytes()));
        value["part"] = json!({"reference":"8:42","index":index,"total":total,"auto_group":true});
        value
    }

    #[test]
    fn auto_multipart_cross_second_fragments_survive_restart_and_deduplicate() {
        let f = Fixture::new();
        let first = auto_part("auto-first", 1, 2, 10_000, "hello ");
        let first_result = f.store.apply("inbound", first.clone()).unwrap();
        assert_eq!(first_result["state"], "partial");
        let mut duplicate = first;
        duplicate["event_id"] = json!("auto-first-retry");
        duplicate["received_at_ms"] = json!(11_000);
        assert_eq!(f.store.apply("inbound", duplicate).unwrap(), first_result);
        let reloaded = Store::open(&f.root).unwrap();
        reloaded.recover().unwrap();
        let result = reloaded
            .apply("inbound", auto_part("auto-second", 2, 2, 12_000, "world"))
            .unwrap();
        assert_eq!(result["state"], "received");
        assert_eq!(result["message_id"], first_result["message_id"]);
        let public = reloaded.read("messages", Some("A"), 0).unwrap();
        assert_eq!(public["messages"].as_array().unwrap().len(), 1);
        assert_eq!(public["messages"][0]["body"], "hello world");
        assert!(public["messages"][0].get("auto_group").is_none());
        assert_eq!(
            reloaded.read("changes", None, 0).unwrap()["changes"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn auto_multipart_completed_reference_reuse_creates_a_distinct_message() {
        let f = Fixture::new();
        let old = f
            .store
            .apply("inbound", auto_part("old-1", 1, 2, 10_000, "old "))
            .unwrap();
        f.store
            .apply("inbound", auto_part("old-2", 2, 2, 11_000, "message"))
            .unwrap();
        let next = f
            .store
            .apply("inbound", auto_part("new-1", 1, 2, 12_000, "new "))
            .unwrap();
        assert_ne!(old["message_id"], next["message_id"]);
        f.store
            .apply("inbound", auto_part("new-2", 2, 2, 13_000, "message"))
            .unwrap();
        let messages = f.store.read("messages", Some("A"), 0).unwrap();
        assert_eq!(messages["messages"].as_array().unwrap().len(), 2);
        assert_eq!(messages["messages"][0]["body"], "old message");
        assert_eq!(messages["messages"][1]["body"], "new message");
    }

    #[test]
    fn auto_multipart_conflicting_index_preserves_raw_fragments_without_notification() {
        let f = Fixture::new();
        f.store
            .apply("inbound", auto_part("collision-1", 1, 2, 10_000, "first "))
            .unwrap();
        let conflict = f
            .store
            .apply(
                "inbound",
                auto_part("collision-2", 1, 2, 11_000, "different "),
            )
            .unwrap();
        assert_eq!(conflict["state"], "undecoded");
        let reloaded = Store::open(&f.root).unwrap();
        reloaded.recover().unwrap();
        assert_eq!(
            reloaded
                .apply("inbound", auto_part("collision-tail", 2, 2, 12_000, "tail"))
                .unwrap()["state"],
            "undecoded"
        );
        assert!(
            reloaded.read("messages", Some("A"), 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        assert!(
            reloaded.read("changes", None, 0).unwrap()["changes"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        let state = read_snapshot(&reloaded.0.load().unwrap()).unwrap();
        let group = state
            .messages
            .values()
            .find(|m| m["state"] == "ambiguous")
            .unwrap();
        assert!(group["parts"]["1"]["raw_tpdu_hex"].as_str().is_some());
        assert_eq!(
            state
                .messages
                .values()
                .filter(|m| m["state"] == "undecoded")
                .count(),
            2
        );
    }

    #[test]
    fn auto_multipart_multiple_window_candidates_are_quarantined() {
        let f = Fixture::new();
        f.store
            .apply("inbound", auto_part("window-1", 1, 3, 10_000, "one "))
            .unwrap();
        f.store
            .apply("inbound", auto_part("window-2", 1, 3, 210_000, "two "))
            .unwrap();
        // Out-of-order SCTS falls within both independent incomplete windows.
        let reply = f
            .store
            .apply(
                "inbound",
                auto_part("window-middle", 2, 3, 110_000, "unknown "),
            )
            .unwrap();
        assert_eq!(reply["state"], "undecoded");
        let state = read_snapshot(&f.store.0.load().unwrap()).unwrap();
        assert_eq!(
            state
                .messages
                .values()
                .filter(|m| m["state"] == "ambiguous")
                .count(),
            2
        );
        assert!(
            f.store.read("changes", None, 0).unwrap()["changes"]
                .as_array()
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn auto_multipart_profile_reference_width_and_time_window_do_not_mix() {
        let f = Fixture::new();
        let first = f
            .store
            .apply("inbound", auto_part("scope-first", 1, 2, 10_000, "A "))
            .unwrap();
        let mut other_profile = auto_part("scope-B", 2, 2, 11_000, "B");
        other_profile["profile_id"] = json!("B");
        assert_eq!(
            f.store.apply("inbound", other_profile).unwrap()["state"],
            "partial"
        );
        let mut other_width = auto_part("scope-width", 2, 2, 11_000, "16");
        other_width["part"]["reference"] = json!("16:42");
        assert_eq!(
            f.store.apply("inbound", other_width).unwrap()["state"],
            "partial"
        );
        let later = f
            .store
            .apply("inbound", auto_part("scope-later", 2, 2, 130_001, "late"))
            .unwrap();
        assert_eq!(later["state"], "partial");
        assert_ne!(later["message_id"], first["message_id"]);
        assert!(
            f.store.read("messages", Some("A"), 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn outbox_idempotence_cancel_fence_and_uncertain_crash_never_resubmit() {
        let f = Fixture::new();
        let queued = f.queue("A", "request");
        let id = queued["message_id"].as_str().unwrap();
        assert_eq!(f.queue("A", "request"), queued);
        assert!(f.store.apply("queue",json!({"event_id":"different-body","profile_id":"A","client_request_id":"request","phone":"+1000","body":"changed"})).is_err());
        let desired = f.store.read("catalog", None, 0).unwrap()["desired"]
            .as_array()
            .unwrap()
            .iter()
            .find(|d| d["profile_id"] == "A")
            .unwrap()["revision"]
            .as_u64()
            .unwrap();
        f.store
            .apply(
                "desired",
                json!({"event_id":"stop-A","profile_id":"A","desired_state":"stopped"}),
            )
            .unwrap();
        assert!(f.store.apply("claim",json!({"event_id":"claim-stopped","profile_id":"A","message_id":id,"attempt_token":"attempt","expected_revision":desired})).is_err());
        f.store
            .apply(
                "desired",
                json!({"event_id":"restart-A","profile_id":"A","desired_state":"keep_online"}),
            )
            .unwrap();
        assert!(f.store.apply("claim",json!({"event_id":"claim-stale","profile_id":"A","message_id":id,"attempt_token":"attempt","expected_revision":desired})).is_err());
        f.store.apply("claim",json!({"event_id":"claim","profile_id":"A","message_id":id,"attempt_token":"attempt"})).unwrap();
        let reloaded = Store::open(&f.root).unwrap();
        reloaded.recover().unwrap();
        assert!(
            reloaded.read("pending", None, 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            reloaded.read("messages", Some("A"), 0).unwrap()["messages"][0]["state"],
            "unknown"
        );
        assert!(reloaded.apply("claim",json!({"event_id":"reclaim","profile_id":"A","message_id":id,"attempt_token":"attempt2"})).is_err());
        assert!(reloaded.apply("outcome",json!({"event_id":"wrong-owner","profile_id":"A","message_id":id,"attempt_token":"wrong","state":"delivered"})).is_err());
        reloaded.apply("outcome",json!({"event_id":"late-valid","profile_id":"A","message_id":id,"attempt_token":"attempt","state":"delivered"})).unwrap();
    }

    #[test]
    fn concurrent_duplicate_submission_has_one_message_and_payload_conflict_is_rejected() {
        let f = Fixture::new();
        let store = Arc::new(f.store.clone());
        let workers:Vec<_>=(0..8).map(|i|{let store=store.clone();std::thread::spawn(move||store.apply("queue",json!({"event_id":format!("e{i}"),"profile_id":"A","client_request_id":"same","phone":"+1000","body":"hello"})).unwrap())}).collect();
        let results: Vec<_> = workers.into_iter().map(|t| t.join().unwrap()).collect();
        assert!(
            results
                .iter()
                .all(|r| r["message_id"] == results[0]["message_id"])
        );
        assert_eq!(
            store.read("pending", None, 0).unwrap()["messages"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        assert!(
            store
                .apply(
                    "desired",
                    json!({"event_id":"e0","profile_id":"A","desired_state":"stopped"})
                )
                .is_err()
        );
    }

    #[test]
    fn failed_write_never_reports_durable_success_or_truncates_existing_records() {
        let f = Fixture::new();
        let before = std::fs::read(f.store.0.path()).unwrap();
        let limited = Store(
            ArchiveStore::with_limits(f.store.0.path(), before.len() as u64 + 10, 1).unwrap(),
        );
        let body = "x".repeat(2048);
        assert!(
            limited
                .apply("inbound", incoming("A", "too-large", "large", "new", &body))
                .is_err()
        );
        assert_eq!(std::fs::read(f.store.0.path()).unwrap(), before);
    }

    #[test]
    fn application_credential_cannot_authorize_internal_store_or_reservation() {
        let app = Request::builder()
            .header("x-proletarius-app-token", "app")
            .body(())
            .unwrap();
        assert!(!internal_authenticated(&app, "private"));
        let valid = Request::builder()
            .header(INTERNAL_HEADER, "private")
            .body(())
            .unwrap();
        assert!(internal_authenticated(&valid, "private"));
        assert!(!internal_authenticated(&valid, "other"));
        assert!(!internal_authenticated(&valid, ""));
    }
    #[test]
    fn cached_claim_cannot_authorize_network_replay_after_recovery() {
        let f = Fixture::new();
        let queued = f.queue("A", "crash-test");
        let claim = json!({"event_id":"claim-once","profile_id":"A","message_id":queued["message_id"],"attempt_token":"attempt"});
        f.store.apply("claim", claim.clone()).unwrap();
        f.store.recover().unwrap();
        assert!(f.store.apply("claim", claim).is_err());
    }

    #[test]
    fn undecodable_sms_is_durable_and_never_published_as_empty_text() {
        let f = Fixture::new();
        let reply=f.store.apply("inbound",json!({"event_id":"bad-decode","dedup_key":"wire-undecoded","profile_id":"A","message_id":"opaque","kind":"undecoded","raw_tpdu_hex":"ffaa","report":{"decode_error":"unsupported_dcs"}})).unwrap();
        assert_eq!(reply["state"], "undecoded");
        assert_eq!(reply["complete"], false);
        assert!(
            f.store.read("changes", None, 0).unwrap()["changes"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            read_snapshot(&f.store.0.load().unwrap())
                .unwrap()
                .messages
                .values()
                .filter(|m| m["kind"] == "undecoded")
                .count(),
            1
        );
    }

    async fn http_roundtrip(
        store: Store,
        method: &str,
        path: &str,
        header: &str,
        token: &str,
        body: Value,
    ) -> (StatusCode, Value) {
        let state = crate::ServerState {
            app_token: Some("app-fixture".into()),
            internal_token: "private-fixture".into(),
            multistandby_store: store,
            go_socket_path: std::path::PathBuf::new(),
            availability: crate::BackendAvailability::new(),
            reader_discovery: Arc::new(std::sync::Mutex::new(crate::ReaderDiscovery::default())),
            snapshot_instance_id: "fixture".into(),
            snapshot_revision: Arc::new(crate::AtomicU64::new(0)),
            eap_exchanges: Arc::new(crate::eap_identity::EapExchangeStore::default()),
        };
        let (client, server) = tokio::net::UnixStream::pair().unwrap();
        let server = tokio::spawn(crate::serve_connection(server, state));
        let (mut sender, connection) =
            hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(client))
                .await
                .unwrap();
        let connection = tokio::spawn(connection);
        let request = Request::builder()
            .method(method)
            .uri(path)
            .header(header, token)
            .body(Full::new(Bytes::from(serde_json::to_vec(&body).unwrap())))
            .unwrap();
        let response = sender.send_request(request).await.unwrap();
        let status = response.status();
        let body = response.into_body().collect().await.unwrap().to_bytes();
        drop(sender);
        if let Err(error) = connection.await.unwrap() {
            assert!(
                crate::benign_connection_error_message(&error.to_string()),
                "{error}"
            );
        }
        if let Err(error) = server.await.unwrap() {
            assert!(
                crate::benign_connection_error_message(&error.to_string()),
                "{error}"
            );
        }
        (status, serde_json::from_slice(&body).unwrap())
    }

    #[tokio::test]
    async fn real_http_routes_enforce_private_credential_and_keep_history_offline() {
        let f = Fixture::new();
        let path = "/api/internal/multistandby/store/catalog";
        assert_eq!(
            http_roundtrip(
                f.store.clone(),
                "GET",
                path,
                "x-proletarius-app-token",
                "app-fixture",
                Value::Null
            )
            .await
            .0,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            http_roundtrip(
                f.store.clone(),
                "GET",
                path,
                INTERNAL_HEADER,
                "private-fixture",
                Value::Null
            )
            .await
            .0,
            StatusCode::OK
        );
        let incoming = incoming("A", "http-in", "http-wire", "http-message", "new text");
        assert_eq!(
            http_roundtrip(
                f.store.clone(),
                "POST",
                "/api/internal/multistandby/store/inbound",
                INTERNAL_HEADER,
                "private-fixture",
                incoming
            )
            .await
            .0,
            StatusCode::OK
        );
        let (status, body) = http_roundtrip(
            f.store.clone(),
            "GET",
            "/api/multistandby/messages?profile_id=A&cursor=0",
            "x-proletarius-app-token",
            "app-fixture",
            Value::Null,
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(body["messages"][0]["body"], "new text");
        assert!(body["messages"][0].get("parts").is_none());
        assert_eq!(
            http_roundtrip(
                f.store.clone(),
                "GET",
                "/api/multistandby/messages?profile_id=A",
                INTERNAL_HEADER,
                "private-fixture",
                Value::Null
            )
            .await
            .0,
            StatusCode::UNAUTHORIZED
        );
    }
}
