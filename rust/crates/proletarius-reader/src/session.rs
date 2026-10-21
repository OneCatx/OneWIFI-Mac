//! Exclusive APDU ownership for the eventual native reader runtime.
//! Discovery must never construct this type while Go owns a reader.
use std::ffi::CString;
use std::fmt;

use pcsc::{Card, Context, Disposition, Protocols, Scope, ShareMode, Transaction};
use proletarius_sim::ApduTransport;

#[derive(Debug)]
pub enum SessionError {
    InvalidReaderName,
    InvalidCommandLength,
    Closed,
    Pcsc(pcsc::Error),
}

impl fmt::Display for SessionError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidReaderName => f.write_str("invalid PC/SC reader name"),
            Self::InvalidCommandLength => f.write_str("unsupported short APDU length"),
            Self::Closed => f.write_str("PC/SC session is closed"),
            Self::Pcsc(error) => write!(f, "PC/SC operation failed: {error}"),
        }
    }
}
impl std::error::Error for SessionError {}

pub struct PcscSession {
    card: Option<Card>,
}

pub struct PcscTransaction<'session>(Transaction<'session>);

impl PcscSession {
    /// Reader discovery and eSIM status refresh may legitimately keep another
    /// shared PC/SC handle alive.  Match macOS' normal smart-card ownership
    /// model here, then use `transaction()` to serialize the complete AKA APDU
    /// sequence without requiring every observer to disconnect first.
    pub fn connect(reader: &str) -> Result<Self, SessionError> {
        if reader.trim().is_empty() {
            return Err(SessionError::InvalidReaderName);
        }
        let name = CString::new(reader).map_err(|_| SessionError::InvalidReaderName)?;
        let context = Context::establish(Scope::System).map_err(SessionError::Pcsc)?;
        let card = context
            .connect(&name, ShareMode::Shared, Protocols::ANY)
            .map_err(SessionError::Pcsc)?;
        Ok(Self { card: Some(card) })
    }

    pub fn transaction(&mut self) -> Result<PcscTransaction<'_>, SessionError> {
        self.card
            .as_mut()
            .ok_or(SessionError::Closed)?
            .transaction()
            .map(PcscTransaction)
            .map_err(SessionError::Pcsc)
    }

    /// Explicit close reports failures and retains the handle for retry.
    pub fn close(&mut self) -> Result<(), SessionError> {
        if let Some(card) = self.card.take()
            && let Err((card, error)) = card.disconnect(Disposition::LeaveCard)
        {
            self.card = Some(card);
            return Err(SessionError::Pcsc(error));
        }
        Ok(())
    }
}

impl ApduTransport for PcscSession {
    type Error = SessionError;

    fn transmit(&mut self, command: &[u8]) -> Result<Vec<u8>, Self::Error> {
        if !(4..=261).contains(&command.len()) {
            return Err(SessionError::InvalidCommandLength);
        }
        let card = self.card.as_ref().ok_or(SessionError::Closed)?;
        let mut response = [0u8; 4098];
        card.transmit(command, &mut response)
            .map(<[u8]>::to_vec)
            .map_err(SessionError::Pcsc)
    }
}

impl ApduTransport for PcscTransaction<'_> {
    type Error = SessionError;

    fn transmit(&mut self, command: &[u8]) -> Result<Vec<u8>, Self::Error> {
        if !(4..=261).contains(&command.len()) {
            return Err(SessionError::InvalidCommandLength);
        }
        let mut response = [0u8; 4098];
        self.0
            .transmit(command, &mut response)
            .map(<[u8]>::to_vec)
            .map_err(SessionError::Pcsc)
    }
}

impl Drop for PcscSession {
    fn drop(&mut self) {
        let _ = self.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn closed_session_never_sends_commands() {
        let mut session = PcscSession { card: None };
        assert!(matches!(
            session.transmit(&[0, 0xa4, 0, 0]),
            Err(SessionError::Closed)
        ));
        assert!(matches!(
            session.transmit(&[]),
            Err(SessionError::InvalidCommandLength)
        ));
        session.close().unwrap();
        session.close().unwrap();
    }

    #[test]
    fn invalid_names_fail_before_pcsc_access() {
        for name in ["", "  ", "reader\0other"] {
            assert!(matches!(
                PcscSession::connect(name),
                Err(SessionError::InvalidReaderName)
            ));
        }
    }
}
