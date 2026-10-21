//! Per-reader/SIM lifecycle core for the future native VoWiFi owner.
//!
//! This module intentionally has no transport, PC/SC, DNS, proxy, or task
//! creation dependency. It establishes the ownership invariants before a live
//! IKE/IMS worker is connected: a reader/SIM pair has one generation at a
//! time; cancellation is scoped to that generation; and a late worker result
//! cannot clear a replacement session.
//!
//! The live owner must keep this order when it consumes a stop signal:
//!
//! `quiesce new work -> RegisteredImsSession::shutdown() -> signal IKE stop
//! -> await this exact generation -> worker_finished`.
//!
//! Signalling IKE before the registered IMS session is shut down is unsafe:
//! its security-lease cleanup is serviced by the still-running encrypted
//! network owner.

#![allow(
    dead_code,
    reason = "The reducer is intentionally dormant until the native IKE/IMS runtime is gated on; it is fully unit-tested but has no public route yet."
)]

use std::{
    collections::HashMap,
    hash::{Hash, Hasher},
};

use tokio::sync::watch;
use zeroize::Zeroizing;

/// Non-serializable, non-debuggable key for one physical reader plus the card
/// identity observed during preparation. `sim_id` accepts ICCID when present
/// and IMSI only as the existing read-path fallback; callers must never obtain
/// it from an HTTP request.
pub(crate) struct ReaderSessionKey {
    reader: String,
    sim_id: Zeroizing<String>,
}

impl ReaderSessionKey {
    pub(crate) fn new(reader: &str, sim_id: &str) -> Result<Self, SessionKeyError> {
        let reader = reader.trim();
        let sim_id = sim_id.trim();
        if reader.is_empty()
            || reader.len() > 512
            || reader.bytes().any(|byte| byte.is_ascii_control())
            || !(5..=22).contains(&sim_id.len())
            || !sim_id.bytes().all(|byte| byte.is_ascii_digit())
        {
            return Err(SessionKeyError::Invalid);
        }
        Ok(Self {
            reader: reader.to_owned(),
            sim_id: Zeroizing::new(sim_id.to_owned()),
        })
    }
}

impl Clone for ReaderSessionKey {
    fn clone(&self) -> Self {
        Self {
            reader: self.reader.clone(),
            sim_id: Zeroizing::new(self.sim_id.as_str().to_owned()),
        }
    }
}

impl PartialEq for ReaderSessionKey {
    fn eq(&self, other: &Self) -> bool {
        self.reader == other.reader && self.sim_id.as_str() == other.sim_id.as_str()
    }
}

impl Eq for ReaderSessionKey {}

impl Hash for ReaderSessionKey {
    fn hash<H: Hasher>(&self, state: &mut H) {
        self.reader.hash(state);
        self.sim_id.as_str().hash(state);
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SessionKeyError {
    Invalid,
}

/// Opaque monotonically allocated worker generation. It is safe to expose in
/// non-subscriber diagnostics, but never substitutes for the reader/SIM key.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub(crate) struct SessionGeneration(u64);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum NativeSessionPhase {
    Prepared,
    Running,
    Stopping,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct NativeSessionSnapshot {
    pub(crate) generation: SessionGeneration,
    pub(crate) phase: NativeSessionPhase,
}

/// Held only by the future worker that owns the matching immutable session
/// context. It carries the scoped cancellation receiver, never a mutable
/// global "active reader" slot.
pub(crate) struct NativeSessionPermit {
    key: ReaderSessionKey,
    generation: SessionGeneration,
    stop: watch::Receiver<bool>,
}

impl NativeSessionPermit {
    pub(crate) const fn generation(&self) -> SessionGeneration {
        self.generation
    }

    pub(crate) fn stop_signal(&self) -> watch::Receiver<bool> {
        self.stop.clone()
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReserveError {
    Active,
    GenerationExhausted,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum StartError {
    Missing,
    StaleGeneration,
    NotPrepared,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum StopDisposition {
    Signalled,
    AlreadyStopping,
    Missing,
    StaleGeneration,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CompletionDisposition {
    Removed,
    Missing,
    StaleGeneration,
}

struct SessionEntry {
    generation: SessionGeneration,
    phase: NativeSessionPhase,
    stop: watch::Sender<bool>,
}

/// Pure ownership registry. It does not spawn workers and cannot start IKE,
/// read a card, resolve a host, or alter proxy state. The future actor inserts
/// one immutable `NativeSessionContext` alongside the returned permit.
#[derive(Default)]
pub(crate) struct NativeVoWiFiSupervisor {
    next_generation: u64,
    sessions: HashMap<ReaderSessionKey, SessionEntry>,
}

impl NativeVoWiFiSupervisor {
    pub(crate) fn reserve(
        &mut self,
        key: ReaderSessionKey,
    ) -> Result<NativeSessionPermit, ReserveError> {
        if self.sessions.contains_key(&key) {
            return Err(ReserveError::Active);
        }
        let generation = self.allocate_generation()?;
        let (stop, stop_receiver) = watch::channel(false);
        self.sessions.insert(
            key.clone(),
            SessionEntry {
                generation,
                phase: NativeSessionPhase::Prepared,
                stop,
            },
        );
        Ok(NativeSessionPermit {
            key,
            generation,
            stop: stop_receiver,
        })
    }

    pub(crate) fn mark_running(&mut self, permit: &NativeSessionPermit) -> Result<(), StartError> {
        let entry = self
            .sessions
            .get_mut(&permit.key)
            .ok_or(StartError::Missing)?;
        if entry.generation != permit.generation {
            return Err(StartError::StaleGeneration);
        }
        if entry.phase != NativeSessionPhase::Prepared {
            return Err(StartError::NotPrepared);
        }
        entry.phase = NativeSessionPhase::Running;
        Ok(())
    }

    /// Requests shutdown for exactly the matching worker. The worker performs
    /// IMS/TCP/security cleanup before it observes this IKE stop receiver.
    pub(crate) fn request_stop(&mut self, permit: &NativeSessionPermit) -> StopDisposition {
        let Some(entry) = self.sessions.get_mut(&permit.key) else {
            return StopDisposition::Missing;
        };
        if entry.generation != permit.generation {
            return StopDisposition::StaleGeneration;
        }
        if entry.phase == NativeSessionPhase::Stopping {
            return StopDisposition::AlreadyStopping;
        }
        entry.phase = NativeSessionPhase::Stopping;
        entry.stop.send_replace(true);
        StopDisposition::Signalled
    }

    /// Requests stopping every current session. It intentionally does not
    /// remove an entry; the matching worker must finish and report back before
    /// a replacement may reserve the same reader/SIM key.
    pub(crate) fn request_stop_all(&mut self) -> usize {
        let mut signalled = 0;
        for entry in self.sessions.values_mut() {
            if entry.phase != NativeSessionPhase::Stopping {
                entry.phase = NativeSessionPhase::Stopping;
                entry.stop.send_replace(true);
                signalled += 1;
            }
        }
        signalled
    }

    /// Removes only an exact completed worker generation. Late completions
    /// cannot erase a replacement session that reused the same reader/SIM.
    pub(crate) fn worker_finished(
        &mut self,
        permit: &NativeSessionPermit,
    ) -> CompletionDisposition {
        match self.sessions.get(&permit.key) {
            None => CompletionDisposition::Missing,
            Some(entry) if entry.generation != permit.generation => {
                CompletionDisposition::StaleGeneration
            }
            Some(_) => {
                self.sessions.remove(&permit.key);
                CompletionDisposition::Removed
            }
        }
    }

    pub(crate) fn snapshot(&self, key: &ReaderSessionKey) -> Option<NativeSessionSnapshot> {
        self.sessions.get(key).map(|entry| NativeSessionSnapshot {
            generation: entry.generation,
            phase: entry.phase,
        })
    }

    fn allocate_generation(&mut self) -> Result<SessionGeneration, ReserveError> {
        let next = self
            .next_generation
            .checked_add(1)
            .ok_or(ReserveError::GenerationExhausted)?;
        self.next_generation = next;
        Ok(SessionGeneration(next))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(reader: &str, sim: &str) -> ReaderSessionKey {
        ReaderSessionKey::new(reader, sim).unwrap()
    }

    #[test]
    fn key_rejects_control_characters_and_unusable_card_identity() {
        for (reader, sim) in [
            ("", "8901234567890123456"),
            ("Reader\nA", "8901234567890123456"),
            ("Reader A", "1234"),
            ("Reader A", "89012345678901234x"),
        ] {
            assert!(matches!(
                ReaderSessionKey::new(reader, sim),
                Err(SessionKeyError::Invalid)
            ));
        }
    }

    #[test]
    fn duplicate_reservation_is_rejected_but_independent_reader_sim_pairs_isolate() {
        let first_key = key("Reader A", "8901234567890123456");
        let second_key = key("Reader B", "8901234567890123457");
        let mut supervisor = NativeVoWiFiSupervisor::default();
        let first = supervisor.reserve(first_key.clone()).unwrap();
        assert!(matches!(
            supervisor.reserve(first_key),
            Err(ReserveError::Active)
        ));
        let second = supervisor.reserve(second_key.clone()).unwrap();
        assert_ne!(first.generation(), second.generation());
        assert_eq!(
            supervisor.snapshot(&second_key).unwrap().phase,
            NativeSessionPhase::Prepared
        );
    }

    #[test]
    fn only_the_matching_session_receives_stop_and_duplicate_start_is_rejected() {
        let first_key = key("Reader A", "8901234567890123456");
        let second_key = key("Reader B", "8901234567890123457");
        let mut supervisor = NativeVoWiFiSupervisor::default();
        let first = supervisor.reserve(first_key.clone()).unwrap();
        let second = supervisor.reserve(second_key.clone()).unwrap();
        let first_stop = first.stop_signal();
        let second_stop = second.stop_signal();

        supervisor.mark_running(&first).unwrap();
        assert_eq!(
            supervisor.mark_running(&first),
            Err(StartError::NotPrepared)
        );
        assert_eq!(supervisor.request_stop(&first), StopDisposition::Signalled);
        assert!(*first_stop.borrow());
        assert!(!*second_stop.borrow());
        assert_eq!(
            supervisor.snapshot(&first_key).unwrap().phase,
            NativeSessionPhase::Stopping
        );
        assert_eq!(
            supervisor.snapshot(&second_key).unwrap().phase,
            NativeSessionPhase::Prepared
        );
    }

    #[test]
    fn stale_completion_cannot_remove_a_replacement_generation() {
        let session_key = key("Reader A", "8901234567890123456");
        let mut supervisor = NativeVoWiFiSupervisor::default();
        let first = supervisor.reserve(session_key.clone()).unwrap();
        supervisor.mark_running(&first).unwrap();
        assert_eq!(supervisor.request_stop(&first), StopDisposition::Signalled);
        assert_eq!(
            supervisor.worker_finished(&first),
            CompletionDisposition::Removed
        );

        let replacement = supervisor.reserve(session_key.clone()).unwrap();
        supervisor.mark_running(&replacement).unwrap();
        assert_eq!(
            supervisor.worker_finished(&first),
            CompletionDisposition::StaleGeneration
        );
        assert_eq!(
            supervisor.snapshot(&session_key).unwrap(),
            NativeSessionSnapshot {
                generation: replacement.generation(),
                phase: NativeSessionPhase::Running,
            }
        );
    }

    #[test]
    fn shutdown_all_is_idempotent_and_retains_sessions_until_exact_worker_completion() {
        let first_key = key("Reader A", "8901234567890123456");
        let second_key = key("Reader B", "8901234567890123457");
        let mut supervisor = NativeVoWiFiSupervisor::default();
        let first = supervisor.reserve(first_key.clone()).unwrap();
        let second = supervisor.reserve(second_key.clone()).unwrap();
        let first_stop = first.stop_signal();
        let second_stop = second.stop_signal();
        supervisor.mark_running(&first).unwrap();
        supervisor.mark_running(&second).unwrap();

        assert_eq!(supervisor.request_stop_all(), 2);
        assert_eq!(supervisor.request_stop_all(), 0);
        assert!(*first_stop.borrow());
        assert!(*second_stop.borrow());
        assert!(supervisor.snapshot(&first_key).is_some());
        assert_eq!(
            supervisor.worker_finished(&first),
            CompletionDisposition::Removed
        );
        assert!(supervisor.snapshot(&first_key).is_none());
        assert!(supervisor.snapshot(&second_key).is_some());
        assert_eq!(
            supervisor.worker_finished(&second),
            CompletionDisposition::Removed
        );
    }

    #[test]
    fn stop_during_preparation_prevents_late_worker_start() {
        let session_key = key("Reader A", "8901234567890123456");
        let mut supervisor = NativeVoWiFiSupervisor::default();
        let permit = supervisor.reserve(session_key.clone()).unwrap();
        let stop = permit.stop_signal();

        assert_eq!(supervisor.request_stop(&permit), StopDisposition::Signalled);
        assert!(*stop.borrow());
        assert_eq!(
            supervisor.mark_running(&permit),
            Err(StartError::NotPrepared)
        );
        assert_eq!(
            supervisor.snapshot(&session_key).unwrap().phase,
            NativeSessionPhase::Stopping
        );
        assert_eq!(
            supervisor.worker_finished(&permit),
            CompletionDisposition::Removed
        );
    }
}
