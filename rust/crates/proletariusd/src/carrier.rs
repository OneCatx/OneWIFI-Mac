use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::{Request, Response, StatusCode, body::Incoming};
use proletarius_vowifi::carrier::{Registry, SimIdentity};
use proletarius_vowifi::carrier_runtime_profile::RuntimeCarrierProfile;
use serde_json::json;
use std::sync::OnceLock;

static REGISTRY: OnceLock<Result<Registry, String>> = OnceLock::new();

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let sim = match tokio::time::timeout(
        std::time::Duration::from_secs(5),
        decode(request.into_body()),
    )
    .await
    {
        Ok(Ok(sim)) => sim,
        Ok(Err((status, code))) => return error(status, code),
        Err(_) => return error(StatusCode::REQUEST_TIMEOUT, "request_body_timeout"),
    };
    let result = tokio::task::spawn_blocking(move || resolve(sim)).await;
    match result {
        Ok(response) => response,
        Err(_) => error(StatusCode::INTERNAL_SERVER_ERROR, "carrier_worker_failed"),
    }
}

async fn decode<B: hyper::body::Body<Data = Bytes> + Unpin>(
    mut body: B,
) -> Result<SimIdentity, (StatusCode, &'static str)> {
    let mut bytes = Vec::new();
    while let Some(frame) = body.frame().await {
        let Ok(frame) = frame else {
            return Err((StatusCode::BAD_REQUEST, "invalid_request_body"));
        };
        if let Ok(data) = frame.into_data() {
            if data.len() > 4096usize.saturating_sub(bytes.len()) {
                return Err((StatusCode::PAYLOAD_TOO_LARGE, "request_body_too_large"));
            }
            bytes.extend_from_slice(&data);
        }
    }
    serde_json::from_slice::<SimIdentity>(&bytes)
        .map_err(|_| (StatusCode::BAD_REQUEST, "invalid_carrier_identity"))
}

fn resolve(sim: SimIdentity) -> Response<Full<Bytes>> {
    let registry = match REGISTRY.get_or_init(Registry::embedded) {
        Ok(registry) => registry,
        Err(_) => {
            return error(
                StatusCode::INTERNAL_SERVER_ERROR,
                "carrier_registry_invalid",
            );
        }
    };
    let selected = match registry.resolve(&sim) {
        Ok(selected) => selected,
        Err(_) => return error(StatusCode::UNPROCESSABLE_ENTITY, "carrier_selection_failed"),
    };
    let runtime_profile = match selected.as_ref() {
        Some(profile) => RuntimeCarrierProfile::expand(profile, &sim, &registry.revision),
        None => Ok(RuntimeCarrierProfile::standard(&sim, &registry.revision)),
    };
    let runtime_profile = match runtime_profile {
        Ok(profile) => profile,
        Err(_) => return error(StatusCode::UNPROCESSABLE_ENTITY, "carrier_profile_invalid"),
    };
    let (matched, id, bundle, selector, configuration) = match selected {
        Some(profile) => (
            true,
            Some(profile.id),
            Some(profile.bundle),
            Some(profile.selector),
            Some(profile.configuration),
        ),
        None => (false, None, None, None, None),
    };
    crate::json_response(
        StatusCode::OK,
        &json!({
            "matched": matched,
            "id": id,
            "bundle": bundle,
            "selector": selector,
            "revision": registry.revision,
            "configuration": configuration,
            "runtime_profile": runtime_profile,
        }),
    )
}

fn error(status: StatusCode, code: &str) -> Response<Full<Bytes>> {
    // No SIM identity or configuration secrets in error responses or logs.
    crate::json_response(status, &json!({"error":code,"code":code,"retryable":false}))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn identity_decoder_bounds_and_validates_input() {
        assert!(matches!(
            decode(Full::new(Bytes::from(vec![b' '; 4097]))).await,
            Err((StatusCode::PAYLOAD_TOO_LARGE, _))
        ));
        assert!(matches!(
            decode(Full::new(Bytes::from_static(b"{"))).await,
            Err((StatusCode::BAD_REQUEST, _))
        ));
        let sim = decode(Full::new(Bytes::from_static(
            br#"{"mcc":"234","mnc":"33"}"#,
        )))
        .await
        .unwrap();
        assert_eq!(sim.mnc, "33");
    }
    #[test]
    fn native_resolver_does_not_require_go_or_hardware() {
        assert_eq!(
            resolve(SimIdentity {
                mcc: "234".into(),
                mnc: "33".into(),
                ..Default::default()
            })
            .status(),
            StatusCode::OK
        );
        assert_eq!(
            resolve(SimIdentity::default()).status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
    }
}
