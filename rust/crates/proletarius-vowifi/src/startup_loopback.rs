//! Full native startup over a local SOCKS relay, with no real SIM or DNS.
use crate::*;
use proletarius_ike::{
    ExchangeType, IkeHeader, Modp2048KeyPair, PayloadFrame, ProposalProtocol,
    build_native_proposals, compute_eap_auth, decode_payload_chain, derive_ike_sa_key_material,
    derive_initial_child_sa_keys,
    eap_aka::{AkaCheckcodeMode, answer_eap_aka_challenge},
    encode_payload_chain,
    esp::{EspReceiver, EspSender},
    sk::{SkPayloadError, open_ike_sk_cbc, open_ike_sk_gcm, seal_ike_sk_cbc, seal_ike_sk_gcm},
};
use smoltcp::{
    iface::{Config, Interface, SocketSet},
    socket::udp,
    time::Instant,
    wire::{HardwareAddress, IpCidr},
};
use std::{net::SocketAddr, time::Duration};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, UdpSocket},
    sync::watch,
};
use zeroize::Zeroizing;

#[derive(Clone, Copy, Debug)]
enum NativeSuite {
    GcmBaseline,
    CtExcel,
    Giffgaff,
    Hutchison,
}

#[derive(Clone)]
struct NativeClientConfig {
    identity: Vec<u8>,
    apn: String,
    proposals: carrier::NativeAlgorithmProposals,
    ip_stack: RequestedIpStack,
    checkcode_mode: AkaCheckcodeMode,
}

impl NativeSuite {
    fn ike_proposal(self) -> &'static str {
        match self {
            Self::GcmBaseline => "aes128gcm16-prfsha256-modp2048",
            Self::CtExcel | Self::Hutchison => "aes128-sha256-prfsha256-modp2048",
            Self::Giffgaff => "aes256-sha512-prfsha512-modp2048",
        }
    }

    fn esp_proposal(self) -> &'static str {
        match self {
            Self::GcmBaseline => "aes128gcm16",
            Self::CtExcel | Self::Hutchison => "aes128-sha256",
            Self::Giffgaff => "aes256-sha512",
        }
    }

    fn prf_id(self) -> u16 {
        match self {
            Self::GcmBaseline | Self::CtExcel | Self::Hutchison => 5,
            Self::Giffgaff => 7,
        }
    }

    fn encryption_transform(self) -> u16 {
        match self {
            Self::GcmBaseline => 20,
            Self::CtExcel | Self::Giffgaff | Self::Hutchison => 12,
        }
    }

    fn integrity_transform(self) -> Option<u16> {
        match self {
            Self::GcmBaseline => None,
            Self::CtExcel | Self::Hutchison => Some(12),
            Self::Giffgaff => Some(14),
        }
    }

    fn encryption_key_len(self) -> usize {
        match self {
            Self::GcmBaseline => 20,
            Self::CtExcel | Self::Hutchison => 16,
            Self::Giffgaff => 32,
        }
    }

    fn integrity_key_len(self) -> usize {
        match self {
            Self::GcmBaseline => 0,
            Self::CtExcel | Self::Hutchison => 32,
            Self::Giffgaff => 64,
        }
    }

    fn client_config(self) -> NativeClientConfig {
        let identity = match self {
            Self::GcmBaseline => {
                return NativeClientConfig {
                    identity: b"nai@example".to_vec(),
                    apn: "ims".into(),
                    proposals: carrier::NativeAlgorithmProposals {
                        ike: vec![self.ike_proposal().into()],
                        esp: vec![self.esp_proposal().into()],
                    },
                    ip_stack: RequestedIpStack::Ipv4,
                    checkcode_mode: AkaCheckcodeMode::Minimal,
                };
            }
            Self::CtExcel => carrier::SimIdentity {
                mcc: "234".into(),
                mnc: "33".into(),
                imsi: "234330123456789".into(),
                ..Default::default()
            },
            Self::Giffgaff => carrier::SimIdentity {
                mcc: "234".into(),
                mnc: "10".into(),
                imsi: "234100123456789".into(),
                gid1: "508fffff00".into(),
                ..Default::default()
            },
            Self::Hutchison => carrier::SimIdentity {
                mcc: "234".into(),
                mnc: "20".into(),
                imsi: "234200123456789".into(),
                ..Default::default()
            },
        };
        let profile = carrier::Registry::embedded()
            .unwrap()
            .resolve(&identity)
            .unwrap()
            .unwrap();
        let config = profile
            .native_tunnel_config(
                &identity,
                &carrier::NativeTunnelOverrides {
                    epdg: Some("epdg.invalid".into()),
                    ip_stack: Some("ipv4".into()),
                    ..Default::default()
                },
            )
            .unwrap();
        assert_eq!(config.epdg, "epdg.invalid");
        assert_eq!(config.proposals.ike, [self.ike_proposal()]);
        assert_eq!(config.proposals.esp, [self.esp_proposal()]);
        assert!(!config.identity().is_empty());
        NativeClientConfig {
            // The fixed challenge below has an independently pinned AT_MAC
            // for this synthetic identity. Carrier identity expansion is
            // validated separately in carrier tests.
            identity: b"nai@example".to_vec(),
            apn: config.apn,
            proposals: config.proposals,
            ip_stack: config.ip_stack,
            checkcode_mode: config.aka_challenge_mode,
        }
    }
}

fn open_ike_sk(
    suite: NativeSuite,
    packet: &[u8],
    encryption_key: &[u8],
    integrity_key: &[u8],
) -> Result<(IkeHeader, Vec<PayloadFrame>), SkPayloadError> {
    match suite.integrity_transform() {
        None => open_ike_sk_gcm(packet, suite.encryption_transform(), encryption_key),
        Some(integrity) => open_ike_sk_cbc(
            packet,
            suite.encryption_transform(),
            integrity,
            encryption_key,
            integrity_key,
        ),
    }
}

fn seal_ike_sk(
    suite: NativeSuite,
    header: IkeHeader,
    encryption_key: &[u8],
    integrity_key: &[u8],
    frames: &[PayloadFrame],
) -> Result<Vec<u8>, SkPayloadError> {
    match suite.integrity_transform() {
        None => seal_ike_sk_gcm(header, suite.encryption_transform(), encryption_key, frames),
        Some(integrity) => seal_ike_sk_cbc(
            header,
            suite.encryption_transform(),
            integrity,
            encryption_key,
            integrity_key,
            frames,
        ),
    }
}

fn esp_receiver(
    suite: NativeSuite,
    spi: u32,
    encryption_key: Vec<u8>,
    integrity_key: Vec<u8>,
) -> EspReceiver {
    match suite.integrity_transform() {
        None => {
            assert!(integrity_key.is_empty());
            EspReceiver::new(spi, Zeroizing::new(encryption_key)).unwrap()
        }
        Some(integrity) => EspReceiver::new_cbc(
            spi,
            Zeroizing::new(encryption_key),
            integrity,
            Zeroizing::new(integrity_key),
        )
        .unwrap(),
    }
}

fn esp_sender(
    suite: NativeSuite,
    spi: u32,
    encryption_key: Vec<u8>,
    integrity_key: Vec<u8>,
) -> EspSender {
    match suite.integrity_transform() {
        None => {
            assert!(integrity_key.is_empty());
            EspSender::new(spi, Zeroizing::new(encryption_key)).unwrap()
        }
        Some(integrity) => EspSender::new_cbc(
            spi,
            Zeroizing::new(encryption_key),
            integrity,
            Zeroizing::new(integrity_key),
        )
        .unwrap(),
    }
}

fn sip_socket() -> udp::Socket<'static> {
    let mut socket = udp::Socket::new(
        udp::PacketBuffer::new(vec![udp::PacketMetadata::EMPTY; 4], vec![0; 4096]),
        udp::PacketBuffer::new(vec![udp::PacketMetadata::EMPTY; 4], vec![0; 4096]),
    );
    socket.bind(5060).unwrap();
    socket
}

fn frame(kind: u8, body: Vec<u8>) -> PayloadFrame {
    PayloadFrame {
        kind,
        critical: false,
        body,
    }
}
fn ike_offer(suite: NativeSuite) -> Vec<u8> {
    build_native_proposals(&[suite.ike_proposal().into()], ProposalProtocol::Ike, &[]).unwrap()
}
fn esp_offer(suite: NativeSuite, spi: u32) -> PayloadFrame {
    let body = build_native_proposals(
        &[suite.esp_proposal().into()],
        ProposalProtocol::Esp,
        &spi.to_be_bytes(),
    )
    .unwrap();
    frame(33, body)
}
fn ts(kind: u8) -> PayloadFrame {
    frame(
        kind,
        vec![
            1, 0, 0, 0, 7, 0, 0, 16, 0, 0, 255, 255, 0, 0, 0, 0, 255, 255, 255, 255,
        ],
    )
}
async fn recv(udp: &UdpSocket, port: u16) -> (Vec<u8>, SocketAddr) {
    let mut buf = [0; 4096];
    let (n, source) = udp.recv_from(&mut buf).await.unwrap();
    let packet = decode_udp_datagram(&buf[..n]).unwrap();
    assert_eq!(packet.target, SocksTarget::Domain("epdg.invalid".into()));
    assert_eq!(packet.port, port);
    (packet.payload, source)
}
async fn send(udp: &UdpSocket, source: SocketAddr, port: u16, packet: &[u8]) {
    udp.send_to(
        &encode_udp_domain("epdg.invalid", port, packet).unwrap(),
        source,
    )
    .await
    .unwrap();
}

#[tokio::test]
async fn native_startup_gcm_socks_natt_eap_auth_esp_dpd_and_delete() {
    native_network_case(NativeSuite::GcmBaseline, false).await;
    native_network_case(NativeSuite::GcmBaseline, true).await;
}

#[tokio::test]
async fn native_startup_ctexcel_cbc_socks_natt_eap_auth_esp_dpd_and_delete() {
    native_network_case(NativeSuite::CtExcel, false).await;
    native_network_case(NativeSuite::CtExcel, true).await;
}

#[tokio::test]
async fn native_startup_giffgaff_cbc_socks_natt_eap_auth_esp_dpd_and_delete() {
    native_network_case(NativeSuite::Giffgaff, false).await;
    native_network_case(NativeSuite::Giffgaff, true).await;
}

#[tokio::test]
async fn native_startup_hutchison_default_ike_socks_natt_eap_auth_esp_dpd_and_delete() {
    native_network_case(NativeSuite::Hutchison, false).await;
    native_network_case(NativeSuite::Hutchison, true).await;
}

async fn native_network_case(suite: NativeSuite, application_failure: bool) {
    tokio::time::timeout(Duration::from_secs(5), async {
        let client_config = suite.client_config();
        let server_identity = client_config.identity.clone();
        let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let proxy = tcp.local_addr().unwrap();
        let udp = UdpSocket::bind("127.0.0.1:0").await.unwrap();
        let server = tokio::spawn(async move {
            let (mut control, _) = tcp.accept().await.unwrap();
            let mut greeting = [0; 3];
            control.read_exact(&mut greeting).await.unwrap();
            assert_eq!(greeting, [5, 1, 0]);
            control.write_all(&[5, 0]).await.unwrap();
            let mut request = [0; 10];
            control.read_exact(&mut request).await.unwrap();
            assert_eq!(&request[..3], &[5, 3, 0]);
            let mut reply = vec![5, 0, 0, 1, 127, 0, 0, 1];
            reply.extend(udp.local_addr().unwrap().port().to_be_bytes());
            control.write_all(&reply).await.unwrap();

            let (raw_init, source) = recv(&udp, 500).await;
            let (mut header, body) = IkeHeader::decode(&raw_init).unwrap();
            let initiator_spi = header.initiator_spi;
            let initial = decode_payload_chain(header.next_payload, body).unwrap();
            let expected_ike_offer = ike_offer(suite);
            assert_eq!(
                initial.iter().find(|frame| frame.kind == 33).unwrap().body,
                expected_ike_offer
            );
            let ni = &initial.iter().find(|frame| frame.kind == 40).unwrap().body;
            let dh = Modp2048KeyPair::generate().unwrap();
            let shared = dh
                .shared_secret(
                    &initial
                        .iter()
                        .find(|frame| frame.kind == 34)
                        .unwrap()
                        .body[4..],
                )
                .unwrap();
            let nr = vec![8; 32];
            let mut ke = vec![0, 14, 0, 0];
            ke.extend(dh.public_key());
            header.responder_spi = 22;
            header.flags = IkeHeader::RESPONSE_FLAG;
            let (kind, body) = encode_payload_chain(&[
                frame(33, ike_offer(suite)),
                frame(34, ke),
                frame(40, nr.clone()),
            ])
            .unwrap();
            header.next_payload = kind;
            let init_response = header.encode(&body).unwrap();
            let keys = derive_ike_sa_key_material(
                suite.prf_id(),
                ni,
                &nr,
                &shared,
                header.initiator_spi,
                22,
                suite.encryption_key_len(),
                suite.integrity_key_len(),
            )
            .unwrap();
            send(&udp, source, 500, &init_response).await;

            let golden = "014d004417010000010500001111111111111111111111111111111102050000222222222222222222222222222222220b05000019a5cd42f0c8250e9e34edb24815c777";
            let challenge: Vec<u8> = (0..golden.len())
                .step_by(2)
                .map(|i| u8::from_str_radix(&golden[i..i + 2], 16).unwrap())
                .collect();
            let res = [0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88];
            let aka = answer_eap_aka_challenge(
                &challenge,
                &server_identity,
                &res,
                &[0x31; 16],
                &[0x42; 16],
            )
            .unwrap();
            let idi = [&[3, 0, 0, 0][..], server_identity.as_slice()].concat();
            let idr = [&[2, 0, 0, 0][..], b"epdg.invalid"].concat();
            let mut local_spi = 0;
            for (round, eap) in [vec![1, 9, 0, 5, 1], challenge, vec![3, 77, 0, 4]]
                .into_iter()
                .enumerate()
            {
                let (wire, from) = recv(&udp, 4500).await;
                assert_eq!(from, source);
                assert_eq!(&wire[..4], &[0; 4]);
                let (mut auth_header, frames) =
                    open_ike_sk(suite, &wire[4..], &keys.sk_ei, &keys.sk_ai).unwrap();
                assert_eq!(auth_header.message_id, round as u32 + 1);
                if round == 0 {
                    assert_eq!(frames[0].body, idi);
                    let child_offer = &frames
                        .iter()
                        .find(|frame| frame.kind == 33)
                        .unwrap()
                        .body;
                    let offered = proletarius_ike::decode_sa_proposals(child_offer).unwrap();
                    local_spi = u32::from_be_bytes(offered[0].spi.as_slice().try_into().unwrap());
                    assert_ne!(local_spi, 0);
                    assert_eq!(*child_offer, esp_offer(suite, local_spi).body);
                }
                if round == 1 {
                    assert_eq!(&frames[0].body[5..], server_identity.as_slice());
                }
                if round == 2 {
                    assert_eq!(frames[0].body, aka.packet);
                }
                auth_header.flags = IkeHeader::RESPONSE_FLAG;
                let reply = seal_ike_sk(
                    suite,
                    auth_header,
                    &keys.sk_er,
                    &keys.sk_ar,
                    &[frame(36, idr.clone()), frame(48, eap)],
                )
                .unwrap();
                send(
                    &udp,
                    source,
                    4500,
                    &proletarius_ike::encode_natt(&reply),
                )
                .await;
            }

            let (wire, _) = recv(&udp, 4500).await;
            let (mut auth_header, frames) =
                open_ike_sk(suite, &wire[4..], &keys.sk_ei, &keys.sk_ai).unwrap();
            assert_eq!(auth_header.message_id, 4);
            let expected = compute_eap_auth(
                suite.prf_id(),
                &aka.msk,
                &raw_init,
                &nr,
                &keys.sk_pi,
                &idi,
            )
            .unwrap();
            assert_eq!(&frames[0].body[4..], &*expected);
            let auth = compute_eap_auth(
                suite.prf_id(),
                &aka.msk,
                &init_response,
                ni,
                &keys.sk_pr,
                &idr,
            )
            .unwrap();
            let mut auth_body = vec![2, 0, 0, 0];
            auth_body.extend_from_slice(&auth);
            auth_header.flags = IkeHeader::RESPONSE_FLAG;
            let reply = seal_ike_sk(
                suite,
                auth_header,
                &keys.sk_er,
                &keys.sk_ar,
                &[
                    frame(39, auth_body),
                    esp_offer(suite, 2),
                    ts(44),
                    ts(45),
                    frame(47, vec![2, 0, 0, 0, 0, 1, 0, 4, 10, 0, 0, 2]),
                ],
            )
            .unwrap();
            send(
                &udp,
                source,
                4500,
                &proletarius_ike::encode_natt(&reply),
            )
            .await;

            let mut child = derive_initial_child_sa_keys(
                suite.prf_id(),
                &keys.sk_d,
                ni,
                &nr,
                suite.encryption_key_len(),
                suite.integrity_key_len(),
            )
            .unwrap();
            let mut rx = esp_receiver(
                suite,
                2,
                std::mem::take(&mut child.initiator_encryption),
                std::mem::take(&mut child.initiator_integrity),
            );
            let mut tx = esp_sender(
                suite,
                local_spi,
                std::mem::take(&mut child.responder_encryption),
                std::mem::take(&mut child.responder_integrity),
            );
            if application_failure {
                let mut byte = [0];
                assert_eq!(
                    tokio::time::timeout(Duration::from_secs(1), control.read(&mut byte))
                        .await
                        .unwrap()
                        .unwrap(),
                    0
                );
                assert!(
                    tokio::time::timeout(Duration::from_millis(80), recv(&udp, 4500))
                        .await
                        .is_err()
                );
                return;
            }

            let mut device = netstack_device::TunnelIpDevice::default();
            let mut interface =
                Interface::new(Config::new(HardwareAddress::Ip), &mut device, Instant::ZERO);
            interface.update_ip_addrs(|addresses| {
                addresses
                    .push("10.0.0.9/24".parse::<IpCidr>().unwrap())
                    .unwrap()
            });
            let mut sockets = SocketSet::new(vec![]);
            let socket = sockets.add(sip_socket());
            let (esp, _) = recv(&udp, 4500).await;
            let ip = rx.open(&esp).unwrap();
            assert_eq!(&ip[12..16], &[10, 0, 0, 2]);
            device.push_inbound(ip).unwrap();
            interface.poll(Instant::ZERO, &mut device, &mut sockets);
            let peer = sockets.get_mut::<udp::Socket>(socket);
            let (data, meta) = peer.recv().unwrap();
            assert_eq!(
                data,
                b"REGISTER sip:ims.example SIP/2.0\r\nContent-Length: 0\r\n\r\n"
            );
            peer.send_slice(
                b"SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n",
                meta.endpoint,
            )
            .unwrap();
            interface.poll(Instant::from_millis(1), &mut device, &mut sockets);
            let response = device.pop_outbound().unwrap();
            send(&udp, source, 4500, &tx.seal(&response).unwrap()).await;

            // This second packet proves the client decrypted the responder
            // direction before the server closes the authenticated session.
            let (esp, _) = recv(&udp, 4500).await;
            device.push_inbound(rx.open(&esp).unwrap()).unwrap();
            interface.poll(Instant::from_millis(2), &mut device, &mut sockets);
            assert_eq!(
                sockets.get_mut::<udp::Socket>(socket).recv().unwrap().0,
                b"received"
            );

            // Exercise the runtime DPD path, including authentication failure
            // isolation, before the peer-initiated DELETE closes the SA.
            let (wire, _) = recv(&udp, 4500).await;
            assert_eq!(&wire[..4], &[0; 4]);
            let (mut dpd_header, frames) =
                open_ike_sk(suite, &wire[4..], &keys.sk_ei, &keys.sk_ai).unwrap();
            assert_eq!(dpd_header.exchange, ExchangeType::Informational);
            assert_eq!(dpd_header.flags, IkeHeader::INITIATOR_FLAG);
            assert_eq!(dpd_header.message_id, 5);
            assert!(frames.is_empty());
            dpd_header.flags = IkeHeader::RESPONSE_FLAG;
            let dpd_response =
                seal_ike_sk(suite, dpd_header, &keys.sk_er, &keys.sk_ar, &[]).unwrap();
            let mut corrupt_dpd = dpd_response.clone();
            *corrupt_dpd.last_mut().unwrap() ^= 1;
            send(
                &udp,
                source,
                4500,
                &proletarius_ike::encode_natt(&corrupt_dpd),
            )
            .await;
            send(
                &udp,
                source,
                4500,
                &proletarius_ike::encode_natt(&dpd_response),
            )
            .await;

            let delete = seal_ike_sk(
                suite,
                IkeHeader {
                    initiator_spi,
                    responder_spi: 22,
                    next_payload: 46,
                    version: 0x20,
                    exchange: ExchangeType::Informational,
                    flags: 0,
                    message_id: 0,
                },
                &keys.sk_er,
                &keys.sk_ar,
                &[frame(42, vec![1, 0, 0, 0])],
            )
            .unwrap();
            let mut corrupt_delete = delete.clone();
            *corrupt_delete.last_mut().unwrap() ^= 1;
            send(
                &udp,
                source,
                4500,
                &proletarius_ike::encode_natt(&corrupt_delete),
            )
            .await;
            send(
                &udp,
                source,
                4500,
                &proletarius_ike::encode_natt(&delete),
            )
            .await;
            let (ack, _) = recv(&udp, 4500).await;
            let (ack_header, frames) =
                open_ike_sk(suite, &ack[4..], &keys.sk_ei, &keys.sk_ai).unwrap();
            assert_eq!(ack_header.exchange, ExchangeType::Informational);
            assert_eq!(
                ack_header.flags,
                IkeHeader::INITIATOR_FLAG | IkeHeader::RESPONSE_FLAG
            );
            assert_eq!(ack_header.message_id, 0);
            assert!(frames.is_empty());
        });

        let transport = EpdgDatagramTransport::open(
            ProxyMode::Socks5UdpDomain,
            "epdg.invalid",
            500,
            Some(proxy),
        )
        .await
        .unwrap();
        let (_stop, stop) = watch::channel(false);
        let tunnel = start_configured_native_tunnel(
            transport,
            NativeTunnelSettings {
                identity: &client_config.identity,
                apn: &client_config.apn,
                ike_proposals: &client_config.proposals.ike,
                esp_proposals: &client_config.proposals.esp,
                ip_stack: client_config.ip_stack,
                checkcode_mode: client_config.checkcode_mode,
                initial_contact: false,
            },
            |challenge| async move {
                assert_eq!(challenge.rand, [0x11; 16]);
                Ok::<_, ()>(SimAkaResult::Success {
                    res: vec![0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88],
                    ck: [0x31; 16],
                    ik: [0x42; 16],
                })
            },
            stop.clone(),
        )
        .await
        .unwrap();
        assert_eq!(
            tunnel.configuration().ipv4_addresses[0].to_string(),
            "10.0.0.2"
        );
        let mut sockets = SocketSet::new(vec![]);
        let socket = sockets.add(sip_socket());
        let (_wake, wake) = watch::channel(0);
        let mut sent = false;
        let mut received = false;
        let result = tunnel
            .run_network(
                sockets,
                stop,
                wake,
                Duration::from_millis(150),
                Duration::from_millis(80),
                2,
                |_, sockets| {
                    if application_failure {
                        return Err(());
                    }
                    let socket = sockets.get_mut::<udp::Socket>(socket);
                    if !sent {
                        socket
                            .send_slice(
                                b"REGISTER sip:ims.example SIP/2.0\r\nContent-Length: 0\r\n\r\n",
                                (
                                    "10.0.0.9"
                                        .parse::<smoltcp::wire::IpAddress>()
                                        .unwrap(),
                                    5060,
                                ),
                            )
                            .unwrap();
                        sent = true;
                    }
                    if socket.can_recv() {
                        let (data, meta) = socket.recv().unwrap();
                        assert_eq!(
                            data,
                            b"SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n"
                        );
                        received = true;
                        socket.send_slice(b"received", meta.endpoint).unwrap();
                    }
                    Ok::<_, ()>(())
                },
            )
            .await;
        if application_failure {
            assert!(matches!(
                result,
                Err(NativeNetworkError::Network(
                    netstack_runtime::NetworkRunError::Application(())
                ))
            ));
            assert!(!sent && !received);
        } else {
            assert!(matches!(
                result,
                Err(NativeNetworkError::Tunnel(IkeAuthError::DataPlane(
                    ChildDataError::PeerDeleted
                )))
            ));
            assert!(sent && received);
        }
        server.await.unwrap();
    })
    .await
    .unwrap();
}
