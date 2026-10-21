//! Native RP envelope encoding only; this endpoint never sends an SMS.
use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use serde::Deserialize;
use serde_json::json;

#[derive(Deserialize)]
struct EncodeRequest {
    reference: u8,
    smsc: String,
    tpdu: Vec<u8>,
}

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let decoding = request.uri().path() == "/api/v2/sms/decode-rpdu";
    let preparing = request.uri().path() == "/api/v2/sms/prepare-tpdu";
    let decoding_incoming = request.uri().path() == "/api/v2/sms/decode-incoming-tpdu";
    let body = match tokio::time::timeout(
        std::time::Duration::from_secs(5),
        Limited::new(request.into_body(), if preparing { 262144 } else { 4096 }).collect(),
    )
    .await
    {
        Ok(Ok(body)) => body.to_bytes(),
        Ok(Err(_)) => return failure(StatusCode::BAD_REQUEST),
        Err(_) => return failure(StatusCode::REQUEST_TIMEOUT),
    };
    if decoding_incoming {
        decode_incoming(&body)
    } else if preparing {
        prepare(&body)
    } else if decoding {
        decode(&body)
    } else {
        encode(&body)
    }
}

fn decode_incoming(body: &[u8]) -> Response<Full<Bytes>> {
    #[derive(Deserialize)]
    struct Input {
        tpdu: Vec<u8>,
    }
    let Ok(input) = serde_json::from_slice::<Input>(body) else {
        return failure(StatusCode::BAD_REQUEST);
    };
    match input.tpdu.first().map(|value| value & 3) {
        Some(0) => decode_deliver_tpdu(&input.tpdu),
        Some(2) => decode_status_report_tpdu(&input.tpdu),
        _ => failure(StatusCode::UNPROCESSABLE_ENTITY),
    }
}

fn decode_deliver_tpdu(tpdu: &[u8]) -> Response<Full<Bytes>> {
    let Ok(message) = proletarius_sms::deliver::parse(tpdu) else {
        return failure(StatusCode::UNPROCESSABLE_ENTITY);
    };
    let (text, binary) = match message.content {
        proletarius_sms::user_data::Content::Text(text) => (Some(text), None),
        proletarius_sms::user_data::Content::Binary(value) => (None, Some(value)),
    };
    let concat = message.concatenation.map(|value| {
        json!({
            "reference":value.reference,"wide_reference":value.wide_reference,
            "total":value.total,"sequence":value.sequence
        })
    });
    let service_center_time = message.service_center_time.map(|value| {
        json!({
            "year":value.year,"month":value.month,"day":value.day,
            "hour":value.hour,"minute":value.minute,"second":value.second,
            "offset_minutes":value.offset_minutes
        })
    });
    crate::json_response(
        StatusCode::OK,
        &json!({
            "kind":"deliver",
            "sender":message.sender,"timestamp":message.timestamp,
            "service_center_time":service_center_time,
            "protocol_id":message.protocol_id,"dcs":message.dcs,
            "text":text,"binary":binary,"concatenation":concat
        }),
    )
}

fn decode_status_report_tpdu(tpdu: &[u8]) -> Response<Full<Bytes>> {
    let Ok(report) = proletarius_sms::status_report::parse(tpdu) else {
        return failure(StatusCode::UNPROCESSABLE_ENTITY);
    };
    crate::json_response(
        StatusCode::OK,
        &json!({
            "kind":"status_report", "reference":report.reference,
            "recipient":report.recipient, "status_code":report.status_code,
            "delivery_status":report.status.as_str()
        }),
    )
}

fn prepare(body: &[u8]) -> Response<Full<Bytes>> {
    #[derive(Deserialize)]
    struct Input {
        recipient: String,
        text: String,
        reference: u8,
    }
    let Ok(input) = serde_json::from_slice::<Input>(body) else {
        return failure(StatusCode::BAD_REQUEST);
    };
    match proletarius_sms::submit::prepare(&input.recipient, &input.text, input.reference) {
        Ok(parts) => crate::json_response(StatusCode::OK, &json!({"tpdus":parts})),
        Err(_) => failure(StatusCode::UNPROCESSABLE_ENTITY),
    }
}

fn decode(body: &[u8]) -> Response<Full<Bytes>> {
    #[derive(Deserialize)]
    struct DecodeRequest {
        rpdu: Vec<u8>,
    }
    let Ok(input) = serde_json::from_slice::<DecodeRequest>(body) else {
        return failure(StatusCode::BAD_REQUEST);
    };
    match proletarius_sms::MobileTerminatedData::parse(&input.rpdu) {
        Ok(message) => crate::json_response(
            StatusCode::OK,
            &json!({
                "reference": message.reference, "originator": message.originator,
                "tpdu": message.tpdu
            }),
        ),
        Err(_) => failure(StatusCode::UNPROCESSABLE_ENTITY),
    }
}

fn encode(body: &[u8]) -> Response<Full<Bytes>> {
    let Ok(input) = serde_json::from_slice::<EncodeRequest>(body) else {
        return failure(StatusCode::BAD_REQUEST);
    };
    match proletarius_sms::build_mobile_originated_data(input.reference, &input.smsc, &input.tpdu) {
        Ok(rpdu) => crate::json_response(StatusCode::OK, &json!({"rpdu":rpdu})),
        Err(_) => failure(StatusCode::UNPROCESSABLE_ENTITY),
    }
}

fn failure(status: StatusCode) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"短信协议参数无效或读取超时","code":"sms_encode_failed","retryable":false}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn prepares_text_as_numeric_tpdu_arrays() {
        let response = prepare(br#"{"recipient":"888","text":"BAL","reference":42}"#);
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(
            value,
            json!({"tpdus":[[33,0,3,129,136,248,0,0,3,194,32,19]]})
        );
    }

    #[tokio::test]
    async fn incoming_envelope_decodes_without_transmission() {
        let response = decode(br#"{"rpdu":[1,42,4,145,68,119,246,0,3,1,2,3]}"#);
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(
            value,
            json!({"reference":42,"originator":"+44776","tpdu":[1,2,3]})
        );
        assert_eq!(
            decode(br#"{"rpdu":[1,42,0,0,3,1]}"#).status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
    }

    #[tokio::test]
    async fn deliver_tpdu_decodes_to_explicit_text_or_binary() {
        let response =
            decode_incoming(br#"{"tpdu":[0,3,129,136,248,0,8,98,144,96,0,0,0,0,2,0,65]}"#);
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["sender"], "888");
        assert_eq!(value["kind"], "deliver");
        assert_eq!(value["text"], "A");
        assert_eq!(value["binary"], serde_json::Value::Null);
        assert_eq!(
            decode_incoming(br#"{"tpdu":[1,2,3]}"#).status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
    }

    #[tokio::test]
    async fn status_report_is_native_and_explicit() {
        let response = decode_incoming(
            br#"{"tpdu":[2,1,4,145,68,119,98,128,98,97,33,0,0,98,128,98,97,33,0,0,0]}"#,
        );
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["kind"], "status_report");
        assert_eq!(value["recipient"], "+4477");
        assert_eq!(value["delivery_status"], "delivered");
    }

    #[tokio::test]
    async fn octet_array_preserves_wire_bytes() {
        let response = encode(br#"{"reference":42,"smsc":"888","tpdu":[1,42]}"#);
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value, json!({"rpdu":[0,42,0,3,129,136,248,2,1,42]}));
    }

    #[test]
    fn invalid_wire_types_and_protocol_inputs_are_rejected() {
        for body in [
            r#"{"reference":256,"smsc":"888","tpdu":[1]}"#,
            r#"{"reference":1,"smsc":"888","tpdu":"AQ=="}"#,
            r#"{"reference":1,"smsc":"888","tpdu":[-1]}"#,
        ] {
            assert_eq!(encode(body.as_bytes()).status(), StatusCode::BAD_REQUEST);
        }
        for body in [
            r#"{"reference":1,"smsc":"invalid","tpdu":[1]}"#,
            r#"{"reference":1,"smsc":"888","tpdu":[]}"#,
        ] {
            assert_eq!(
                encode(body.as_bytes()).status(),
                StatusCode::UNPROCESSABLE_ENTITY
            );
        }
    }
}
