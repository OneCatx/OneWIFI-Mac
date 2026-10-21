//! Native PC/SC AKA endpoint. Requests are accepted only through the gateway's
//! authenticated Unix socket and execute on the blocking pool.

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use proletarius_reader::{AkaApplication, AuthenticationError};
use proletarius_sim::AkaResult;
use serde::Deserialize;
use serde_json::json;

#[derive(Deserialize)]
struct AuthRequest {
    reader: String,
    #[serde(default)]
    aid: Vec<u8>,
    #[serde(default)]
    application: Option<String>,
    #[serde(default)]
    expected_identity: Option<ExpectedIdentity>,
    rand: Vec<u8>,
    autn: Vec<u8>,
}

#[derive(Clone, PartialEq, Deserialize)]
pub(crate) struct ExpectedIdentity {
    pub(crate) iccid: String,
    pub(crate) imsi: String,
}

impl Drop for ExpectedIdentity {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.iccid.zeroize();
        self.imsi.zeroize();
    }
}

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let body = match tokio::time::timeout(
        std::time::Duration::from_secs(2),
        Limited::new(request.into_body(), 8192).collect(),
    )
    .await
    {
        Ok(Ok(value)) => value.to_bytes(),
        Ok(Err(_)) => return failure(StatusCode::BAD_REQUEST, "invalid_request"),
        Err(_) => return failure(StatusCode::REQUEST_TIMEOUT, "request_timeout"),
    };
    let Ok(input) = serde_json::from_slice::<AuthRequest>(&body) else {
        return failure(StatusCode::BAD_REQUEST, "invalid_request");
    };
    if !valid_input(&input) {
        return failure(
            StatusCode::UNPROCESSABLE_ENTITY,
            "invalid_authentication_input",
        );
    }
    let result = if let Some(expected) = &input.expected_identity {
        let application = if input.application.as_deref() == Some("usim") {
            AkaApplication::Usim
        } else {
            AkaApplication::Isim
        };
        let Ok(binding) = crate::reader_aka_binding::ReaderAkaBinding::new(
            &input.reader,
            application,
            &expected.iccid,
            &expected.imsi,
        ) else {
            return failure(
                StatusCode::UNPROCESSABLE_ENTITY,
                "invalid_authentication_input",
            );
        };
        binding
            .authenticate(
                input.rand.as_slice().try_into().unwrap(),
                input.autn.as_slice().try_into().unwrap(),
            )
            .await
    } else {
        crate::reader_task::run(
            move || {
                if let Some(application) = input.application.as_deref() {
                    let application = match application {
                        "usim" => AkaApplication::Usim,
                        "isim" => AkaApplication::Isim,
                        _ => unreachable!("validated before spawning"),
                    };
                    proletarius_reader::authenticate_aka_auto(
                        input.reader.trim(),
                        application,
                        &input.rand,
                        &input.autn,
                    )
                } else {
                    proletarius_reader::authenticate_aka(
                        input.reader.trim(),
                        &input.aid,
                        &input.rand,
                        &input.autn,
                    )
                }
            },
            std::time::Duration::from_secs(35),
        )
        .await
    };
    match result {
        Ok(Ok(AkaResult::Success { res, ck, ik })) => crate::json_response(
            StatusCode::OK,
            &json!({"outcome":"success","res":res,"ck":ck,"ik":ik}),
        ),
        Ok(Ok(AkaResult::SynchronizationFailure { auts })) => crate::json_response(
            StatusCode::OK,
            &json!({"outcome":"synchronization_failure","auts":auts}),
        ),
        Ok(Ok(AkaResult::AuthenticationReject)) => {
            crate::json_response(StatusCode::OK, &json!({"outcome":"authentication_reject"}))
        }
        Ok(Err(AuthenticationError::ApplicationUnavailable)) => {
            failure(StatusCode::NOT_FOUND, "reader_application_unavailable")
        }
        Ok(Err(error)) => {
            // RAND/AUTN and derived key material are intentionally never
            // logged. The typed error only contains the PC/SC stage/status,
            // which is enough to diagnose reader ownership and APDU failures.
            eprintln!("switchd-rs reader AKA failed: {error}");
            let code = error.failure_code();
            let status = match code {
                "reader_card_absent" | "reader_unavailable" | "reader_identity_changed" => {
                    StatusCode::CONFLICT
                }
                "reader_busy" => StatusCode::SERVICE_UNAVAILABLE,
                _ => StatusCode::BAD_GATEWAY,
            };
            failure(status, code)
        }
        Err(crate::reader_task::ReaderTaskError::Busy) => {
            failure(StatusCode::CONFLICT, "card_busy")
        }
        Err(crate::reader_task::ReaderTaskError::WorkerFailed) => {
            failure(StatusCode::INTERNAL_SERVER_ERROR, "reader_task_failed")
        }
        Err(crate::reader_task::ReaderTaskError::Timeout) => {
            failure(StatusCode::GATEWAY_TIMEOUT, "reader_authentication_timeout")
        }
    }
}

fn valid_input(input: &AuthRequest) -> bool {
    !input.reader.trim().is_empty()
        && input.expected_identity.as_ref().is_none_or(|identity| {
            input.application.is_some()
                && (18..=22).contains(&identity.iccid.len())
                && (5..=15).contains(&identity.imsi.len())
                && identity
                    .iccid
                    .bytes()
                    .chain(identity.imsi.bytes())
                    .all(|b| b.is_ascii_digit())
        })
        && input.rand.len() == 16
        && input.autn.len() == 16
        && match input.application.as_deref() {
            Some("usim" | "isim") => input.aid.is_empty(),
            None => (5..=16).contains(&input.aid.len()),
            _ => false,
        }
}

fn failure(status: StatusCode, code: &str) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"读卡器 AKA 鉴权失败","code":code,"retryable":false}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_partial_secrets_and_prefix_only_aid() {
        for (aid, rand, autn) in [
            (vec![0; 4], vec![0; 16], vec![0; 16]),
            (vec![0; 7], vec![0; 15], vec![0; 16]),
            (vec![0; 7], vec![0; 16], vec![0; 17]),
        ] {
            let input = AuthRequest {
                reader: "Reader".into(),
                aid,
                application: None,
                expected_identity: None,
                rand,
                autn,
            };
            assert!(!valid_input(&input));
        }
    }

    #[test]
    fn automatic_selection_requires_known_application_and_no_aid() {
        let mut input = AuthRequest {
            reader: "Reader".into(),
            aid: Vec::new(),
            application: Some("isim".into()),
            expected_identity: None,
            rand: vec![0; 16],
            autn: vec![0; 16],
        };
        assert!(valid_input(&input));
        input.aid = vec![0xa0; 8];
        assert!(!valid_input(&input));
        input.aid.clear();
        input.application = Some("sim".into());
        assert!(!valid_input(&input));
    }
}
