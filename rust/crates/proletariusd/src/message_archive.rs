use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use proletarius_sms::stable_digest_hex;
use proletarius_store::{ArchiveStore, StoreError};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use serde_json::{Map, Value, json};

const MAX_REQUEST_BODY_BYTES: usize = 16 * 1024;

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    match (request.method(), request.uri().path()) {
        (&Method::GET, "/api/v2/sms-archive") => {
            match crate::archive_io::run(|| archive().and_then(|store| list(&store))).await {
                Ok(items) => crate::json_response(StatusCode::OK, &items),
                Err(error) => store_error(error),
            }
        }
        (&Method::POST, "/api/v2/sms-archive/clear") => {
            match crate::archive_io::run(|| archive().and_then(|store| store.replace(Vec::new())))
                .await
            {
                Ok(()) => crate::json_response(StatusCode::OK, &json!({"cleared": true})),
                Err(error) => store_error(error),
            }
        }
        (&Method::POST, "/api/v2/sms-archive/delete") => {
            let body: DeleteRequest = match decode_request(request).await {
                Ok(body) => body,
                Err(error) => return error.into_response(),
            };
            let id = body.id.trim();
            if id.is_empty() {
                return invalid_request("id is required");
            }
            let id = id.to_owned();
            match crate::archive_io::run(move || archive().and_then(|store| delete_id(&store, &id)))
                .await
            {
                Ok(deleted) => crate::json_response(
                    StatusCode::OK,
                    &json!({"deleted": deleted > 0, "deleted_count": deleted}),
                ),
                Err(error) => store_error(error),
            }
        }
        (&Method::POST, "/api/v2/sms-archive/delete-sender") => {
            let body: DeleteSenderRequest = match decode_request(request).await {
                Ok(body) => body,
                Err(error) => return error.into_response(),
            };
            let sender = body.sender.trim();
            if sender.is_empty() {
                return invalid_request("sender is required");
            }
            let sender = sender.to_owned();
            match crate::archive_io::run(move || {
                archive().and_then(|store| delete_sender(&store, &sender))
            })
            .await
            {
                Ok(deleted) => crate::json_response(
                    StatusCode::OK,
                    &json!({"deleted": deleted > 0, "deleted_count": deleted}),
                ),
                Err(error) => store_error(error),
            }
        }
        _ => crate::json_response(
            StatusCode::NOT_FOUND,
            &json!({
                "error": "Rust SMS archive route not found",
                "code": "route_not_found",
                "retryable": false
            }),
        ),
    }
}

fn list(store: &ArchiveStore) -> Result<Vec<Value>, StoreError> {
    let mut items = store.load()?;
    for item in &mut items {
        let id = stable_id(item);
        if let Some(object) = item.as_object_mut() {
            object.insert("id".into(), Value::String(id));
            object.insert("archived".into(), Value::Bool(true));
        }
    }
    Ok(items)
}

fn delete_id(store: &ArchiveStore, id: &str) -> Result<usize, StoreError> {
    store.mutate(|items| {
        let before = items.len();
        items.retain(|item| stable_id(item) != id);
        Ok(before - items.len())
    })
}

fn delete_sender(store: &ArchiveStore, sender: &str) -> Result<usize, StoreError> {
    store.mutate(|items| {
        let before = items.len();
        items.retain(|item| string_field(item, "sender").trim() != sender);
        Ok(before - items.len())
    })
}

fn stable_id(item: &Value) -> String {
    if let Some(id) = item
        .get("id")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|id| !id.is_empty())
    {
        return id.to_owned();
    }
    let module_index = item
        .get("module_index")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    let key = if module_index > 0 {
        format!(
            "{}\0{}\0{}\0{}",
            string_field(item, "device"),
            string_field(item, "module_storage").to_uppercase(),
            module_index,
            string_field(item, "content")
        )
    } else {
        format!(
            "{}\0{}\0{}\0{}",
            string_field(item, "device"),
            string_field(item, "sender"),
            string_field(item, "content"),
            string_field(item, "timestamp")
        )
    };
    stable_digest_hex(key.as_bytes())
}

fn string_field<'a>(item: &'a Value, field: &str) -> &'a str {
    item.get(field).and_then(Value::as_str).unwrap_or_default()
}

async fn decode_request<T: DeserializeOwned>(
    request: Request<Incoming>,
) -> Result<T, RequestDecodeError> {
    let mut body = request.into_body();
    let mut collected = Vec::new();
    while let Some(frame) = body.frame().await {
        let frame =
            frame.map_err(|error| RequestDecodeError::Invalid(format!("读取请求失败: {error}")))?;
        let Ok(data) = frame.into_data() else {
            continue;
        };
        let length = collected
            .len()
            .checked_add(data.len())
            .ok_or(RequestDecodeError::TooLarge)?;
        if length > MAX_REQUEST_BODY_BYTES {
            return Err(RequestDecodeError::TooLarge);
        }
        collected.extend_from_slice(&data);
    }
    serde_json::from_slice(&collected)
        .map_err(|_| RequestDecodeError::Invalid("请求格式错误".into()))
}

enum RequestDecodeError {
    Invalid(String),
    TooLarge,
}

impl RequestDecodeError {
    fn into_response(self) -> Response<Full<Bytes>> {
        match self {
            Self::Invalid(message) => invalid_request(&message),
            Self::TooLarge => request_too_large(),
        }
    }
}

fn request_too_large() -> Response<Full<Bytes>> {
    crate::json_response(
        StatusCode::PAYLOAD_TOO_LARGE,
        &json!({
            "error": "请求体过大",
            "code": "request_body_too_large",
            "retryable": false
        }),
    )
}

fn invalid_request(message: &str) -> Response<Full<Bytes>> {
    crate::json_response(
        StatusCode::BAD_REQUEST,
        &json!({
            "error": message,
            "code": "invalid_request_body",
            "retryable": false
        }),
    )
}

fn archive() -> Result<ArchiveStore, StoreError> {
    Ok(ArchiveStore::new(
        crate::data_root::resolve()
            .map_err(StoreError::Configuration)?
            .join("sms-archive.json"),
    ))
}

fn store_error(error: StoreError) -> Response<Full<Bytes>> {
    crate::json_response(
        StatusCode::INTERNAL_SERVER_ERROR,
        &json!({
            "error": format!("短信记录存储失败: {error}"),
            "code": "sms_archive_failed",
            "retryable": false
        }),
    )
}

#[derive(Deserialize)]
struct DeleteRequest {
    id: String,
    #[serde(flatten)]
    _future: Map<String, Value>,
}

#[derive(Deserialize)]
struct DeleteSenderRequest {
    sender: String,
    #[serde(flatten)]
    _future: Map<String, Value>,
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};

    use proletarius_store::ArchiveStore;
    use serde_json::{Value, json};

    use super::{delete_id, delete_sender, list, stable_id};

    static DIRECTORY_SEQUENCE: AtomicU64 = AtomicU64::new(0);

    struct TestDirectory(PathBuf);

    impl TestDirectory {
        fn new() -> Self {
            let nonce = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .expect("clock")
                .as_nanos();
            let sequence = DIRECTORY_SEQUENCE.fetch_add(1, Ordering::Relaxed);
            let path = std::env::temp_dir().join(format!(
                "proletarius-message-archive-{}-{nonce}-{sequence}",
                std::process::id()
            ));
            fs::create_dir_all(&path).expect("create test directory");
            Self(path)
        }

        fn store(&self) -> ArchiveStore {
            ArchiveStore::new(self.0.join("sms-archive.json"))
        }
    }

    impl Drop for TestDirectory {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn fixture(sender: &str, content: &str, timestamp: &str) -> Value {
        json!({
            "sender": sender,
            "content": content,
            "timestamp": timestamp,
            "direction": "in",
            "device": "reader",
            "archived": true,
            "future_field": {"preserve": true}
        })
    }

    #[test]
    fn list_adds_stable_ids_without_rewriting_unknown_fields() {
        let directory = TestDirectory::new();
        let store = directory.store();
        let item = fixture("888", "BALANCE", "2026-09-01T12:03:52.123456789Z");
        let expected_id = stable_id(&item);
        assert_eq!(
            expected_id,
            "2c513a5bd1137e6b4aaef5c126835b037292828d1bbc42e593f8e5d301e6ea69"
        );
        store.replace(vec![item.clone()]).expect("seed archive");

        let listed = list(&store).expect("list archive");
        assert_eq!(listed[0]["id"], expected_id);
        assert_eq!(listed[0]["future_field"], item["future_field"]);
        assert_eq!(store.load().expect("reload archive")[0], item);
    }

    #[test]
    fn delete_by_id_removes_only_the_exact_logical_message() {
        let directory = TestDirectory::new();
        let store = directory.store();
        let first = fixture("888", "same", "2026-09-01T12:00:00Z");
        let second = fixture("888", "same", "2026-09-01T12:00:01Z");
        let first_id = stable_id(&first);
        store
            .replace(vec![first, second.clone()])
            .expect("seed archive");

        assert_eq!(delete_id(&store, &first_id).expect("delete item"), 1);
        assert_eq!(store.load().expect("reload archive"), vec![second]);
    }

    #[test]
    fn delete_sender_is_trimmed_exact_and_preserves_other_records() {
        let directory = TestDirectory::new();
        let store = directory.store();
        let first = fixture("+447700900123", "one", "2026-09-01T12:00:00Z");
        let second = fixture("+447700900124", "two", "2026-09-01T12:00:01Z");
        store
            .replace(vec![first, second.clone()])
            .expect("seed archive");

        assert_eq!(
            delete_sender(&store, "+447700900123").expect("delete sender"),
            1
        );
        assert_eq!(store.load().expect("reload archive"), vec![second]);
    }
}
