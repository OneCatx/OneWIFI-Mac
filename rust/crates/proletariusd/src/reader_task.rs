//! Cancellation-safe blocking reader work shared by native authentication APIs.
use std::time::Duration;

#[derive(Debug, Eq, PartialEq)]
pub(crate) enum ReaderTaskError {
    Timeout,
    WorkerFailed,
    Busy,
}

struct AbortQueuedOnDrop(tokio::task::AbortHandle);
impl Drop for AbortQueuedOnDrop {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// Cancellation prevents a queued operation from starting. A worker already
/// inside a synchronous hardware call is NOT killed: its transaction/lock
/// must remain owned until that call actually returns.
pub(crate) async fn run<F, T>(operation: F, deadline: Duration) -> Result<T, ReaderTaskError>
where
    F: FnOnce() -> T + Send + 'static,
    T: Send + 'static,
{
    run_with_gate(crate::reader_reservation::gate(), operation, deadline).await
}

pub(crate) async fn run_with_gate<F, T>(
    gate: std::sync::Arc<crate::reader_reservation::Gate>,
    operation: F,
    deadline: Duration,
) -> Result<T, ReaderTaskError>
where
    F: FnOnce() -> T + Send + 'static,
    T: Send + 'static,
{
    let permit = gate.admit().map_err(|_| ReaderTaskError::Busy)?;
    let mut task = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        operation()
    });
    let _guard = AbortQueuedOnDrop(task.abort_handle());
    tokio::time::timeout(deadline, &mut task)
        .await
        .map_err(|_| ReaderTaskError::Timeout)?
        .map_err(|_| ReaderTaskError::WorkerFailed)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    #[test]
    fn dropping_waiter_cancels_queued_physical_work() {
        use std::{
            future::{Future, poll_fn},
            sync::atomic::{AtomicBool, Ordering},
            task::Poll,
        };
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .max_blocking_threads(1)
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            let (release, wait) = std::sync::mpsc::channel();
            let (started, start) = tokio::sync::oneshot::channel();
            let blocker = tokio::task::spawn_blocking(move || {
                started.send(()).unwrap();
                wait.recv().unwrap();
            });
            start.await.unwrap();
            let called = Arc::new(AtomicBool::new(false));
            let worker_called = called.clone();
            let mut future = Box::pin(run(
                move || worker_called.store(true, Ordering::SeqCst),
                Duration::from_secs(2),
            ));
            let queued = poll_fn(|cx| Poll::Ready(future.as_mut().poll(cx).is_pending())).await;
            drop(future);
            release.send(()).unwrap();
            blocker.await.unwrap();
            tokio::task::spawn_blocking(|| ()).await.unwrap();
            assert!(queued);
            assert!(!called.load(Ordering::SeqCst));
        });
    }
    #[tokio::test]
    async fn cancellation_and_timeout_keep_running_worker_lock_until_return() {
        for cancel in [false, true] {
            let lock = Arc::new(Mutex::new(()));
            let worker_lock = lock.clone();
            let (started, start) = tokio::sync::oneshot::channel();
            let (release, wait) = std::sync::mpsc::channel();
            let (done, finished) = tokio::sync::oneshot::channel();
            let job = tokio::spawn(run(
                move || {
                    let guard = worker_lock.lock().unwrap();
                    started.send(()).unwrap();
                    wait.recv().unwrap();
                    drop(guard);
                    done.send(()).unwrap();
                },
                Duration::from_millis(100),
            ));
            start.await.unwrap();
            if cancel {
                job.abort();
                assert!(job.await.unwrap_err().is_cancelled());
            } else {
                assert_eq!(job.await.unwrap(), Err(ReaderTaskError::Timeout));
            }
            let held = lock.try_lock().is_err();
            release.send(()).unwrap();
            finished.await.unwrap();
            assert!(held, "waiter exit must not release hardware ownership");
            assert!(lock.try_lock().is_ok());
        }
    }
}
