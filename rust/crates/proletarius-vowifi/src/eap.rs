use std::fmt;

use proletarius_ike::eap_aka::{
    AkaCheckcodeMode, EapAkaChallenge, EapAkaError, EapAkaResponse,
    answer_eap_aka_challenge_with_checkcode, build_eap_aka_sync_failure, parse_eap_aka_challenge,
};
use zeroize::Zeroize;
use zeroize::Zeroizing;

const EAP_PAYLOAD: u8 = 48;
const EAP_REQUEST: u8 = 1;
const EAP_RESPONSE: u8 = 2;
const EAP_IDENTITY: u8 = 1;
const EAP_AKA: u8 = 23;
const EAP_AKA_CHALLENGE: u8 = 1;
const MAX_EAP_PACKET: usize = 4096;
const MAX_IDENTITY: usize = 2048;

#[derive(Debug)]
pub enum Ikev2EapError {
    MissingIdentity,
    InvalidPacket,
    UnsupportedMethod(u8),
    UnsupportedAkaSubtype(u8),
    Aka(EapAkaError),
}

impl fmt::Display for Ikev2EapError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingIdentity => formatter.write_str("EAP identity is empty or too long"),
            Self::InvalidPacket => formatter.write_str("invalid IKEv2 EAP request payload"),
            Self::UnsupportedMethod(method) => {
                write!(formatter, "unsupported EAP method {method}")
            }
            Self::UnsupportedAkaSubtype(subtype) => {
                write!(formatter, "unsupported EAP-AKA subtype {subtype}")
            }
            Self::Aka(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for Ikev2EapError {}

impl From<EapAkaError> for Ikev2EapError {
    fn from(value: EapAkaError) -> Self {
        Self::Aka(value)
    }
}

/// One AKA outcome returned by the selected SIM service. Secret fields are
/// erased on drop, including when EAP response construction fails.
pub enum SimAkaResult {
    Success {
        res: Vec<u8>,
        ck: [u8; 16],
        ik: [u8; 16],
    },
    SynchronizationFailure {
        auts: [u8; 14],
    },
    AuthenticationRejected,
}

impl fmt::Debug for SimAkaResult {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let mut debug = formatter.debug_struct("SimAkaResult");
        match self {
            Self::Success { .. } => debug.field("outcome", &"success").finish(),
            Self::SynchronizationFailure { .. } => {
                debug.field("outcome", &"synchronization_failure").finish()
            }
            Self::AuthenticationRejected => {
                debug.field("outcome", &"authentication_rejected").finish()
            }
        }
    }
}

impl Drop for SimAkaResult {
    fn drop(&mut self) {
        match self {
            Self::Success { res, ck, ik } => {
                res.zeroize();
                ck.zeroize();
                ik.zeroize();
            }
            Self::SynchronizationFailure { auts } => auts.zeroize(),
            Self::AuthenticationRejected => {}
        }
    }
}

#[derive(Debug)]
pub enum EapAkaExchangeResult {
    Challenge(EapAkaResponse),
    SynchronizationFailure(Zeroizing<Vec<u8>>),
}

#[derive(Debug)]
pub enum EapAkaExchangeError<E> {
    Eap(Ikev2EapError),
    SimService(E),
    AuthenticationRejected,
}

/// Bounded EAP bridge for the IKE_AUTH loop. The caller remains responsible
/// for obtaining RES/CK/IK from the selected SIM through the reader service.
/// Identity transcript bytes are erased when this conversation is dropped.
pub struct Ikev2EapConversation {
    identity: Zeroizing<Vec<u8>>,
    identity_transcript: Zeroizing<Vec<u8>>,
    checkcode_mode: AkaCheckcodeMode,
}

impl fmt::Debug for Ikev2EapConversation {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Ikev2EapConversation")
            .field("identity", &"[REDACTED]")
            .field("identity_transcript", &"[REDACTED]")
            .field("checkcode_mode", &self.checkcode_mode)
            .finish()
    }
}

impl Ikev2EapConversation {
    pub fn new(identity: &[u8], checkcode_mode: AkaCheckcodeMode) -> Result<Self, Ikev2EapError> {
        if identity.is_empty() || identity.len() > MAX_IDENTITY {
            return Err(Ikev2EapError::MissingIdentity);
        }
        Ok(Self {
            identity: Zeroizing::new(identity.to_vec()),
            identity_transcript: Zeroizing::new(Vec::new()),
            checkcode_mode,
        })
    }

    /// Restores a conversation from an already completed EAP Identity round.
    /// Both transcript packets are parsed and the response must contain this
    /// exact identity, preventing caller-supplied CHECKCODE state from being
    /// attached to another SIM identity.
    pub fn with_identity_transcript(
        identity: &[u8],
        transcript: &[u8],
        checkcode_mode: AkaCheckcodeMode,
    ) -> Result<Self, Ikev2EapError> {
        if identity.is_empty() || identity.len() > MAX_IDENTITY || transcript.len() < 10 {
            return Err(Ikev2EapError::InvalidPacket);
        }
        let request_length = usize::from(u16::from_be_bytes([transcript[2], transcript[3]]));
        if !(5..=MAX_EAP_PACKET).contains(&request_length) || request_length >= transcript.len() {
            return Err(Ikev2EapError::InvalidPacket);
        }
        let (request_id, _) = parse_identity_message(&transcript[..request_length], EAP_REQUEST)?;
        let (response_id, response_identity) =
            parse_identity_message(&transcript[request_length..], EAP_RESPONSE)?;
        if request_id != response_id || response_identity != identity {
            return Err(Ikev2EapError::InvalidPacket);
        }
        let mut conversation = Self::new(identity, checkcode_mode)?;
        conversation
            .identity_transcript
            .extend_from_slice(transcript);
        Ok(conversation)
    }

    /// Recreates the conversation directly from its two-message Identity
    /// transcript, taking the identity only from the validated response.
    pub fn from_identity_transcript(
        transcript: &[u8],
        checkcode_mode: AkaCheckcodeMode,
    ) -> Result<Self, Ikev2EapError> {
        if transcript.len() < 10 {
            return Err(Ikev2EapError::InvalidPacket);
        }
        let request_length = usize::from(u16::from_be_bytes([transcript[2], transcript[3]]));
        if !(5..=MAX_EAP_PACKET).contains(&request_length) || request_length >= transcript.len() {
            return Err(Ikev2EapError::InvalidPacket);
        }
        let (_, identity) = parse_identity_message(&transcript[request_length..], EAP_RESPONSE)?;
        Self::with_identity_transcript(identity, transcript, checkcode_mode)
    }

    /// Responds to an EAP Identity request and stores the exact request and
    /// response bytes used by RFC 4187 AT_CHECKCODE.
    pub fn respond_identity(&mut self, request: &[u8]) -> Result<Vec<u8>, Ikev2EapError> {
        let (identifier, method, _) = parse_request(request)?;
        if method != EAP_IDENTITY {
            return Err(Ikev2EapError::UnsupportedMethod(method));
        }
        let length = 5_usize
            .checked_add(self.identity.len())
            .filter(|length| *length <= MAX_EAP_PACKET && *length <= u16::MAX as usize)
            .ok_or(Ikev2EapError::InvalidPacket)?;
        let mut response = Vec::with_capacity(length);
        response.extend_from_slice(&[EAP_RESPONSE, identifier]);
        response.extend_from_slice(&(length as u16).to_be_bytes());
        response.push(EAP_IDENTITY);
        response.extend_from_slice(&self.identity);

        self.identity_transcript.clear();
        self.identity_transcript.extend_from_slice(request);
        self.identity_transcript.extend_from_slice(&response);
        Ok(response)
    }

    /// Validates and answers EAP-AKA Challenge using only the selected SIM's
    /// successful AKA result. Synchronization-failure/AUTS handling remains a
    /// separate explicit SIM operation and is never guessed here.
    pub fn answer_challenge(
        &self,
        request: &[u8],
        res: &[u8],
        ck: &[u8],
        ik: &[u8],
    ) -> Result<EapAkaResponse, Ikev2EapError> {
        self.challenge_inputs(request)?;
        Ok(answer_eap_aka_challenge_with_checkcode(
            request,
            &self.identity,
            res,
            ck,
            ik,
            &self.identity_transcript,
            self.checkcode_mode,
        )?)
    }

    /// Parses RAND/AUTN and requires AT_MAC before the caller spends a PC/SC
    /// transaction on the selected SIM.
    pub fn challenge_inputs(&self, request: &[u8]) -> Result<EapAkaChallenge, Ikev2EapError> {
        let (_, method, body) = parse_request(request)?;
        if method != EAP_AKA {
            return Err(Ikev2EapError::UnsupportedMethod(method));
        }
        if body.len() < 3 {
            return Err(Ikev2EapError::InvalidPacket);
        }
        let subtype = body[0];
        if subtype != EAP_AKA_CHALLENGE {
            return Err(Ikev2EapError::UnsupportedAkaSubtype(subtype));
        }
        if body[1..3] != [0, 0] {
            return Err(Ikev2EapError::InvalidPacket);
        }
        Ok(parse_eap_aka_challenge(request)?)
    }

    /// Responds to SIM sequence-number synchronization failure without
    /// converting AUTS into an ordinary AKA success or retrying silently.
    pub fn answer_sync_failure(
        &self,
        request: &[u8],
        auts: &[u8],
    ) -> Result<Vec<u8>, Ikev2EapError> {
        let challenge = self.challenge_inputs(request)?;
        Ok(build_eap_aka_sync_failure(challenge.identifier, auts)?)
    }

    /// Validates the challenge before invoking the selected SIM service, then
    /// calls it exactly once. The provider should run synchronous PC/SC work
    /// on a blocking worker and return only the selected card's AKA result.
    pub fn answer_challenge_with_sim<F, E>(
        &self,
        request: &[u8],
        authenticate: F,
    ) -> Result<EapAkaExchangeResult, EapAkaExchangeError<E>>
    where
        F: FnOnce(&[u8; 16], &[u8; 16]) -> Result<SimAkaResult, E>,
    {
        let challenge = self
            .challenge_inputs(request)
            .map_err(EapAkaExchangeError::Eap)?;
        let mut result = authenticate(&challenge.rand, &challenge.autn)
            .map_err(EapAkaExchangeError::SimService)?;
        match &mut result {
            SimAkaResult::Success { res, ck, ik } => self
                .answer_challenge(request, res, ck, ik)
                .map(EapAkaExchangeResult::Challenge)
                .map_err(EapAkaExchangeError::Eap),
            SimAkaResult::SynchronizationFailure { auts } => self
                .answer_sync_failure(request, auts)
                .map(|packet| EapAkaExchangeResult::SynchronizationFailure(Zeroizing::new(packet)))
                .map_err(EapAkaExchangeError::Eap),
            SimAkaResult::AuthenticationRejected => {
                Err(EapAkaExchangeError::AuthenticationRejected)
            }
        }
    }
}

/// Extracts exactly one EAP payload from an IKE_AUTH payload set.
pub fn eap_request_from_ike(
    frames: &[proletarius_ike::PayloadFrame],
) -> Result<&[u8], Ikev2EapError> {
    let mut eap = frames.iter().filter(|frame| frame.kind == EAP_PAYLOAD);
    let body = &eap.next().ok_or(Ikev2EapError::InvalidPacket)?.body;
    if eap.next().is_some() {
        return Err(Ikev2EapError::InvalidPacket);
    }
    parse_request(body)?;
    Ok(body)
}

pub fn eap_response_frame(packet: Vec<u8>) -> Result<proletarius_ike::PayloadFrame, Ikev2EapError> {
    if packet.len() < 5
        || packet.len() > MAX_EAP_PACKET
        || packet[0] != EAP_RESPONSE
        || usize::from(u16::from_be_bytes([packet[2], packet[3]])) != packet.len()
    {
        return Err(Ikev2EapError::InvalidPacket);
    }
    Ok(proletarius_ike::PayloadFrame {
        kind: EAP_PAYLOAD,
        critical: false,
        body: packet,
    })
}

fn parse_request(packet: &[u8]) -> Result<(u8, u8, &[u8]), Ikev2EapError> {
    if packet.len() < 5
        || packet.len() > MAX_EAP_PACKET
        || packet[0] != EAP_REQUEST
        || usize::from(u16::from_be_bytes([packet[2], packet[3]])) != packet.len()
    {
        return Err(Ikev2EapError::InvalidPacket);
    }
    Ok((packet[1], packet[4], &packet[5..]))
}

fn parse_identity_message(packet: &[u8], expected_code: u8) -> Result<(u8, &[u8]), Ikev2EapError> {
    if packet.len() < 5
        || packet.len() > MAX_EAP_PACKET
        || packet[0] != expected_code
        || usize::from(u16::from_be_bytes([packet[2], packet[3]])) != packet.len()
        || packet[4] != EAP_IDENTITY
        || packet.len() - 5 > MAX_IDENTITY
    {
        return Err(Ikev2EapError::InvalidPacket);
    }
    Ok((packet[1], &packet[5..]))
}

#[cfg(test)]
mod tests {
    use proletarius_ike::{
        PayloadFrame,
        eap_aka::{AkaCheckcodeMode, EapAkaError},
    };

    use super::{
        EapAkaExchangeError, EapAkaExchangeResult, Ikev2EapConversation, Ikev2EapError,
        SimAkaResult, eap_request_from_ike, eap_response_frame,
    };

    #[test]
    fn identity_response_preserves_request_id_and_exact_checkcode_transcript() {
        let mut conversation = Ikev2EapConversation::new(
            b"123456789012345@wlan.mnc001.mcc001.3gppnetwork.org",
            AkaCheckcodeMode::Checkcode,
        )
        .unwrap();
        let request = [1, 9, 0, 5, 1];
        let response = conversation.respond_identity(&request).unwrap();
        assert_eq!(response[0..5], [2, 9, 0, response.len() as u8, 1]);
        assert_eq!(
            &response[5..],
            b"123456789012345@wlan.mnc001.mcc001.3gppnetwork.org"
        );
        assert_eq!(
            conversation.identity_transcript.as_slice(),
            [&request[..], &response].concat()
        );
    }

    #[test]
    fn ike_frame_requires_exactly_one_well_formed_eap_request() {
        let eap = vec![1, 4, 0, 5, 1];
        let frames = [PayloadFrame {
            kind: 48,
            critical: false,
            body: eap.clone(),
        }];
        assert_eq!(eap_request_from_ike(&frames).unwrap(), eap);
        let duplicate = [frames[0].clone(), frames[0].clone()];
        assert!(matches!(
            eap_request_from_ike(&duplicate),
            Err(Ikev2EapError::InvalidPacket)
        ));
        let malformed = [PayloadFrame {
            kind: 48,
            critical: false,
            body: vec![1, 4, 0, 7, 1],
        }];
        assert!(eap_request_from_ike(&malformed).is_err());
    }

    #[test]
    fn challenge_parser_requires_card_inputs_before_sim_authentication() {
        let conversation =
            Ikev2EapConversation::new(b"nai@example", AkaCheckcodeMode::Minimal).unwrap();
        let challenge = [1, 7, 0, 8, 23, 1, 0, 0];
        assert!(matches!(
            conversation.challenge_inputs(&challenge),
            Err(Ikev2EapError::Aka(EapAkaError::MissingAttribute(1)))
        ));
        assert_eq!(eap_response_frame(vec![2, 7, 0, 5, 1]).unwrap().kind, 48);
        assert!(eap_response_frame(vec![2, 7, 0, 6, 1]).is_err());
    }

    #[test]
    fn challenge_exposes_sim_inputs_and_sync_failure_response_keeps_identifier() {
        let conversation =
            Ikev2EapConversation::new(b"nai@example", AkaCheckcodeMode::Minimal).unwrap();
        let mut challenge = vec![1, 33, 0, 68, 23, 1, 0, 0];
        challenge.extend_from_slice(&[1, 5, 0, 0]);
        challenge.extend_from_slice(&[0x11; 16]);
        challenge.extend_from_slice(&[2, 5, 0, 0]);
        challenge.extend_from_slice(&[0x22; 16]);
        challenge.extend_from_slice(&[11, 5, 0, 0]);
        challenge.extend_from_slice(&[0; 16]);
        let inputs = conversation.challenge_inputs(&challenge).unwrap();
        assert_eq!(inputs.identifier, 33);
        assert_eq!(inputs.rand, [0x11; 16]);
        assert_eq!(inputs.autn, [0x22; 16]);
        let response = conversation
            .answer_sync_failure(&challenge, &[0x55; 14])
            .unwrap();
        assert_eq!(response[0..2], [2, 33]);
        assert_eq!(response[4..8], [23, 4, 0, 0]);
        assert_eq!(eap_response_frame(response).unwrap().kind, 48);
    }

    fn valid_shape_challenge(identifier: u8) -> Vec<u8> {
        let mut packet = vec![1, identifier, 0, 68, 23, 1, 0, 0];
        packet.extend_from_slice(&[1, 5, 0, 0]);
        packet.extend_from_slice(&[0x11; 16]);
        packet.extend_from_slice(&[2, 5, 0, 0]);
        packet.extend_from_slice(&[0x22; 16]);
        packet.extend_from_slice(&[11, 5, 0, 0]);
        packet.extend_from_slice(&[0; 16]);
        packet
    }

    #[test]
    fn malformed_challenge_is_rejected_before_selected_sim_is_called() {
        use std::cell::Cell;

        let conversation =
            Ikev2EapConversation::new(b"nai@example", AkaCheckcodeMode::Minimal).unwrap();
        let called = Cell::new(false);
        let result = conversation.answer_challenge_with_sim(&[1, 3, 0, 8, 23, 1, 0, 0], |_, _| {
            called.set(true);
            Ok::<_, &'static str>(SimAkaResult::AuthenticationRejected)
        });
        assert!(matches!(
            result,
            Err(EapAkaExchangeError::Eap(Ikev2EapError::Aka(
                EapAkaError::MissingAttribute(1)
            )))
        ));
        assert!(!called.get());
    }

    #[test]
    fn sim_sync_failure_becomes_matching_eap_auts_without_retrying_sim() {
        use std::cell::Cell;

        let conversation =
            Ikev2EapConversation::new(b"nai@example", AkaCheckcodeMode::Minimal).unwrap();
        let challenge = valid_shape_challenge(77);
        let calls = Cell::new(0);
        let result = conversation
            .answer_challenge_with_sim(&challenge, |rand, autn| {
                calls.set(calls.get() + 1);
                assert_eq!(rand, &[0x11; 16]);
                assert_eq!(autn, &[0x22; 16]);
                Ok::<_, &'static str>(SimAkaResult::SynchronizationFailure { auts: [0x5a; 14] })
            })
            .unwrap();
        let EapAkaExchangeResult::SynchronizationFailure(packet) = result else {
            panic!("expected EAP synchronization-failure response");
        };
        assert_eq!(calls.get(), 1);
        assert_eq!(&packet[0..2], &[2, 77]);
        assert_eq!(&packet[4..8], &[23, 4, 0, 0]);
    }

    #[test]
    fn selected_sim_rejection_and_service_error_remain_distinct() {
        let conversation =
            Ikev2EapConversation::new(b"nai@example", AkaCheckcodeMode::Minimal).unwrap();
        let challenge = valid_shape_challenge(78);
        assert!(matches!(
            conversation.answer_challenge_with_sim(&challenge, |_, _| {
                Ok::<_, &'static str>(SimAkaResult::AuthenticationRejected)
            }),
            Err(EapAkaExchangeError::AuthenticationRejected)
        ));
        assert!(matches!(
            conversation.answer_challenge_with_sim(&challenge, |_, _| Err("reader busy")),
            Err(EapAkaExchangeError::SimService("reader busy"))
        ));
    }

    #[test]
    fn sim_aka_debug_output_redacts_all_secret_material() {
        let result = SimAkaResult::Success {
            res: b"private-res".to_vec(),
            ck: [0xa1; 16],
            ik: [0xb2; 16],
        };
        let debug = format!("{result:?}");
        assert!(debug.contains("success"));
        assert!(!debug.contains("private-res"));
        assert!(!debug.contains("161"));
        assert!(!debug.contains("178"));
    }

    #[test]
    fn restored_identity_transcript_is_bound_to_the_same_identity_and_identifier() {
        let identity = b"nai@example";
        let mut original =
            Ikev2EapConversation::new(identity, AkaCheckcodeMode::Checkcode).unwrap();
        let request = [1, 9, 0, 5, 1];
        let response = original.respond_identity(&request).unwrap();
        let transcript = [&request[..], &response].concat();
        let restored = Ikev2EapConversation::with_identity_transcript(
            identity,
            &transcript,
            AkaCheckcodeMode::Checkcode,
        )
        .unwrap();
        assert_eq!(restored.identity_transcript.as_slice(), transcript);
        assert!(
            Ikev2EapConversation::with_identity_transcript(
                b"other@example",
                &transcript,
                AkaCheckcodeMode::Checkcode,
            )
            .is_err()
        );

        let mut mismatched_id = transcript;
        mismatched_id[response.len() + 1] ^= 1;
        assert!(
            Ikev2EapConversation::with_identity_transcript(
                identity,
                &mismatched_id,
                AkaCheckcodeMode::Checkcode,
            )
            .is_err()
        );
    }
}
