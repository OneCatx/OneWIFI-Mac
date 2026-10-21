#![forbid(unsafe_code)]

use std::fmt;

pub mod amr;
pub mod pcma;
pub mod sdp;

pub const CALL_PROTOCOL_CONTRACT: &str = "ims-call-rtp-state-v1";
const RTP_HEADER_LENGTH: usize = 12;
const MAX_RTP_PACKET_LENGTH: usize = 65_535;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CallError {
    InvalidTransition,
    InvalidResponse,
    ResponseTransactionMismatch,
    Rejected(u16),
    RtpTooShort,
    RtpTooLarge,
    InvalidRtpVersion,
    InvalidRtpExtension,
    InvalidRtpPadding,
    EmptyRtpPayload,
    InvalidDtmfDigit,
}

impl fmt::Display for CallError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidTransition => formatter.write_str("invalid IMS call transition"),
            Self::InvalidResponse => formatter.write_str("invalid SIP INVITE response"),
            Self::ResponseTransactionMismatch => {
                formatter.write_str("SIP response does not match the INVITE transaction")
            }
            Self::Rejected(status) => write!(formatter, "IMS call rejected with SIP {status}"),
            Self::RtpTooShort => formatter.write_str("RTP packet is shorter than its header"),
            Self::RtpTooLarge => formatter.write_str("RTP packet exceeds the configured limit"),
            Self::InvalidRtpVersion => formatter.write_str("unsupported RTP version"),
            Self::InvalidRtpExtension => formatter.write_str("invalid RTP header extension"),
            Self::InvalidRtpPadding => formatter.write_str("invalid RTP padding"),
            Self::EmptyRtpPayload => formatter.write_str("RTP packet has no payload"),
            Self::InvalidDtmfDigit => formatter.write_str("DTMF digit must be 0-9, * or #"),
        }
    }
}

impl std::error::Error for CallError {}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CallPhase {
    Idle,
    Inviting,
    Early,
    Cancelling,
    Active,
    Terminating,
    Ended,
    Failed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum InviteAction {
    Wait,
    StartEarlyMedia,
    SendPrack { start_early_media: bool },
    AckAndStartMedia,
    AckAndBye,
    AckAndEnd,
    AckAndFail { status: u16 },
    AckOnly,
}

#[derive(Debug)]
pub struct OutgoingCallTracker {
    phase: CallPhase,
    invite_cseq: u32,
    cancel_requested: bool,
    last_rseq: Option<u32>,
    reliable_media_negotiated: bool,
    final_status: Option<u16>,
}

impl OutgoingCallTracker {
    pub fn begin(invite_cseq: u32) -> Result<Self, CallError> {
        if invite_cseq == 0 {
            return Err(CallError::InvalidTransition);
        }
        Ok(Self {
            phase: CallPhase::Inviting,
            invite_cseq,
            cancel_requested: false,
            last_rseq: None,
            reliable_media_negotiated: false,
            final_status: None,
        })
    }

    pub fn phase(&self) -> CallPhase {
        self.phase
    }

    pub fn cancel(&mut self) -> Result<(), CallError> {
        if !matches!(self.phase, CallPhase::Inviting | CallPhase::Early) {
            return Err(CallError::InvalidTransition);
        }
        self.cancel_requested = true;
        self.phase = CallPhase::Cancelling;
        Ok(())
    }

    /// `has_sdp` means a successfully validated offer/answer, not merely a body.
    /// The caller must scope this tracker to a single SIP dialog.
    pub fn accept_invite_response(
        &mut self,
        status: u16,
        cseq: u32,
        has_sdp: bool,
        reliable_rseq: Option<u32>,
    ) -> Result<InviteAction, CallError> {
        if cseq != self.invite_cseq {
            return Err(CallError::ResponseTransactionMismatch);
        }
        if !(100..=699).contains(&status) {
            return Err(CallError::InvalidResponse);
        }
        if self.final_status == Some(status) {
            return Ok(InviteAction::AckOnly);
        }
        if !matches!(
            self.phase,
            CallPhase::Inviting | CallPhase::Early | CallPhase::Cancelling
        ) {
            return Err(CallError::InvalidTransition);
        }
        match status {
            100 => Ok(InviteAction::Wait),
            101..=199 => {
                if let Some(rseq) = reliable_rseq {
                    if rseq == 0 || (self.last_rseq.is_none() && rseq > i32::MAX as u32) {
                        return Err(CallError::InvalidResponse);
                    }
                    // RFC 3262: duplicates and gaps are not processed; a later
                    // retransmission can supply the missing consecutive RSeq.
                    if self
                        .last_rseq
                        .is_some_and(|previous| previous.checked_add(1) != Some(rseq))
                    {
                        return Ok(InviteAction::Wait);
                    }
                    self.last_rseq = Some(rseq);
                    self.reliable_media_negotiated |= has_sdp;
                    if self.phase != CallPhase::Cancelling {
                        self.phase = CallPhase::Early;
                    }
                    return Ok(InviteAction::SendPrack {
                        start_early_media: has_sdp && !self.cancel_requested,
                    });
                }
                if self.phase != CallPhase::Cancelling {
                    self.phase = CallPhase::Early;
                }
                Ok(if has_sdp && !self.cancel_requested {
                    InviteAction::StartEarlyMedia
                } else {
                    InviteAction::Wait
                })
            }
            200..=299 => {
                self.final_status = Some(status);
                if self.cancel_requested {
                    self.phase = CallPhase::Ended;
                    Ok(InviteAction::AckAndBye)
                } else if has_sdp || self.reliable_media_negotiated {
                    self.phase = CallPhase::Active;
                    Ok(InviteAction::AckAndStartMedia)
                } else {
                    self.phase = CallPhase::Failed;
                    Ok(InviteAction::AckAndFail { status })
                }
            }
            status => {
                self.final_status = Some(status);
                if self.cancel_requested && status == 487 {
                    self.phase = CallPhase::Ended;
                    Ok(InviteAction::AckAndEnd)
                } else {
                    self.phase = CallPhase::Failed;
                    Ok(InviteAction::AckAndFail { status })
                }
            }
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RtpPacket<'a> {
    pub marker: bool,
    pub payload_type: u8,
    pub sequence: u16,
    pub timestamp: u32,
    pub ssrc: u32,
    pub payload: &'a [u8],
}

impl<'a> RtpPacket<'a> {
    pub fn parse(packet: &'a [u8]) -> Result<Self, CallError> {
        if packet.len() < RTP_HEADER_LENGTH {
            return Err(CallError::RtpTooShort);
        }
        if packet.len() > MAX_RTP_PACKET_LENGTH {
            return Err(CallError::RtpTooLarge);
        }
        if packet[0] >> 6 != 2 {
            return Err(CallError::InvalidRtpVersion);
        }
        let csrc_bytes = usize::from(packet[0] & 0x0f)
            .checked_mul(4)
            .ok_or(CallError::RtpTooLarge)?;
        let mut offset = RTP_HEADER_LENGTH
            .checked_add(csrc_bytes)
            .ok_or(CallError::RtpTooLarge)?;
        if offset > packet.len() {
            return Err(CallError::RtpTooShort);
        }
        if packet[0] & 0x10 != 0 {
            let extension = packet
                .get(offset..offset + 4)
                .ok_or(CallError::InvalidRtpExtension)?;
            let words = usize::from(u16::from_be_bytes([extension[2], extension[3]]));
            offset = offset
                .checked_add(4)
                .and_then(|value| value.checked_add(words.checked_mul(4)?))
                .ok_or(CallError::InvalidRtpExtension)?;
            if offset > packet.len() {
                return Err(CallError::InvalidRtpExtension);
            }
        }
        let mut end = packet.len();
        if packet[0] & 0x20 != 0 {
            let padding = usize::from(*packet.last().ok_or(CallError::InvalidRtpPadding)?);
            if padding == 0 || padding > end.saturating_sub(offset) {
                return Err(CallError::InvalidRtpPadding);
            }
            end -= padding;
        }
        if offset >= end {
            return Err(CallError::EmptyRtpPayload);
        }
        Ok(Self {
            marker: packet[1] & 0x80 != 0,
            payload_type: packet[1] & 0x7f,
            sequence: u16::from_be_bytes([packet[2], packet[3]]),
            timestamp: u32::from_be_bytes(packet[4..8].try_into().expect("fixed RTP header")),
            ssrc: u32::from_be_bytes(packet[8..12].try_into().expect("fixed RTP header")),
            payload: &packet[offset..end],
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SequenceDisposition {
    First,
    InOrder,
    Missing(u8),
    DuplicateOrLate,
    StreamRestart,
}

#[derive(Debug, Default)]
pub struct SequenceTracker {
    ssrc: Option<u32>,
    last: u16,
}

impl SequenceTracker {
    pub fn observe(&mut self, ssrc: u32, sequence: u16) -> SequenceDisposition {
        let Some(previous_ssrc) = self.ssrc else {
            self.ssrc = Some(ssrc);
            self.last = sequence;
            return SequenceDisposition::First;
        };
        if previous_ssrc != ssrc {
            self.ssrc = Some(ssrc);
            self.last = sequence;
            return SequenceDisposition::StreamRestart;
        }
        let delta = sequence.wrapping_sub(self.last) as i16;
        if delta <= 0 {
            return SequenceDisposition::DuplicateOrLate;
        }
        self.last = sequence;
        if delta == 1 {
            SequenceDisposition::InOrder
        } else {
            SequenceDisposition::Missing((delta - 1).min(3) as u8)
        }
    }
}

pub fn dtmf_event(digit: char) -> Result<u8, CallError> {
    match digit {
        '0'..='9' => Ok(digit as u8 - b'0'),
        '*' => Ok(10),
        '#' => Ok(11),
        _ => Err(CallError::InvalidDtmfDigit),
    }
}

pub fn dtmf_payload(event: u8, duration: u16, end: bool) -> [u8; 4] {
    let mut payload = [event, 10, 0, 0];
    if end {
        payload[1] |= 0x80;
    }
    payload[2..].copy_from_slice(&duration.to_be_bytes());
    payload
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn local_cancel_treats_487_as_successful_end() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        call.cancel().unwrap();
        assert_eq!(
            call.accept_invite_response(487, 7, false, None).unwrap(),
            InviteAction::AckAndEnd
        );
        assert_eq!(call.phase(), CallPhase::Ended);
        assert_eq!(
            call.accept_invite_response(487, 7, false, None).unwrap(),
            InviteAction::AckOnly
        );
    }

    #[test]
    fn raced_200_after_cancel_is_acked_then_ended_with_bye() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        call.cancel().unwrap();
        assert_eq!(
            call.accept_invite_response(200, 7, true, None).unwrap(),
            InviteAction::AckAndBye
        );
    }

    #[test]
    fn network_487_is_acked_then_reported_as_failure() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        assert_eq!(
            call.accept_invite_response(487, 7, false, None).unwrap(),
            InviteAction::AckAndFail { status: 487 }
        );
        assert_eq!(call.phase(), CallPhase::Failed);
    }

    #[test]
    fn reliable_early_media_is_pracked_once() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        assert_eq!(
            call.accept_invite_response(183, 7, true, Some(1)).unwrap(),
            InviteAction::SendPrack {
                start_early_media: true
            }
        );
        assert_eq!(
            call.accept_invite_response(183, 7, true, Some(1)).unwrap(),
            InviteAction::Wait
        );
        assert_eq!(
            call.accept_invite_response(200, 7, true, None).unwrap(),
            InviteAction::AckAndStartMedia
        );
        assert_eq!(
            call.accept_invite_response(200, 7, true, None).unwrap(),
            InviteAction::AckOnly
        );
    }

    #[test]
    fn reliable_answer_can_be_reused_by_bodyless_final_response() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        call.accept_invite_response(183, 7, true, Some(8)).unwrap();
        assert_eq!(
            call.accept_invite_response(200, 7, false, None).unwrap(),
            InviteAction::AckAndStartMedia
        );
        assert_eq!(call.phase(), CallPhase::Active);
    }

    #[test]
    fn rseq_gap_does_not_consume_answer_or_advance_sequence() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        call.accept_invite_response(181, 7, false, Some(8)).unwrap();
        assert_eq!(
            call.accept_invite_response(183, 7, true, Some(10)).unwrap(),
            InviteAction::Wait
        );
        assert_eq!(
            call.accept_invite_response(182, 7, false, Some(9)).unwrap(),
            InviteAction::SendPrack {
                start_early_media: false
            }
        );
        assert_eq!(
            call.accept_invite_response(200, 7, false, None).unwrap(),
            InviteAction::AckAndFail { status: 200 }
        );
    }

    #[test]
    fn cancel_still_pracks_but_does_not_start_media() {
        let mut call = OutgoingCallTracker::begin(7).unwrap();
        call.cancel().unwrap();
        assert_eq!(
            call.accept_invite_response(183, 7, true, Some(1)).unwrap(),
            InviteAction::SendPrack {
                start_early_media: false
            }
        );
        assert_eq!(call.phase(), CallPhase::Cancelling);
    }

    #[test]
    fn rtp_parser_strips_csrc_extension_and_padding() {
        let packet = [
            0xb1, 0x88, 0, 9, 0, 0, 0, 10, 0, 0, 0, 11, 1, 2, 3, 4, 0xbe, 0xde, 0, 1, 9, 8, 7, 6,
            0xaa, 0xbb, 0, 2,
        ];
        let parsed = RtpPacket::parse(&packet).unwrap();
        assert!(parsed.marker);
        assert_eq!(parsed.payload_type, 8);
        assert_eq!(parsed.sequence, 9);
        assert_eq!(parsed.payload, [0xaa, 0xbb]);
    }

    #[test]
    fn sequence_tracker_bounds_concealment_and_resets_ssrc() {
        let mut tracker = SequenceTracker::default();
        assert_eq!(tracker.observe(1, 10), SequenceDisposition::First);
        assert_eq!(tracker.observe(1, 12), SequenceDisposition::Missing(1));
        assert_eq!(tracker.observe(1, 12), SequenceDisposition::DuplicateOrLate);
        assert_eq!(tracker.observe(1, 30), SequenceDisposition::Missing(3));
        assert_eq!(tracker.observe(2, 1), SequenceDisposition::StreamRestart);
    }

    #[test]
    fn dtmf_matches_rfc4733_events_and_end_bit() {
        assert_eq!(dtmf_event('9').unwrap(), 9);
        assert_eq!(dtmf_event('*').unwrap(), 10);
        assert_eq!(dtmf_event('#').unwrap(), 11);
        assert_eq!(dtmf_payload(1, 800, true), [1, 0x8a, 0x03, 0x20]);
        assert_eq!(dtmf_event('A').unwrap_err(), CallError::InvalidDtmfDigit);
    }
}
