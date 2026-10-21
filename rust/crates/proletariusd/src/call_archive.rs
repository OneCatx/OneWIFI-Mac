use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::{Method, Request, Response, StatusCode};
use proletarius_store::{ArchiveStore, StoreError};
use serde::Deserialize;
use serde_json::{Value, json};

const MAX_DELETE_BODY_BYTES: usize = 16 * 1024;

pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    match (request.method(), request.uri().path()) {
        (&Method::GET, "/api/calls") => {
            match crate::archive_io::run(|| archive().and_then(|store| list(&store))).await {
                Ok(items) => crate::json_response(StatusCode::OK, &items),
                Err(error) => store_error(error),
            }
        }
        (&Method::POST, "/api/calls/clear") => {
            match crate::archive_io::run(|| archive().and_then(|store| clear(&store))).await {
                Ok(()) => crate::json_response(StatusCode::OK, &json!({"ok": true})),
                Err(error) => store_error(error),
            }
        }
        (&Method::POST, "/api/calls/delete") => delete(request).await,
        _ => crate::json_response(
            StatusCode::NOT_FOUND,
            &json!({
                "error": "Rust call archive route not found",
                "code": "route_not_found",
                "retryable": false
            }),
        ),
    }
}

async fn delete(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let mut body = request.into_body();
    let mut collected = Vec::new();
    while let Some(frame) = body.frame().await {
        let frame = match frame {
            Ok(frame) => frame,
            Err(error) => {
                return crate::json_response(
                    StatusCode::BAD_REQUEST,
                    &json!({
                        "error": format!("读取请求失败: {error}"),
                        "code": "invalid_request_body",
                        "retryable": false
                    }),
                );
            }
        };
        let Ok(data) = frame.into_data() else {
            continue;
        };
        let Some(length) = collected.len().checked_add(data.len()) else {
            return request_too_large();
        };
        if length > MAX_DELETE_BODY_BYTES {
            return request_too_large();
        }
        collected.extend_from_slice(&data);
    }
    let body: DeleteRequest = match serde_json::from_slice(&collected) {
        Ok(body) => body,
        Err(_) => {
            return crate::json_response(
                StatusCode::BAD_REQUEST,
                &json!({
                    "error": "请求格式错误",
                    "code": "invalid_request_body",
                    "retryable": false
                }),
            );
        }
    };
    let id = body.id.trim();
    if id.is_empty() {
        return crate::json_response(
            StatusCode::BAD_REQUEST,
            &json!({
                "error": "id is required",
                "code": "invalid_request_body",
                "retryable": false
            }),
        );
    }
    let id = id.to_owned();
    match crate::archive_io::run(move || {
        archive().and_then(|store| delete_id(&store, &id).map(|_| ()))
    })
    .await
    {
        Ok(()) => crate::json_response(StatusCode::OK, &json!({"ok": true})),
        Err(error) => store_error(error),
    }
}

fn list(store: &ArchiveStore) -> Result<Vec<Value>, StoreError> {
    store.load()
}

fn clear(store: &ArchiveStore) -> Result<(), StoreError> {
    store.replace(Vec::new())
}

fn delete_id(store: &ArchiveStore, id: &str) -> Result<usize, StoreError> {
    store.mutate(|items| {
        let before = items.len();
        items.retain(|item| item.get("id").and_then(Value::as_str) != Some(id));
        Ok(before - items.len())
    })
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

fn archive() -> Result<ArchiveStore, StoreError> {
    Ok(ArchiveStore::new(
        crate::data_root::resolve()
            .map_err(StoreError::Configuration)?
            .join("call-log.json"),
    ))
}

fn store_error(error: StoreError) -> Response<Full<Bytes>> {
    crate::json_response(
        StatusCode::INTERNAL_SERVER_ERROR,
        &json!({
            "error": format!("通话记录存储失败: {error}"),
            "code": "call_archive_failed",
            "retryable": false
        }),
    )
}

#[derive(Deserialize)]
struct DeleteRequest {
    id: String,
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};

    use proletarius_store::ArchiveStore;
    use serde_json::{Value, json};

    use super::{clear, delete_id, list};

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
                "proletarius-call-archive-{}-{nonce}-{sequence}",
                std::process::id()
            ));
            fs::create_dir_all(&path).expect("create test directory");
            Self(path)
        }

        fn store(&self) -> ArchiveStore {
            ArchiveStore::new(self.0.join("call-log.json"))
        }
    }

    impl Drop for TestDirectory {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn record(id: &str, number: &str) -> Value {
        json!({
            "id": id,
            "number": number,
            "direction": "out",
            "device": "reader",
            "future_field": {"preserve": true}
        })
    }

    #[test]
    fn list_preserves_order_and_unknown_fields() {
        let directory = TestDirectory::new();
        let store = directory.store();
        let records = vec![record("new", "888"), record("old", "+441234")];
        store.replace(records.clone()).expect("seed archive");

        assert_eq!(list(&store).expect("list archive"), records);
    }

    #[test]
    fn delete_removes_only_exact_id_and_reports_count() {
        let directory = TestDirectory::new();
        let store = directory.store();
        let retained = record("call-20", "+441234");
        store
            .replace(vec![record("call-2", "888"), retained.clone()])
            .expect("seed archive");

        assert_eq!(delete_id(&store, "call-2").expect("delete"), 1);
        assert_eq!(delete_id(&store, "missing").expect("delete missing"), 0);
        assert_eq!(list(&store).expect("reload archive"), vec![retained]);
    }

    #[test]
    fn clear_commits_an_empty_archive() {
        let directory = TestDirectory::new();
        let store = directory.store();
        store
            .replace(vec![record("call-1", "888")])
            .expect("seed archive");

        clear(&store).expect("clear archive");
        assert!(list(&store).expect("reload archive").is_empty());
    }
}
