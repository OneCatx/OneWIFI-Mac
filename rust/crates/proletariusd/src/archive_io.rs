//! File locking and atomic archive writes must not occupy async I/O workers.
use proletarius_store::StoreError;

pub(crate) async fn run<T: Send + 'static>(
    operation: impl FnOnce() -> Result<T, StoreError> + Send + 'static,
) -> Result<T, StoreError> {
    tokio::task::spawn_blocking(operation)
        .await
        .map_err(|_| StoreError::Io(std::io::Error::other("archive worker failed")))?
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test(flavor = "current_thread")]
    async fn blocking_archive_work_does_not_block_executor() {
        let (send, receive) = std::sync::mpsc::channel();
        let (started, ready) = tokio::sync::oneshot::channel();
        let task = tokio::spawn(run(move || {
            started.send(()).unwrap();
            receive
                .recv_timeout(std::time::Duration::from_secs(2))
                .map_err(std::io::Error::other)
                .map_err(StoreError::Io)?;
            Ok(42)
        }));
        ready.await.unwrap();
        send.send(()).unwrap();
        assert_eq!(task.await.unwrap().unwrap(), 42);
    }
}
