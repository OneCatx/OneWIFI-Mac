//! Read-only bridge to the stable device identity owned by the compatibility
//! backend during migration.
//!
//! The existing Go owner creates and persists this value. Rust may consume it
//! for an immutable native IMS session, but must not generate, rotate, log, or
//! write it until the Go owner has been retired. Two writers would create a
//! different SIP instance and break carrier-side terminating-call binding.

use std::{fs, path::PathBuf};

use serde::Deserialize;
use zeroize::{Zeroize, Zeroizing};

const MAX_CONFIG_BYTES: u64 = 4 * 1024;

#[derive(Debug)]
pub(crate) enum PersistedDeviceIdentityError {
    Unreadable,
    Oversized,
}

#[derive(Deserialize)]
struct GoImeiConfig {
    #[serde(default)]
    imei: String,
}

/// Loads only the exact compatibility-backend configuration location. Missing,
/// empty, or malformed JSON is treated like Go's `loadIMEI`: no identity is
/// available. A caller must then fail its native session preparation rather
/// than inventing a replacement value.
pub(crate) fn load_go_persisted_imei()
-> Result<Option<Zeroizing<String>>, PersistedDeviceIdentityError> {
    let path = go_imei_config_path()?;
    let metadata = match fs::metadata(&path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(PersistedDeviceIdentityError::Unreadable),
    };
    if !metadata.is_file() {
        return Err(PersistedDeviceIdentityError::Unreadable);
    }
    if metadata.len() > MAX_CONFIG_BYTES {
        return Err(PersistedDeviceIdentityError::Oversized);
    }
    let bytes =
        Zeroizing::new(fs::read(path).map_err(|_| PersistedDeviceIdentityError::Unreadable)?);
    Ok(parse_go_imei_config(&bytes))
}

fn go_imei_config_path() -> Result<PathBuf, PersistedDeviceIdentityError> {
    Ok(crate::data_root::resolve()
        .map_err(|_| PersistedDeviceIdentityError::Unreadable)?
        .join("imei.json"))
}

fn parse_go_imei_config(bytes: &[u8]) -> Option<Zeroizing<String>> {
    if bytes.iter().all(u8::is_ascii_whitespace) {
        return None;
    }
    // The on-disk buffer is zeroized at scope exit. After a successful Serde
    // decode, explicitly zero its temporary `String` after copying the
    // non-empty value into the guarded return type. This is a read-only
    // compatibility bridge, not a complete in-memory isolation boundary.
    let mut configuration = serde_json::from_slice::<GoImeiConfig>(bytes).ok()?;
    let result = (!configuration.imei.trim().is_empty())
        .then(|| Zeroizing::new(configuration.imei.trim().to_owned()));
    configuration.imei.zeroize();
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_the_existing_go_shape_without_exposing_a_new_writer() {
        let value = parse_go_imei_config(br#"{"imei":" 353936932685355 "}"#).unwrap();
        assert_eq!(value.as_str(), "353936932685355");
    }

    #[test]
    fn missing_or_malformed_compatibility_config_is_not_replaced() {
        for input in [b"".as_slice(), b"{}", b"{not-json}", b"{\"imei\":\"\"}"] {
            assert!(parse_go_imei_config(input).is_none());
        }
    }

    #[test]
    fn fallback_path_keeps_the_go_compatibility_filename() {
        let path = go_imei_config_path().expect("configured Switch data path");
        assert!(path.ends_with("imei.json"));
    }
}
