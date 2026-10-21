//! Encrypted IKE_AUTH EAP rounds. Final IKE AUTH/CHILD_SA remains a separate gate.
use std::future::Future;
use std::time::Duration;

use proletarius_ike::{PayloadFrame, eap_aka::EapAkaChallenge};
use zeroize::Zeroizing;

use crate::{
    EapAkaExchangeError, EapAkaExchangeResult, EpdgDatagramTransport, IkeAuthError,
    IkeAuthResponse, IkeAuthSession, Ikev2EapConversation, Ikev2EapError, SimAkaResult,
    eap_request_from_ike, eap_response_frame,
};

#[derive(Debug)]
pub enum EapHandshakeError<E> {
    Transport(IkeAuthError),
    Protocol(Ikev2EapError),
    Authentication(EapAkaExchangeError<E>),
    Rejected,
    UnexpectedSuccess,
    RoundLimit,
    Deadline,
}

/// EAP success alone is not proof of an authenticated IKE SA. The caller must
/// still verify responder AUTH and negotiate CHILD_SA before declaring ready.
pub struct EapHandshakeResult {
    pub msk: Zeroizing<Vec<u8>>,
    pub k_aut: Zeroizing<Vec<u8>>,
    pub last_response: IkeAuthResponse,
}

impl IkeAuthSession {
    /// Consumes the session so cancellation/error cannot leave a reusable
    /// half-authenticated session. The SIM provider must be asynchronous and
    /// preserve device ownership if its underlying hardware call outlives it.
    pub async fn authenticate_eap<F, Fut, E>(
        mut self,
        transport: &EpdgDatagramTransport,
        initial_frames: &[PayloadFrame],
        mut conversation: Ikev2EapConversation,
        mut authenticate: F,
    ) -> Result<(Self, EapHandshakeResult), EapHandshakeError<E>>
    where
        F: FnMut(EapAkaChallenge) -> Fut,
        Fut: Future<Output = Result<SimAkaResult, E>>,
    {
        let result = tokio::time::timeout(Duration::from_secs(120), async {
            let mut outgoing = initial_frames.to_vec();
            let mut keys = None;
            let mut last_identifier = None;
            let mut sync_rounds = 0;
            for _ in 0..16 {
                let reply = self
                    .exchange(transport, &outgoing, 3, Duration::from_secs(5))
                    .await
                    .map_err(EapHandshakeError::Transport)?;
                let mut payloads = reply.frames.iter().filter(|frame| frame.kind == 48);
                let packet = &payloads
                    .next()
                    .ok_or(EapHandshakeError::Protocol(Ikev2EapError::InvalidPacket))?
                    .body;
                if payloads.next().is_some()
                    || packet.len() < 4
                    || usize::from(u16::from_be_bytes([packet[2], packet[3]])) != packet.len()
                {
                    return Err(EapHandshakeError::Protocol(Ikev2EapError::InvalidPacket));
                }
                if packet[0] == 3 {
                    if packet.len() != 4 || last_identifier != Some(packet[1]) {
                        return Err(EapHandshakeError::UnexpectedSuccess);
                    }
                    let (msk, k_aut) = keys.take().ok_or(EapHandshakeError::UnexpectedSuccess)?;
                    return Ok(EapHandshakeResult {
                        msk,
                        k_aut,
                        last_response: reply,
                    });
                }
                if packet[0] == 4 {
                    return Err(EapHandshakeError::Rejected);
                }
                let packet =
                    eap_request_from_ike(&reply.frames).map_err(EapHandshakeError::Protocol)?;
                last_identifier = Some(packet[1]);
                let response = if packet[4] == 1 {
                    // A repeated identity round invalidates prior derived keys.
                    keys = None;
                    conversation
                        .respond_identity(packet)
                        .map_err(EapHandshakeError::Protocol)?
                } else {
                    keys = None;
                    let challenge = conversation
                        .challenge_inputs(packet)
                        .map_err(EapHandshakeError::Protocol)?;
                    let sim_result = authenticate(challenge).await.map_err(|error| {
                        EapHandshakeError::Authentication(EapAkaExchangeError::SimService(error))
                    })?;
                    let answer = conversation
                        .answer_challenge_with_sim(packet, |_, _| Ok::<_, E>(sim_result))
                        .map_err(EapHandshakeError::Authentication)?;
                    match answer {
                        EapAkaExchangeResult::Challenge(mut answer) => {
                            keys = Some((
                                std::mem::take(&mut answer.msk),
                                std::mem::take(&mut answer.k_aut),
                            ));
                            std::mem::take(&mut answer.packet)
                        }
                        EapAkaExchangeResult::SynchronizationFailure(packet) => {
                            sync_rounds += 1;
                            if sync_rounds > 2 {
                                return Err(EapHandshakeError::RoundLimit);
                            }
                            packet.to_vec()
                        }
                    }
                };
                outgoing = vec![eap_response_frame(response).map_err(EapHandshakeError::Protocol)?];
            }
            Err(EapHandshakeError::RoundLimit)
        })
        .await
        .map_err(|_| EapHandshakeError::Deadline)??;
        Ok((self, result))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        ProxyMode,
        sa_init::{SaInitKeySchedule, SaInitResponse},
    };
    use proletarius_ike::{
        ExchangeType, IkeHeader, IkeSaKeyMaterial, SessionPhase,
        eap_aka::AkaCheckcodeMode,
        sk::{open_ike_sk_gcm, seal_ike_sk_gcm},
    };
    use std::net::{IpAddr, Ipv4Addr};
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    #[tokio::test]
    async fn encrypted_identity_challenge_success_and_premature_success() {
        for premature in [false, true] {
            tokio::time::timeout(Duration::from_secs(3), async {
                let peer = tokio::net::UdpSocket::bind((Ipv4Addr::LOCALHOST, 0)).await.unwrap();
                let address = peer.local_addr().unwrap();
                let header = IkeHeader { initiator_spi: 11, responder_spi: 22, next_payload: 0,
                    version: 0x20, exchange: ExchangeType::IkeSaInit, flags: IkeHeader::RESPONSE_FLAG, message_id: 0 };
                let session = IkeAuthSession::from_sa_init(SaInitKeySchedule {
                    initiator_nonce: vec![6;32], prf_id:5,
                    initiator_spi: 11, encryption_transform: 20,
                    integrity_transform: None,
                    response: SaInitResponse { request_packet:vec![3;28], raw_packet: header.encode(&[]).unwrap(), responder_spi: 22,
                        selected_sa: vec![], selected_proposal: None, dh_group: 14, peer_public_key: vec![], responder_nonce: vec![7;32] },
                    keys: IkeSaKeyMaterial { sk_d: vec![1;32], sk_ai: vec![], sk_ar: vec![],
                        sk_ei: vec![0x55;20], sk_er: vec![0x66;20], sk_pi: vec![2;32], sk_pr: vec![3;32] },
                }).unwrap();
                let transport = EpdgDatagramTransport::open(ProxyMode::Direct(IpAddr::V4(Ipv4Addr::LOCALHOST)), "unused.invalid", address.port(), None).await.unwrap();
                let server = tokio::spawn(async move {
                    let golden = "014d004417010000010500001111111111111111111111111111111102050000222222222222222222222222222222220b05000019a5cd42f0c8250e9e34edb24815c777";
                    let challenge: Vec<u8> = (0..golden.len()).step_by(2).map(|i| u8::from_str_radix(&golden[i..i+2],16).unwrap()).collect();
                    let packets = if premature { vec![vec![1,9,0,5,1], vec![3,9,0,4]] }
                        else { vec![vec![1,9,0,5,1], challenge, vec![3,77,0,4]] };
                    for (round, packet) in packets.into_iter().enumerate() {
                        let mut buffer = [0;4096];
                        let (size, source) = peer.recv_from(&mut buffer).await.unwrap();
                        let (mut request, frames) = open_ike_sk_gcm(&buffer[..size],20,&[0x55;20]).unwrap();
                        assert_eq!(request.message_id, round as u32 + 1);
                        if round == 1 { assert_eq!(&frames[0].body[5..], b"nai@example"); }
                        if round == 2 { assert_eq!(&frames[0].body[..8], &[2,77,0,40,23,1,0,0]); }
                        request.flags = IkeHeader::RESPONSE_FLAG;
                        let response = seal_ike_sk_gcm(request,20,&[0x66;20], &[PayloadFrame {kind:48, critical:false, body:packet}]).unwrap();
                        peer.send_to(&response, source).await.unwrap();
                    }
                });
                let calls = Arc::new(AtomicUsize::new(0));
                let count = calls.clone();
                let result = session.authenticate_eap(&transport, &[PayloadFrame {kind:35, critical:false, body:[&[3,0,0,0][..], b"nai@example"].concat()}],
                    Ikev2EapConversation::new(b"nai@example", AkaCheckcodeMode::Minimal).unwrap(),
                    move |challenge| { count.fetch_add(1,Ordering::SeqCst); async move {
                        assert_eq!(challenge.rand,[0x11;16]);
                        Ok::<_, ()>(SimAkaResult::Success {res:vec![0x11,0x22,0x33,0x44,0x55,0x66,0x77,0x88],ck:[0x31;16],ik:[0x42;16]})
                    }}).await;
                server.await.unwrap();
                if premature {
                    assert!(matches!(result,Err(EapHandshakeError::UnexpectedSuccess)));
                    assert_eq!(calls.load(Ordering::SeqCst),0);
                } else {
                    let (session, result) = result.unwrap();
                    assert_eq!(result.msk.len(),64);
                    assert_eq!(result.k_aut.len(),16);
                    let auth = session.build_final_eap_auth(&result.msk).unwrap();
                    assert_eq!(auth.kind,39);
                    assert_eq!(&auth.body[..4], &[2,0,0,0]);
                    assert_eq!(auth.body.len(),36);
                    assert!(session.build_final_eap_auth(&[0;63]).is_err());
                    assert_eq!(session.phase(),SessionPhase::Authenticating);
                    assert_eq!(calls.load(Ordering::SeqCst),1);
                }
            }).await.unwrap();
        }
    }
}
