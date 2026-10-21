//! Stateless validation gate for REGISTER responses. The compatibility
//! runtime still owns TCP and registration state, while Rust independently
//! rejects malformed or cross-transaction responses before state mutation.

use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use serde::Deserialize;
use serde_json::json;

#[derive(Deserialize)]
struct ValidateRequest {
    response: Vec<u8>,
    call_id: String,
    cseq: u32,
}

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let body = match tokio::time::timeout(
        std::time::Duration::from_secs(2),
        Limited::new(request.into_body(), 131_072).collect(),
    )
    .await
    {
        Ok(Ok(value)) => value.to_bytes(),
        Ok(Err(_)) => return failure(StatusCode::BAD_REQUEST, "invalid_request"),
        Err(_) => return failure(StatusCode::REQUEST_TIMEOUT, "request_timeout"),
    };
    let Ok(input) = serde_json::from_slice::<ValidateRequest>(&body) else {
        return failure(StatusCode::BAD_REQUEST, "invalid_request");
    };
    validate(input)
}

fn validate(input: ValidateRequest) -> Response<Full<Bytes>> {
    if input.call_id.trim().is_empty() || input.cseq == 0 {
        return failure(StatusCode::UNPROCESSABLE_ENTITY, "invalid_transaction");
    }
    let Ok(response) = proletarius_ims::SipResponse::parse(&input.response) else {
        return failure(StatusCode::UNPROCESSABLE_ENTITY, "invalid_sip_response");
    };
    let Ok(transaction) = response.register_transaction() else {
        return failure(
            StatusCode::UNPROCESSABLE_ENTITY,
            "invalid_register_transaction",
        );
    };
    if transaction.call_id != input.call_id.trim() || transaction.cseq != input.cseq {
        return failure(StatusCode::CONFLICT, "unexpected_register_transaction");
    }
    crate::json_response(
        StatusCode::OK,
        &json!({
            "status":response.status,
            "reason":response.reason,
            "call_id":transaction.call_id,
            "cseq":transaction.cseq,
            "aka_challenge":response.has_aka_challenge(),
            "security_server":response.has_security_server()
        }),
    )
}

fn failure(status: StatusCode, code: &str) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"IMS REGISTER 响应校验失败","code":code,"retryable":false}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn body(response: Response<Full<Bytes>>) -> serde_json::Value {
        let bytes = response.into_body().collect().await.unwrap().to_bytes();
        serde_json::from_slice(&bytes).unwrap()
    }

    fn request(raw: &str, call_id: &str, cseq: u32) -> ValidateRequest {
        ValidateRequest {
            response: raw.as_bytes().to_vec(),
            call_id: call_id.into(),
            cseq,
        }
    }

    #[tokio::test]
    async fn validates_matching_aka_challenge() {
        let raw = "SIP/2.0 401 Unauthorized\r\nCall-ID: reg-1\r\nCSeq: 7 REGISTER\r\nWWW-Authenticate: Digest realm=\"ims\",nonce=\"redacted\",algorithm=AKAv1-MD5\r\nSecurity-Server: ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc\r\nContent-Length: 0\r\n\r\n";
        let response = validate(request(raw, "reg-1", 7));
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(body(response).await["aka_challenge"], true);
    }

    #[test]
    fn rejects_wrong_transaction_and_smuggled_body() {
        let raw =
            "SIP/2.0 200 OK\r\nCall-ID: other\r\nCSeq: 7 REGISTER\r\nContent-Length: 0\r\n\r\n";
        assert_eq!(
            validate(request(raw, "reg-1", 7)).status(),
            StatusCode::CONFLICT
        );
        let smuggled =
            "SIP/2.0 200 OK\r\nCall-ID: reg-1\r\nCSeq: 7 REGISTER\r\nContent-Length: 0\r\n\r\nx";
        assert_eq!(
            validate(request(smuggled, "reg-1", 7)).status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
    }
}
