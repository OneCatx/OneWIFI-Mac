#![forbid(unsafe_code)]

use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use serde_json::Value;

pub const STORE_CONTRACT: &str = "atomic-json-archive-v1";
pub const DEFAULT_MAX_BYTES: u64 = 8 * 1024 * 1024;
pub const DEFAULT_MAX_ITEMS: usize = 1_000;

static TEMP_SEQUENCE: AtomicU64 = AtomicU64::new(1);

#[derive(Clone, Debug)]
pub struct ArchiveStore {
    path: PathBuf,
    max_bytes: u64,
    max_items: usize,
}

impl ArchiveStore {
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self {
            path: path.into(),
            max_bytes: DEFAULT_MAX_BYTES,
            max_items: DEFAULT_MAX_ITEMS,
        }
    }

    pub fn with_limits(
        path: impl Into<PathBuf>,
        max_bytes: u64,
        max_items: usize,
    ) -> Result<Self, StoreError> {
        if max_bytes == 0 || max_items == 0 {
            return Err(StoreError::InvalidLimit);
        }
        Ok(Self {
            path: path.into(),
            max_bytes,
            max_items,
        })
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    pub fn load(&self) -> Result<Vec<Value>, StoreError> {
        self.with_exclusive_lock(|store| store.load_unlocked())
    }

    pub fn replace(&self, mut items: Vec<Value>) -> Result<(), StoreError> {
        self.with_exclusive_lock(move |store| {
            store.bound_items(&mut items);
            store.persist_unlocked(&items)
        })
    }

    /// Delete the backing archive after a caller has durably transferred its
    /// contents elsewhere.  This deliberately uses the same per-file lock
    /// and symlink checks as `load`/`replace`: recovery journals must never
    /// turn a stale or swapped path into an arbitrary file deletion.
    pub fn remove(&self) -> Result<(), StoreError> {
        self.with_exclusive_lock(|store| {
            reject_non_regular(&store.path)?;
            match fs::remove_file(&store.path) {
                Ok(()) => Ok(()),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
                Err(error) => Err(StoreError::Io(error)),
            }
        })
    }

    pub fn mutate<T>(
        &self,
        mutate: impl FnOnce(&mut Vec<Value>) -> Result<T, StoreError>,
    ) -> Result<T, StoreError> {
        self.with_exclusive_lock(move |store| {
            let mut items = store.load_unlocked()?;
            let result = mutate(&mut items)?;
            store.bound_items(&mut items);
            store.persist_unlocked(&items)?;
            Ok(result)
        })
    }

    fn with_exclusive_lock<T>(
        &self,
        operation: impl FnOnce(&Self) -> Result<T, StoreError>,
    ) -> Result<T, StoreError> {
        let parent = self.parent()?;
        fs::create_dir_all(parent).map_err(StoreError::Io)?;
        fs::set_permissions(parent, fs::Permissions::from_mode(0o700)).map_err(StoreError::Io)?;
        let lock_path = self.lock_path()?;
        reject_non_regular(&lock_path)?;
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(&lock_path)
            .map_err(StoreError::Io)?;
        lock.lock().map_err(StoreError::Io)?;
        operation(self)
    }

    fn load_unlocked(&self) -> Result<Vec<Value>, StoreError> {
        reject_non_regular(&self.path)?;
        let file = match File::open(&self.path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(StoreError::Io(error)),
        };
        let length = file.metadata().map_err(StoreError::Io)?.len();
        if length > self.max_bytes {
            return Err(StoreError::TooLarge {
                actual: length,
                limit: self.max_bytes,
            });
        }
        let capacity = usize::try_from(length).map_err(|_| StoreError::TooLarge {
            actual: length,
            limit: self.max_bytes,
        })?;
        let mut bytes = Vec::with_capacity(capacity);
        file.take(self.max_bytes + 1)
            .read_to_end(&mut bytes)
            .map_err(StoreError::Io)?;
        if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > self.max_bytes {
            return Err(StoreError::TooLarge {
                actual: u64::try_from(bytes.len()).unwrap_or(u64::MAX),
                limit: self.max_bytes,
            });
        }
        if bytes.is_empty() {
            return Ok(Vec::new());
        }
        let value: Value = serde_json::from_slice(&bytes).map_err(StoreError::Json)?;
        // The legacy Go writer serializes a cleared nil slice as JSON null.
        if value.is_null() {
            return Ok(Vec::new());
        }
        let Value::Array(items) = value else {
            return Err(StoreError::RootIsNotArray);
        };
        if items.len() > self.max_items {
            return Err(StoreError::TooManyItems {
                actual: items.len(),
                limit: self.max_items,
            });
        }
        if items.iter().any(|item| !item.is_object()) {
            return Err(StoreError::ItemIsNotObject);
        }
        Ok(items)
    }

    fn persist_unlocked(&self, items: &[Value]) -> Result<(), StoreError> {
        if items.iter().any(|item| !item.is_object()) {
            return Err(StoreError::ItemIsNotObject);
        }
        let data = serde_json::to_vec(items).map_err(StoreError::Json)?;
        let length = u64::try_from(data.len()).unwrap_or(u64::MAX);
        if length > self.max_bytes {
            return Err(StoreError::TooLarge {
                actual: length,
                limit: self.max_bytes,
            });
        }
        reject_non_regular(&self.path)?;
        let temp = self.temp_path()?;
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&temp)
            .map_err(StoreError::Io)?;
        let result = (|| {
            file.write_all(&data).map_err(StoreError::Io)?;
            file.sync_all().map_err(StoreError::Io)?;
            fs::rename(&temp, &self.path).map_err(StoreError::Io)?;
            File::open(self.parent()?)
                .and_then(|directory| directory.sync_all())
                .map_err(StoreError::Io)
        })();
        if result.is_err() {
            let _ = fs::remove_file(&temp);
        }
        result
    }

    fn bound_items(&self, items: &mut Vec<Value>) {
        items.truncate(self.max_items);
    }

    fn parent(&self) -> Result<&Path, StoreError> {
        self.path.parent().ok_or(StoreError::MissingParent)
    }

    fn lock_path(&self) -> Result<PathBuf, StoreError> {
        let name = self
            .path
            .file_name()
            .ok_or(StoreError::MissingFileName)?
            .to_string_lossy();
        Ok(self.path.with_file_name(format!("{name}.lock")))
    }

    fn temp_path(&self) -> Result<PathBuf, StoreError> {
        let name = self
            .path
            .file_name()
            .ok_or(StoreError::MissingFileName)?
            .to_string_lossy();
        let sequence = TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
        Ok(self
            .path
            .with_file_name(format!(".{name}.{}.{}.tmp", std::process::id(), sequence)))
    }
}

fn reject_non_regular(path: &Path) -> Result<(), StoreError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => Err(StoreError::SymlinkRejected),
        Ok(metadata) if !metadata.file_type().is_file() => Err(StoreError::NotRegularFile),
        Ok(_) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(StoreError::Io(error)),
    }
}

#[derive(Debug)]
pub enum StoreError {
    InvalidLimit,
    Configuration(String),
    MissingParent,
    MissingFileName,
    SymlinkRejected,
    NotRegularFile,
    RootIsNotArray,
    ItemIsNotObject,
    TooLarge { actual: u64, limit: u64 },
    TooManyItems { actual: usize, limit: usize },
    Json(serde_json::Error),
    Io(std::io::Error),
}

impl fmt::Display for StoreError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidLimit => formatter.write_str("archive limits must be non-zero"),
            Self::Configuration(message) => formatter.write_str(message),
            Self::MissingParent => formatter.write_str("archive path has no parent"),
            Self::MissingFileName => formatter.write_str("archive path has no file name"),
            Self::SymlinkRejected => {
                formatter.write_str("archive path must not be a symbolic link")
            }
            Self::NotRegularFile => formatter.write_str("archive path is not a regular file"),
            Self::RootIsNotArray => formatter.write_str("archive root must be a JSON array"),
            Self::ItemIsNotObject => formatter.write_str("archive items must be JSON objects"),
            Self::TooLarge { actual, limit } => {
                write!(formatter, "archive is {actual} bytes, limit is {limit}")
            }
            Self::TooManyItems { actual, limit } => {
                write!(formatter, "archive has {actual} items, limit is {limit}")
            }
            Self::Json(error) => write!(formatter, "archive JSON: {error}"),
            Self::Io(error) => write!(formatter, "archive I/O: {error}"),
        }
    }
}

impl std::error::Error for StoreError {}

#[cfg(test)]
mod tests {
    use super::{ArchiveStore, StoreError, TEMP_SEQUENCE};
    use serde_json::{Value, json};
    use std::fs;
    use std::path::PathBuf;
    use std::sync::Arc;
    use std::sync::atomic::Ordering;
    use std::thread;
    use std::time::{SystemTime, UNIX_EPOCH};

    struct TestDirectory(PathBuf);

    impl TestDirectory {
        fn new() -> Self {
            let nonce = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            let path = std::env::temp_dir().join(format!(
                "proletarius-store-test-{}-{nonce}",
                std::process::id(),
                nonce = nonce + u128::from(TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed))
            ));
            fs::create_dir_all(&path).unwrap();
            Self(path)
        }

        fn archive(&self, name: &str) -> PathBuf {
            self.0.join(name)
        }
    }

    impl Drop for TestDirectory {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn reads_legacy_cleared_archive() {
        let directory = TestDirectory::new();
        let path = directory.archive("sms-archive.json");
        fs::write(&path, b"null\n").unwrap();
        let store = ArchiveStore::new(path);
        assert!(store.load().unwrap().is_empty());
    }

    #[test]
    fn reads_and_rewrites_go_sms_and_call_shapes_without_field_loss() {
        let directory = TestDirectory::new();
        for (name, fixture) in [
            (
                "sms-archive.json",
                json!([{
                    "id": "reader|fixture",
                    "sender": "888",
                    "content": "BAL",
                    "timestamp": "2026-09-01T12:03:52.123456789Z",
                    "archived": true,
                    "direction": "out",
                    "device": "reader",
                    "future_field": {"preserved": true}
                }]),
            ),
            (
                "call-log.json",
                json!([{
                    "id": "call-fixture",
                    "device": "reader",
                    "direction": "out",
                    "number": "888",
                    "answered": true,
                    "started_at": "2026-09-01T12:06:46Z",
                    "ended_at": "2026-09-01T12:07:04Z",
                    "duration": 18,
                    "future_field": [1, 2, 3]
                }]),
            ),
        ] {
            let path = directory.archive(name);
            fs::write(&path, serde_json::to_vec(&fixture).unwrap()).unwrap();
            let store = ArchiveStore::new(&path);
            let items = store.load().unwrap();
            store.replace(items).unwrap();
            let rewritten: Value = serde_json::from_slice(&fs::read(path).unwrap()).unwrap();
            assert_eq!(rewritten, fixture);
        }
    }

    #[test]
    fn concurrent_mutations_are_serialized_without_lost_updates() {
        let directory = TestDirectory::new();
        let store = Arc::new(ArchiveStore::new(directory.archive("sms-archive.json")));
        let mut workers = Vec::new();
        for index in 0..8 {
            let store = Arc::clone(&store);
            workers.push(thread::spawn(move || {
                store
                    .mutate(|items| {
                        items.push(json!({"id": index}));
                        Ok(())
                    })
                    .unwrap();
            }));
        }
        for worker in workers {
            worker.join().unwrap();
        }
        let mut ids = store
            .load()
            .unwrap()
            .iter()
            .map(|item| item["id"].as_u64().unwrap())
            .collect::<Vec<_>>();
        ids.sort_unstable();
        assert_eq!(ids, (0..8).collect::<Vec<_>>());
    }

    #[test]
    fn malformed_oversized_and_non_object_archives_are_rejected_intact() {
        let directory = TestDirectory::new();
        let path = directory.archive("sms-archive.json");
        let store = ArchiveStore::with_limits(&path, 32, 2).unwrap();
        fs::write(&path, b"{broken").unwrap();
        assert!(matches!(store.load(), Err(StoreError::Json(_))));
        assert_eq!(fs::read(&path).unwrap(), b"{broken");

        fs::write(&path, br#"[1]"#).unwrap();
        assert!(matches!(store.load(), Err(StoreError::ItemIsNotObject)));

        fs::write(&path, vec![b'x'; 33]).unwrap();
        assert!(matches!(
            store.load(),
            Err(StoreError::TooLarge {
                actual: 33,
                limit: 32
            })
        ));
    }

    #[test]
    fn newest_items_are_bounded_before_atomic_replace() {
        let directory = TestDirectory::new();
        let path = directory.archive("call-log.json");
        let store = ArchiveStore::with_limits(&path, 1024, 2).unwrap();
        store
            .replace(vec![json!({"id": 3}), json!({"id": 2}), json!({"id": 1})])
            .unwrap();
        assert_eq!(
            store.load().unwrap(),
            vec![json!({"id": 3}), json!({"id": 2})]
        );
    }
}
