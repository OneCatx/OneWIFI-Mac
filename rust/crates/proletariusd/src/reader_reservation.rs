//! Go owns every APDU while a multistandby reservation is active. Native
//! workers reserve admission before spawn and release it only when dropped by
//! the real blocking worker (including queued-task cancellation).
use bytes::Bytes;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Incoming};
use serde_json::{Value, json};
use std::sync::{Arc, Mutex, OnceLock};

pub(crate) const PREFIX: &str = "/api/internal/multistandby/reader-reservation/";
#[derive(Default)]
struct State {
    workers: usize,
    owner: Option<String>,
    last_released: Option<String>,
}
#[derive(Default)]
pub(crate) struct Gate(Mutex<State>);
pub(crate) struct WorkerPermit(Arc<Gate>);
impl Drop for WorkerPermit {
    fn drop(&mut self) {
        let mut state = self.0.0.lock().unwrap_or_else(|e| e.into_inner());
        state.workers = state.workers.saturating_sub(1);
    }
}
impl Gate {
    pub(crate) fn admit(self: &Arc<Self>) -> Result<WorkerPermit, ()> {
        let mut state = self.0.lock().map_err(|_| ())?;
        if state.owner.is_some() {
            return Err(());
        }
        state.workers = state.workers.checked_add(1).ok_or(())?;
        Ok(WorkerPermit(self.clone()))
    }
    fn acquire(&self, retry: Option<&str>) -> Result<String, &'static str> {
        let mut state = self.0.lock().map_err(|_| "reader_gate_failed")?;
        if let Some(owner) = state.owner.as_deref() {
            return if Some(owner) == retry {
                Ok(owner.to_owned())
            } else {
                Err("card_busy")
            };
        }
        if state.workers != 0 {
            return Err("card_busy");
        }
        if retry.is_some() && retry == state.last_released.as_deref() {
            return Err("owner_expired");
        }
        let owner = match retry {
            Some(value) => value.to_owned(),
            None => crate::multistandby_store::random_token().map_err(|_| "entropy_unavailable")?,
        };
        state.owner = Some(owner.clone());
        Ok(owner)
    }
    fn release(&self, owner: &str) -> Result<(), &'static str> {
        let mut state = self.0.lock().map_err(|_| "reader_gate_failed")?;
        if state.owner.as_deref() != Some(owner) {
            return if state.owner.is_none() && state.last_released.as_deref() == Some(owner) {
                Ok(())
            } else {
                Err("owner_mismatch")
            };
        }
        if state.workers != 0 {
            return Err("card_busy");
        }
        state.owner = None;
        state.last_released = Some(owner.to_owned());
        Ok(())
    }
}
pub(crate) fn gate() -> Arc<Gate> {
    static GATE: OnceLock<Arc<Gate>> = OnceLock::new();
    GATE.get_or_init(|| Arc::new(Gate::default())).clone()
}

/// Called only after the supervisor confirms that its exact Go child exited.
pub(crate) fn confirmed_go_exit() {
    let gate = gate();
    let mut state = gate.0.lock().unwrap_or_else(|e| e.into_inner());
    if let Some(owner) = state.owner.take() {
        state.last_released = Some(owner);
    }
}
pub(crate) async fn handle(request: Request<Incoming>) -> Response<Full<Bytes>> {
    let path = request.uri().path().to_owned();
    if request.method() != hyper::Method::POST {
        return reject("route_not_found", StatusCode::NOT_FOUND);
    }
    let body = match tokio::time::timeout(
        std::time::Duration::from_secs(2),
        Limited::new(request.into_body(), 4096).collect(),
    )
    .await
    {
        Ok(Ok(v)) => v.to_bytes(),
        _ => return reject("invalid_request", StatusCode::BAD_REQUEST),
    };
    let body: Value = match serde_json::from_slice(&body) {
        Ok(v) => v,
        _ => return reject("invalid_request", StatusCode::BAD_REQUEST),
    };
    let owner = body.get("owner_token").and_then(Value::as_str);
    if owner.is_some_and(|s| {
        s.len() < 32
            || s.len() > 128
            || !s
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || b"-_".contains(&c))
    }) {
        return reject("invalid_request", StatusCode::BAD_REQUEST);
    }
    let result = match path.strip_prefix(PREFIX) {
        Some("acquire") => gate()
            .acquire(owner)
            .map(|owner| json!({"owner_token":owner})),
        Some("release") => match owner {
            Some(owner) => gate()
                .release(owner)
                .map(|()| json!({"owner_token":owner,"released":true})),
            None => Err("invalid_request"),
        },
        _ => Err("route_not_found"),
    };
    match result {
        Ok(value) => crate::json_response(StatusCode::OK, &value),
        Err(code) => reject(
            code,
            if code == "invalid_request" {
                StatusCode::BAD_REQUEST
            } else {
                StatusCode::CONFLICT
            },
        ),
    }
}
fn reject(code: &str, status: StatusCode) -> Response<Full<Bytes>> {
    crate::json_response(
        status,
        &json!({"error":"读卡器占用请求未完成","code":code,"retryable":code=="card_busy"}),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn reservation_and_workers_are_atomic_and_owner_scoped() {
        let gate = Arc::new(Gate::default());
        let permit = gate.admit().unwrap();
        assert_eq!(gate.acquire(None), Err("card_busy"));
        drop(permit);
        let owner = gate.acquire(None).unwrap();
        assert!(gate.admit().is_err());
        assert_eq!(gate.acquire(Some(&owner)).unwrap(), owner);
        assert_eq!(gate.release("old-owner"), Err("owner_mismatch"));
        assert!(gate.admit().is_err());
        gate.release(&owner).unwrap();
        gate.release(&owner).unwrap();
        assert!(gate.admit().is_ok());
        let next = gate.acquire(None).unwrap();
        assert_ne!(next, owner);
        assert_eq!(gate.release(&owner), Err("owner_mismatch"));
    }
    #[tokio::test]
    async fn timed_out_blocking_worker_retains_admission_until_actual_return() {
        let gate = Arc::new(Gate::default());
        let worker_gate = gate.clone();
        let (send, recv) = std::sync::mpsc::channel();
        let (ready, started) = tokio::sync::oneshot::channel();
        let worker = tokio::spawn(crate::reader_task::run_with_gate(
            worker_gate,
            move || {
                ready.send(()).unwrap();
                recv.recv().unwrap();
            },
            std::time::Duration::from_millis(10),
        ));
        started.await.unwrap();
        assert_eq!(
            worker.await.unwrap(),
            Err(crate::reader_task::ReaderTaskError::Timeout)
        );
        assert_eq!(gate.acquire(None), Err("card_busy"));
        send.send(()).unwrap();
        for _ in 0..100 {
            if gate.acquire(None).is_ok() {
                return;
            }
            tokio::task::yield_now().await;
        }
        panic!("worker should release admission after finishing");
    }
}
