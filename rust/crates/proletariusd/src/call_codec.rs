//! Native validation for the small, non-real-time RFC 4733 control surface.
//! RTP sequencing and writing stay with the call runtime so this route cannot
//! add IPC jitter to voice media.

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use serde::Deserialize;
use serde_json::json;

#[derive(Deserialize)]
struct DtmfRequest {
    digit: String,
}

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let body = match tokio::time::timeout(
        std::time::Duration::from_secs(2),
        Limited::new(request.into_body(), 1024).collect(),
    )
    .await
    {
        Ok(Ok(value)) => value.to_bytes(),
        Ok(Err(_)) => return failure(StatusCode::BAD_REQUEST),
        Err(_) => return failure(StatusCode::REQUEST_TIMEOUT),
    };
    let Ok(input) = serde_json::from_slice::<DtmfRequest>(&body) else {
        return failure(StatusCode::BAD_REQUEST);
    };
    let digit = input.digit.trim();
    let Ok(event) = parse_event(digit) else {
        return failure(StatusCode::UNPROCESSABLE_ENTITY);
    };
    crate::json_response(
        StatusCode::OK,
        &json!({
            "digit": digit,
            "event": event,
            "end_payload": proletarius_call::dtmf_payload(event, 800, true),
        }),
    )
}

fn parse_event(value: &str) -> Result<u8, proletarius_call::CallError> {
    let mut chars = value.trim().chars();
    let Some(digit) = chars.next() else {
        return Err(proletarius_call::CallError::InvalidDtmfDigit);
    };
    if chars.next().is_some() {
        return Err(proletarius_call::CallError::InvalidDtmfDigit);
    }
    proletarius_call::dtmf_event(digit)
}

fn failure(status: StatusCode) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"DTMF 仅支持 0-9、* 和 #","code":"invalid_dtmf","retryable":false}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn handle_json(body: &[u8]) -> Response<Full<Bytes>> {
        let input = serde_json::from_slice::<DtmfRequest>(body).unwrap();
        let digit = input.digit.trim();
        let Ok(event) = parse_event(digit) else {
            return failure(StatusCode::UNPROCESSABLE_ENTITY);
        };
        crate::json_response(
            StatusCode::OK,
            &json!({"digit":digit,"event":event,"end_payload":proletarius_call::dtmf_payload(event,800,true)}),
        )
    }

    #[tokio::test]
    async fn emits_rfc4733_end_payload() {
        let response = handle_json(br##"{"digit":"#"}"##);
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&body).unwrap(),
            json!({"digit":"#","event":11,"end_payload":[11,138,3,32]})
        );
    }

    #[test]
    fn rejects_non_dtmf() {
        assert_eq!(
            handle_json(br#"{"digit":"A"}"#).status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
    }
}
