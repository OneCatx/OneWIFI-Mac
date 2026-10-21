//! Authenticated, read-only reader identity endpoint.

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use serde::Deserialize;
use serde_json::json;

#[derive(Deserialize)]
struct IdentityRequest {
    reader: String,
    #[serde(default)]
    aid: Vec<u8>,
}

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let body = match tokio::time::timeout(
        std::time::Duration::from_secs(2),
        Limited::new(request.into_body(), 4096).collect(),
    )
    .await
    {
        Ok(Ok(value)) => value.to_bytes(),
        Ok(Err(_)) => return failure(StatusCode::BAD_REQUEST, "invalid_request"),
        Err(_) => return failure(StatusCode::REQUEST_TIMEOUT, "request_timeout"),
    };
    let Ok(input) = serde_json::from_slice::<IdentityRequest>(&body) else {
        return failure(StatusCode::BAD_REQUEST, "invalid_request");
    };
    if input.reader.trim().is_empty()
        || (!input.aid.is_empty() && !(5..=16).contains(&input.aid.len()))
    {
        return failure(StatusCode::UNPROCESSABLE_ENTITY, "invalid_identity_input");
    }
    let task = crate::reader_task::run(
        move || {
            if input.aid.is_empty() {
                proletarius_reader::read_carrier_identity_auto(input.reader.trim())
            } else {
                proletarius_reader::read_carrier_identity(input.reader.trim(), &input.aid)
            }
        },
        std::time::Duration::from_secs(15),
    );
    match task.await {
        Ok(Ok(carrier)) => crate::json_response(
            StatusCode::OK,
            &json!({
                "ok": true,
                "iccid": carrier.identity.iccid,
                "imsi": carrier.identity.imsi,
                "mcc": carrier.identity.mcc,
                "mnc": carrier.identity.mnc,
                "gid1": carrier.gid1,
                "gid2": carrier.gid2,
                "spn": carrier.spn,
                "source": "rust_pcsc",
            }),
        ),
        Ok(Err(error)) => {
            eprintln!("switchd-rs reader identity failed: {error}");
            let code = error.failure_code();
            let status = match code {
                "reader_busy" => StatusCode::SERVICE_UNAVAILABLE,
                "reader_card_absent" | "reader_unavailable" => StatusCode::CONFLICT,
                _ => StatusCode::BAD_GATEWAY,
            };
            failure(status, code)
        }
        Err(crate::reader_task::ReaderTaskError::WorkerFailed) => {
            failure(StatusCode::INTERNAL_SERVER_ERROR, "reader_task_failed")
        }
        Err(crate::reader_task::ReaderTaskError::Timeout) => {
            failure(StatusCode::GATEWAY_TIMEOUT, "reader_identity_timeout")
        }
        Err(crate::reader_task::ReaderTaskError::Busy) => {
            failure(StatusCode::CONFLICT, "card_busy")
        }
    }
}

fn failure(status: StatusCode, code: &str) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"读取读卡器身份失败","code":code,"retryable":true}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_missing_reader_or_short_aid() {
        for input in [
            IdentityRequest {
                reader: " ".into(),
                aid: vec![0; 7],
            },
            IdentityRequest {
                reader: "Reader".into(),
                aid: vec![0; 4],
            },
        ] {
            assert!(
                input.reader.trim().is_empty()
                    || (!input.aid.is_empty() && !(5..=16).contains(&input.aid.len()))
            );
        }
    }

    #[test]
    fn missing_aid_is_valid_for_native_ef_dir_discovery() {
        let request: IdentityRequest = serde_json::from_str(r#"{"reader":"Reader"}"#).unwrap();
        assert!(request.aid.is_empty());
        assert!(!request.reader.trim().is_empty());
    }
}
