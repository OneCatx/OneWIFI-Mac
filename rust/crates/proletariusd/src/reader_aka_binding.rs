//! Immutable PC/SC identity binding for async native authentication clients.
use crate::reader_task::ReaderTaskError;
use proletarius_ims::aka_exchange::SimAnswer;
use proletarius_reader::{AkaApplication, AuthenticationError};
use proletarius_sim::AkaResult;
use std::{sync::Arc, time::Duration};
use zeroize::Zeroizing;

#[derive(Debug)]
#[allow(dead_code)] // Installed by the native IMS owner in the next gated wiring stage.
pub(crate) enum ImsReaderError {
    Worker(ReaderTaskError),
    Card(AuthenticationError),
}

#[allow(dead_code)] // Kept with authenticate_ims so late results remain zeroized before wiring.
fn ims_answer(result: Result<AkaResult, AuthenticationError>) -> Result<SimAnswer, ImsReaderError> {
    Ok(match result.map_err(ImsReaderError::Card)? {
        AkaResult::Success { res, ck, ik } => SimAnswer::Success {
            res: Zeroizing::new(res),
            ck: Zeroizing::new(ck),
            ik: Zeroizing::new(ik),
        },
        AkaResult::SynchronizationFailure { auts } => {
            SimAnswer::SynchronizationFailure(Zeroizing::new(auts))
        }
        AkaResult::AuthenticationReject => SimAnswer::Rejected,
    })
}

struct Binding {
    reader: String,
    application: AkaApplication,
    iccid: Zeroizing<String>,
    imsi: Zeroizing<String>,
}

/// Clones share an immutable identity, never a mutable "current reader" slot.
/// Deliberately no Debug/Serialize implementation for identifiers or secrets.
#[derive(Clone)]
pub(crate) struct ReaderAkaBinding(Arc<Binding>);

impl ReaderAkaBinding {
    pub(crate) fn new(
        reader: &str,
        application: AkaApplication,
        iccid: &str,
        imsi: &str,
    ) -> Result<Self, ()> {
        if reader.trim().is_empty()
            || reader.contains('\0')
            || !(18..=22).contains(&iccid.len())
            || !(5..=15).contains(&imsi.len())
            || !iccid
                .bytes()
                .chain(imsi.bytes())
                .all(|b| b.is_ascii_digit())
        {
            return Err(());
        }
        Ok(Self(Arc::new(Binding {
            reader: reader.trim().into(),
            application,
            iccid: Zeroizing::new(iccid.into()),
            imsi: Zeroizing::new(imsi.into()),
        })))
    }

    pub(crate) async fn authenticate(
        &self,
        rand: [u8; 16],
        autn: [u8; 16],
    ) -> Result<Result<AkaResult, AuthenticationError>, ReaderTaskError> {
        self.authenticate_with(
            rand,
            autn,
            |reader, application, iccid, imsi, rand, autn| {
                proletarius_reader::authenticate_aka_auto_for_identity(
                    reader,
                    application,
                    iccid,
                    imsi,
                    rand,
                    autn,
                )
            },
        )
        .await
    }

    /// The result is wrapped inside the worker so even a late response after
    /// cancellation is dropped with secret buffers zeroized.
    #[allow(dead_code)] // Exercised once the native registration owner is enabled.
    pub(crate) async fn authenticate_ims(
        &self,
        rand: Zeroizing<[u8; 16]>,
        autn: Zeroizing<[u8; 16]>,
    ) -> Result<SimAnswer, ImsReaderError> {
        self.authenticate_with(
            *rand,
            *autn,
            |reader, application, iccid, imsi, rand, autn| {
                ims_answer(proletarius_reader::authenticate_aka_auto_for_identity(
                    reader,
                    application,
                    iccid,
                    imsi,
                    rand,
                    autn,
                ))
            },
        )
        .await
        .map_err(ImsReaderError::Worker)?
    }

    async fn authenticate_with<F, T>(
        &self,
        rand: [u8; 16],
        autn: [u8; 16],
        operation: F,
    ) -> Result<T, ReaderTaskError>
    where
        F: FnOnce(&str, AkaApplication, &str, &str, &[u8; 16], &[u8; 16]) -> T + Send + 'static,
        T: Send + 'static,
    {
        let binding = self.0.clone();
        let rand = Zeroizing::new(rand);
        let autn = Zeroizing::new(autn);
        crate::reader_task::run(
            move || {
                operation(
                    &binding.reader,
                    binding.application,
                    &binding.iccid,
                    &binding.imsi,
                    &rand,
                    &autn,
                )
            },
            Duration::from_secs(35),
        )
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn ims_challenge_uses_bound_identity_and_preserves_card_errors() {
        use proletarius_ims::{
            SipResponse,
            aka_authorization::AuthorizationRequest,
            aka_exchange::{ExchangeError, answer_challenge},
        };
        let binding = ReaderAkaBinding::new(
            "Reader A",
            AkaApplication::Usim,
            "8901234567890123456",
            "234100896392986",
        )
        .unwrap();
        let response = SipResponse::parse(format!("SIP/2.0 401 Challenge\r\nWWW-Authenticate: Digest realm=\"ims.test\",nonce=\"{}\",algorithm=AKAv1-MD5\r\nContent-Length: 0\r\n\r\n", "01".repeat(32)).as_bytes()).unwrap();
        let request = AuthorizationRequest {
            username: "user",
            method: "REGISTER",
            uri: "sip:ims.test",
            body: b"",
            cnonce: "c",
            nonce_count: 1,
        };
        let (_stop_tx, stop) = tokio::sync::watch::channel(false);
        for identity_changed in [false, true] {
            let binding = binding.clone();
            let result = answer_challenge(
                &response,
                &request,
                tokio::time::Instant::now() + Duration::from_secs(1),
                stop.clone(),
                |rand, autn| async move {
                    binding
                        .authenticate_with(
                            *rand,
                            *autn,
                            move |reader, _, iccid, imsi, rand, autn| {
                                assert_eq!(reader, "Reader A");
                                assert_eq!(iccid, "8901234567890123456");
                                assert_eq!(imsi, "234100896392986");
                                assert_eq!(rand, &[1; 16]);
                                assert_eq!(autn, &[1; 16]);
                                ims_answer(if identity_changed {
                                    Err(AuthenticationError::IdentityChanged)
                                } else {
                                    Ok(AkaResult::Success {
                                        res: vec![1; 8],
                                        ck: [2; 16],
                                        ik: [3; 16],
                                    })
                                })
                            },
                        )
                        .await
                        .map_err(ImsReaderError::Worker)?
                },
            )
            .await;
            if identity_changed {
                assert!(matches!(
                    result,
                    Err(ExchangeError::Sim(ImsReaderError::Card(
                        AuthenticationError::IdentityChanged
                    )))
                ));
            } else {
                let result = result.unwrap();
                assert_eq!(result.header().name(), "Authorization");
                assert_eq!(*result.security_keys().unwrap().ik, [3; 16]);
            }
        }
    }
    #[tokio::test]
    async fn independent_bindings_keep_reader_identity_and_application() {
        let a = ReaderAkaBinding::new(
            " Reader A ",
            AkaApplication::Usim,
            "8901234567890123456",
            "234100896392986",
        )
        .unwrap();
        let b = ReaderAkaBinding::new(
            "Reader B",
            AkaApplication::Isim,
            "8901234567890123457",
            "234100896392987",
        )
        .unwrap();
        let a_clone = a.clone();
        drop(a);
        let one = a_clone.authenticate_with([1; 16], [2; 16], |r, app, iccid, imsi, rand, autn| {
            assert_eq!(r, "Reader A");
            assert_eq!(app, AkaApplication::Usim);
            assert_eq!(iccid, "8901234567890123456");
            assert_eq!(imsi, "234100896392986");
            assert_eq!(rand, &[1; 16]);
            assert_eq!(autn, &[2; 16]);
            Ok::<_, AuthenticationError>(AkaResult::AuthenticationReject)
        });
        let two = b.authenticate_with([3; 16], [4; 16], |r, app, iccid, imsi, rand, autn| {
            assert_eq!(r, "Reader B");
            assert_eq!(app, AkaApplication::Isim);
            assert_eq!(iccid, "8901234567890123457");
            assert_eq!(imsi, "234100896392987");
            assert_eq!(rand, &[3; 16]);
            assert_eq!(autn, &[4; 16]);
            Err::<AkaResult, _>(AuthenticationError::IdentityChanged)
        });
        let (one, two) = tokio::join!(one, two);
        assert!(matches!(one, Ok(Ok(AkaResult::AuthenticationReject))));
        assert!(matches!(two, Ok(Err(AuthenticationError::IdentityChanged))));
        assert!(
            ReaderAkaBinding::new(
                "reader\0",
                AkaApplication::Usim,
                "8901234567890123456",
                "234100896392986"
            )
            .is_err()
        );
    }
}
