//! Session-owned ESP data plane. Does not publish IMS or tunnel readiness.
use crate::{
    ChildTrafficSelectors, DatagramTransportError, EpdgDatagramTransport, PendingChildSa,
    TrafficSelector, TunnelConfiguration,
};
use proletarius_ike::esp::{EspError, EspReceiver, EspSender};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use zeroize::Zeroizing;

/// IKE packets are deliberately still untrusted here: the IKE owner must
/// authenticate them before processing DPD, DELETE, or rekey.
pub enum TunnelEvent {
    Ip(Zeroizing<Vec<u8>>),
    UnauthenticatedIke(Vec<u8>),
}

#[derive(Debug, Default)]
pub struct TunnelRunStats {
    pub received_ip: u64,
    pub discarded_packets: u64,
    pub received_ike: u64,
}

/// The caller sends already encrypted IKE packets; plaintext control frames
/// never belong on this queue. Inner IP is encrypted by the data plane.
pub enum TunnelCommand {
    Ip(Zeroizing<Vec<u8>>),
    EncryptedIke(Vec<u8>),
    /// Send the authenticated IKE DELETE acknowledgement before releasing ESP.
    CloseAfterIke(Vec<u8>),
}

#[derive(Debug)]
pub enum ChildDataError {
    PeerDeleted,
    InvalidControlPacket,
    EventConsumerUnavailable,
    SendTimeout,
    UnsupportedAlgorithm,
    MissingConfiguration,
    PolicyRejected,
    Esp(EspError),
    Transport(DatagramTransportError),
}
impl std::fmt::Display for ChildDataError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::PeerDeleted => f.write_str("peer deleted the IKE SA; tunnel closed"),
            Self::InvalidControlPacket => {
                f.write_str("tunnel IKE command must contain an encrypted SK payload")
            }
            Self::EventConsumerUnavailable => {
                f.write_str("tunnel event consumer closed or backlogged; session must be rebuilt")
            }
            Self::SendTimeout => f.write_str("tunnel datagram send timed out"),
            Self::UnsupportedAlgorithm => {
                f.write_str("negotiated ESP algorithm is not implemented by the native data plane")
            }
            Self::MissingConfiguration => {
                f.write_str("ESP data plane requires an assigned internal address")
            }
            Self::PolicyRejected => {
                f.write_str("inner packet is malformed or outside negotiated traffic selectors")
            }
            Self::Esp(e) => e.fmt(f),
            Self::Transport(e) => e.fmt(f),
        }
    }
}
impl std::error::Error for ChildDataError {}

pub struct ChildDataPlane {
    sender: EspSender,
    receiver: EspReceiver,
    selectors: ChildTrafficSelectors,
    configuration: TunnelConfiguration,
}

impl PendingChildSa {
    /// Consumes the negotiated keys, so this value cannot accidentally install
    /// a second sender with its nonce counter reset.
    pub fn into_data_plane(mut self) -> Result<ChildDataPlane, ChildDataError> {
        let suite = (
            self.selection.encryption_transform,
            self.selection.integrity_transform,
        );
        let (sender, receiver) = match suite {
            (20, None)
                if self.selection.integrity_key_len == 0
                    && self.keys.initiator_integrity.is_empty()
                    && self.keys.responder_integrity.is_empty() =>
            {
                (
                    EspSender::new(
                        self.selection.remote_spi,
                        Zeroizing::new(std::mem::take(&mut self.keys.initiator_encryption)),
                    )
                    .map_err(ChildDataError::Esp)?,
                    EspReceiver::new(
                        self.selection.local_spi,
                        Zeroizing::new(std::mem::take(&mut self.keys.responder_encryption)),
                    )
                    .map_err(ChildDataError::Esp)?,
                )
            }
            (12, Some(integrity @ (12 | 14))) => (
                EspSender::new_cbc(
                    self.selection.remote_spi,
                    Zeroizing::new(std::mem::take(&mut self.keys.initiator_encryption)),
                    integrity,
                    Zeroizing::new(std::mem::take(&mut self.keys.initiator_integrity)),
                )
                .map_err(ChildDataError::Esp)?,
                EspReceiver::new_cbc(
                    self.selection.local_spi,
                    Zeroizing::new(std::mem::take(&mut self.keys.responder_encryption)),
                    integrity,
                    Zeroizing::new(std::mem::take(&mut self.keys.responder_integrity)),
                )
                .map_err(ChildDataError::Esp)?,
            ),
            _ => return Err(ChildDataError::UnsupportedAlgorithm),
        };
        let configuration = self
            .configuration
            .ok_or(ChildDataError::MissingConfiguration)?;
        if configuration.ipv4_addresses.is_empty() && configuration.ipv6_addresses.is_empty() {
            return Err(ChildDataError::MissingConfiguration);
        }
        Ok(ChildDataPlane {
            sender,
            receiver,
            selectors: self.traffic_selectors,
            configuration,
        })
    }

    /// Compatibility gate for callers that still promise a GCM-only proposal
    /// set. It deliberately refuses a negotiated CBC suite.
    pub fn into_gcm_data_plane(self) -> Result<ChildDataPlane, ChildDataError> {
        if self.selection.encryption_transform != 20 || self.selection.integrity_transform.is_some()
        {
            return Err(ChildDataError::UnsupportedAlgorithm);
        }
        self.into_data_plane()
    }
}

impl ChildDataPlane {
    /// Single socket receive owner. Cancellation drops keys and transport.
    /// NAT keepalive is not a liveness proof; IMS/DPD owners remain responsible
    /// for authenticated health checks. Queue saturation fails visibly rather
    /// than silently losing an IKE control packet or leaving a fake-online SA.
    pub async fn run(
        mut self,
        transport: EpdgDatagramTransport,
        mut commands: tokio::sync::mpsc::Receiver<TunnelCommand>,
        events: tokio::sync::mpsc::Sender<TunnelEvent>,
        mut shutdown: tokio::sync::oneshot::Receiver<Vec<u8>>,
        mut stop: tokio::sync::watch::Receiver<bool>,
    ) -> Result<TunnelRunStats, ChildDataError> {
        use tokio::time::{Duration, MissedTickBehavior, timeout};
        let mut stats = TunnelRunStats::default();
        let mut keepalive = tokio::time::interval(Duration::from_secs(20));
        keepalive.set_missed_tick_behavior(MissedTickBehavior::Skip);
        keepalive.tick().await;
        loop {
            if *stop.borrow() {
                return Ok(stats);
            }
            tokio::select! {
                biased;
                packet=&mut shutdown=>{
                    let packet=packet.map_err(|_|ChildDataError::EventConsumerUnavailable)?;
                    send_priority_shutdown(&transport,&packet).await?;
                    return Ok(stats);
                },
                changed=stop.changed()=>{if changed.is_err() || *stop.borrow() {return Ok(stats);}},
                _=events.closed()=>return Err(ChildDataError::EventConsumerUnavailable),
                command=commands.recv()=>{
                    let Some(command)=command else {return Ok(stats);};
                    let close_after_send = matches!(&command, TunnelCommand::CloseAfterIke(_));
                    let peer_deleted = matches!(&command, TunnelCommand::CloseAfterIke(_));
                    let operation=timeout(Duration::from_secs(5),async {
                        match command {
                            TunnelCommand::Ip(packet)=>self.send_ip(&transport,&packet).await,
                            TunnelCommand::EncryptedIke(packet)
                            | TunnelCommand::CloseAfterIke(packet)=>{
                                if !proletarius_ike::IkeHeader::decode(&packet).is_ok_and(|(header,body)|header.next_payload==46 && body.len()>=29) {
                                    return Err(ChildDataError::InvalidControlPacket);
                                }
                                transport.send_ike(&packet).await.map_err(ChildDataError::Transport)
                            },
                        }
                    });
                    tokio::pin!(operation);
                    tokio::select! {
                        biased;
                        packet=&mut shutdown=>{
                            let packet=packet.map_err(|_|ChildDataError::EventConsumerUnavailable)?;
                            send_priority_shutdown(&transport,&packet).await?;
                            return Ok(stats);
                        },
                        result=&mut operation=>{
                            result.map_err(|_|ChildDataError::SendTimeout)??;
                        },
                    }
                    if close_after_send {
                        return if peer_deleted {
                            Err(ChildDataError::PeerDeleted)
                        } else {
                            Ok(stats)
                        };
                    }
                },
                _=keepalive.tick()=>{
                    let operation=timeout(Duration::from_secs(5),transport.send_keepalive());
                    tokio::pin!(operation);
                    tokio::select! {
                        biased;
                        packet=&mut shutdown=>{
                            let packet=packet.map_err(|_|ChildDataError::EventConsumerUnavailable)?;
                            send_priority_shutdown(&transport,&packet).await?;
                            return Ok(stats);
                        },
                        result=&mut operation=>{
                            result.map_err(|_|ChildDataError::SendTimeout)?
                                .map_err(ChildDataError::Transport)?;
                        },
                    }
                },
                packet=transport.receive_packet()=>{
                    let event=match packet {
                        Ok(crate::EpdgPacket::Esp(packet))=>match self.open_esp(&packet) {
                            Ok(ip)=>{stats.received_ip=stats.received_ip.saturating_add(1);Some(TunnelEvent::Ip(ip))},
                            Err(ChildDataError::Esp(_)|ChildDataError::PolicyRejected)=>{stats.discarded_packets=stats.discarded_packets.saturating_add(1);None},
                            Err(error)=>return Err(error),
                        },
                        Ok(crate::EpdgPacket::Ike(packet))=>{stats.received_ike=stats.received_ike.saturating_add(1);Some(TunnelEvent::UnauthenticatedIke(packet))},
                        Ok(crate::EpdgPacket::Keepalive)=>None,
                        Err(DatagramTransportError::InvalidEsp|DatagramTransportError::InvalidIke(_))=>{stats.discarded_packets=stats.discarded_packets.saturating_add(1);None},
                        Err(error)=>return Err(ChildDataError::Transport(error)),
                    };
                    if let Some(event)=event {
                        let delivery=events.send(event);
                        tokio::pin!(delivery);
                        tokio::select! {
                            biased;
                            packet=&mut shutdown=>{
                                let packet=packet.map_err(|_|ChildDataError::EventConsumerUnavailable)?;
                                send_priority_shutdown(&transport,&packet).await?;
                                return Ok(stats);
                            },
                            delivered=timeout(Duration::from_millis(250),&mut delivery)=>{
                                delivered.map_err(|_|ChildDataError::EventConsumerUnavailable)?
                                    .map_err(|_|ChildDataError::EventConsumerUnavailable)?;
                            },
                        }
                    }
                },
            }
        }
    }
    pub fn configuration(&self) -> &TunnelConfiguration {
        &self.configuration
    }
    pub fn seal_ip(&mut self, packet: &[u8]) -> Result<Vec<u8>, ChildDataError> {
        self.check_policy(packet, true)?;
        self.sender.seal(packet).map_err(ChildDataError::Esp)
    }
    pub fn open_esp(&mut self, packet: &[u8]) -> Result<Zeroizing<Vec<u8>>, ChildDataError> {
        let plain = self.receiver.open(packet).map_err(ChildDataError::Esp)?;
        self.check_policy(&plain, false)?;
        Ok(plain)
    }
    /// Uses the existing selected ePDG channel, including SOCKS/TUN routing.
    /// No DNS resolution, fallback socket, or host route changes occur here.
    pub async fn send_ip(
        &mut self,
        transport: &EpdgDatagramTransport,
        packet: &[u8],
    ) -> Result<(), ChildDataError> {
        let esp = self.seal_ip(packet)?;
        transport
            .send_esp(&esp)
            .await
            .map_err(ChildDataError::Transport)
    }
    fn check_policy(&self, packet: &[u8], outbound: bool) -> Result<(), ChildDataError> {
        let invalid = || ChildDataError::PolicyRejected;
        let (source, destination, protocol, offset) = match packet.first().map(|b| b >> 4) {
            Some(4) if packet.len() >= 20 => {
                let offset = usize::from(packet[0] & 15) * 4;
                if offset < 20
                    || offset > packet.len()
                    || usize::from(u16::from_be_bytes([packet[2], packet[3]])) != packet.len()
                    || u16::from_be_bytes([packet[6], packet[7]]) & 0x3fff != 0
                {
                    return Err(invalid());
                }
                (
                    IpAddr::V4(Ipv4Addr::from(
                        <[u8; 4]>::try_from(&packet[12..16]).unwrap(),
                    )),
                    IpAddr::V4(Ipv4Addr::from(
                        <[u8; 4]>::try_from(&packet[16..20]).unwrap(),
                    )),
                    packet[9],
                    offset,
                )
            }
            Some(6) if packet.len() >= 40 => {
                if usize::from(u16::from_be_bytes([packet[4], packet[5]])) + 40 != packet.len() {
                    return Err(invalid());
                }
                // Extension headers and fragments require explicit walking /
                // reassembly before port policy can be applied safely.
                if matches!(packet[6], 0 | 43 | 44 | 50 | 51 | 60) {
                    return Err(invalid());
                }
                (
                    IpAddr::V6(Ipv6Addr::from(
                        <[u8; 16]>::try_from(&packet[8..24]).unwrap(),
                    )),
                    IpAddr::V6(Ipv6Addr::from(
                        <[u8; 16]>::try_from(&packet[24..40]).unwrap(),
                    )),
                    packet[6],
                    40,
                )
            }
            _ => return Err(invalid()),
        };
        let (source_port, destination_port) = match protocol {
            6 | 17 => {
                let ports = packet.get(offset..offset + 4).ok_or_else(invalid)?;
                (
                    Some(u16::from_be_bytes([ports[0], ports[1]])),
                    Some(u16::from_be_bytes([ports[2], ports[3]])),
                )
            }
            1 | 58 => {
                let icmp = packet.get(offset..offset + 2).ok_or_else(invalid)?;
                let port = Some(u16::from_be_bytes([icmp[0], icmp[1]]));
                (port, port)
            }
            _ => (None, None),
        };
        let local = if outbound { source } else { destination };
        let assigned = match local {
            IpAddr::V4(ip) => self.configuration.ipv4_addresses.contains(&ip),
            IpAddr::V6(ip) => self
                .configuration
                .ipv6_addresses
                .iter()
                .any(|(address, _)| *address == ip),
        };
        let (sources, destinations) = if outbound {
            (&self.selectors.initiator, &self.selectors.responder)
        } else {
            (&self.selectors.responder, &self.selectors.initiator)
        };
        if !assigned
            || !matches_selector(sources, source, protocol, source_port)
            || !matches_selector(destinations, destination, protocol, destination_port)
        {
            return Err(invalid());
        }
        Ok(())
    }
}

async fn send_priority_shutdown(
    transport: &EpdgDatagramTransport,
    packet: &[u8],
) -> Result<(), ChildDataError> {
    if !proletarius_ike::IkeHeader::decode(packet)
        .is_ok_and(|(header, body)| header.next_payload == 46 && body.len() >= 29)
    {
        return Err(ChildDataError::InvalidControlPacket);
    }
    tokio::time::timeout(
        std::time::Duration::from_secs(5),
        transport.send_ike(packet),
    )
    .await
    .map_err(|_| ChildDataError::SendTimeout)?
    .map_err(ChildDataError::Transport)
}

fn matches_selector(
    selectors: &[TrafficSelector],
    address: IpAddr,
    protocol: u8,
    port: Option<u16>,
) -> bool {
    selectors.iter().any(|s| {
        s.start_address.is_ipv4() == address.is_ipv4()
            && s.start_address <= address
            && s.end_address >= address
            && (s.protocol == 0 || s.protocol == protocol)
            && port.map_or(s.start_port == 0 && s.end_port == 65535, |port| {
                s.start_port <= port && s.end_port >= port
            })
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn broad_selectors() -> ChildTrafficSelectors {
        let selector = || {
            vec![TrafficSelector {
                protocol: 0,
                start_port: 0,
                end_port: 65535,
                start_address: "0.0.0.0".parse().unwrap(),
                end_address: "255.255.255.255".parse().unwrap(),
            }]
        };
        ChildTrafficSelectors {
            initiator: selector(),
            responder: selector(),
        }
    }

    fn ipv4_packet(source: [u8; 4], destination: [u8; 4]) -> Vec<u8> {
        let mut packet = vec![0; 20];
        packet[0] = 0x45;
        packet[3] = 20;
        packet[12..16].copy_from_slice(&source);
        packet[16..20].copy_from_slice(&destination);
        packet
    }

    fn runtime_plane() -> ChildDataPlane {
        ChildDataPlane {
            sender: EspSender::new(1, Zeroizing::new(vec![1; 20])).unwrap(),
            receiver: EspReceiver::new(2, Zeroizing::new(vec![2; 20])).unwrap(),
            selectors: broad_selectors(),
            configuration: TunnelConfiguration {
                ipv4_addresses: vec![Ipv4Addr::new(10, 0, 0, 2)],
                ..Default::default()
            },
        }
    }

    fn protected_control(message_id: u32, frames: &[proletarius_ike::PayloadFrame]) -> Vec<u8> {
        proletarius_ike::sk::seal_ike_sk_gcm(
            proletarius_ike::IkeHeader {
                initiator_spi: 1,
                responder_spi: 2,
                next_payload: 0,
                version: 0x20,
                exchange: proletarius_ike::ExchangeType::Informational,
                flags: proletarius_ike::IkeHeader::INITIATOR_FLAG,
                message_id,
            },
            20,
            &[3; 20],
            frames,
        )
        .unwrap()
    }

    #[tokio::test]
    async fn priority_shutdown_preempts_dpd_control_and_event_backlog() {
        use tokio::{
            net::UdpSocket,
            sync::{mpsc, oneshot, watch},
            time::{Duration, timeout},
        };
        timeout(Duration::from_secs(2), async {
            let peer = UdpSocket::bind("127.0.0.1:0").await.unwrap();
            let address = peer.local_addr().unwrap();
            let transport = EpdgDatagramTransport::open(
                crate::ProxyMode::Direct(address.ip()),
                "unused.invalid",
                address.port(),
                None,
            )
            .await
            .unwrap();
            let (commands, rx) = mpsc::channel(2);
            commands
                .send(TunnelCommand::EncryptedIke(protected_control(3, &[])))
                .await
                .unwrap();
            commands
                .send(TunnelCommand::EncryptedIke(protected_control(4, &[])))
                .await
                .unwrap();
            let (events, _incoming) = mpsc::channel(1);
            assert!(
                events
                    .try_send(TunnelEvent::UnauthenticatedIke(vec![0]))
                    .is_ok()
            );
            let (shutdown, shutdown_rx) = oneshot::channel();
            let (_stop, stop_rx) = watch::channel(false);
            let delete = protected_control(
                5,
                &[proletarius_ike::PayloadFrame {
                    kind: 42,
                    critical: false,
                    body: vec![1, 0, 0, 0],
                }],
            );
            shutdown.send(delete.clone()).unwrap();
            let runtime =
                tokio::spawn(runtime_plane().run(transport, rx, events, shutdown_rx, stop_rx));
            let mut wire = [0; 512];
            let (size, _) = peer.recv_from(&mut wire).await.unwrap();
            assert_eq!(&wire[..size], delete.as_slice());
            assert_eq!(runtime.await.unwrap().unwrap().received_ike, 0);
            assert!(
                timeout(Duration::from_millis(50), peer.recv_from(&mut wire))
                    .await
                    .is_err(),
                "queued DPD must not be emitted after priority shutdown"
            );
        })
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn priority_shutdown_escapes_event_backpressure() {
        use tokio::{
            net::UdpSocket,
            sync::{mpsc, oneshot, watch},
            time::{Duration, timeout},
        };
        timeout(Duration::from_secs(2), async {
            let peer = UdpSocket::bind("127.0.0.1:0").await.unwrap();
            let address = peer.local_addr().unwrap();
            let transport = EpdgDatagramTransport::open(
                crate::ProxyMode::Direct(address.ip()),
                "unused.invalid",
                address.port(),
                None,
            )
            .await
            .unwrap();
            let (commands, rx) = mpsc::channel(1);
            let (events, _incoming) = mpsc::channel(1);
            assert!(
                events
                    .try_send(TunnelEvent::UnauthenticatedIke(vec![0]))
                    .is_ok()
            );
            let (shutdown, shutdown_rx) = oneshot::channel();
            let (_stop, stop_rx) = watch::channel(false);
            let runtime =
                tokio::spawn(runtime_plane().run(transport, rx, events, shutdown_rx, stop_rx));
            let dpd = protected_control(3, &[]);
            commands
                .send(TunnelCommand::EncryptedIke(dpd.clone()))
                .await
                .unwrap();
            let mut wire = [0; 512];
            let (size, source) = peer.recv_from(&mut wire).await.unwrap();
            assert_eq!(&wire[..size], dpd.as_slice());
            peer.send_to(&dpd, source).await.unwrap();
            tokio::time::sleep(Duration::from_millis(20)).await;
            let delete = protected_control(
                4,
                &[proletarius_ike::PayloadFrame {
                    kind: 42,
                    critical: false,
                    body: vec![1, 0, 0, 0],
                }],
            );
            shutdown.send(delete.clone()).unwrap();
            let (size, _) = peer.recv_from(&mut wire).await.unwrap();
            assert_eq!(&wire[..size], delete.as_slice());
            assert_eq!(runtime.await.unwrap().unwrap().received_ike, 1);
        })
        .await
        .unwrap();
    }

    #[test]
    fn cbc_child_sa_installs_directional_keys_and_preserves_policy() {
        let initiator_encryption = vec![0x11; 16];
        let initiator_integrity = vec![0x22; 32];
        let responder_encryption = vec![0x33; 16];
        let responder_integrity = vec![0x44; 32];
        let child = PendingChildSa {
            configuration: Some(TunnelConfiguration {
                ipv4_addresses: vec![Ipv4Addr::new(10, 0, 0, 2)],
                ..Default::default()
            }),
            traffic_selectors: broad_selectors(),
            response: crate::IkeAuthResponse {
                header: proletarius_ike::IkeHeader {
                    initiator_spi: 1,
                    responder_spi: 2,
                    next_payload: 46,
                    version: 0x20,
                    exchange: proletarius_ike::ExchangeType::IkeAuth,
                    flags: proletarius_ike::IkeHeader::RESPONSE_FLAG,
                    message_id: 1,
                },
                frames: vec![],
            },
            selection: crate::InitialChildSaSelection {
                local_spi: 1,
                remote_spi: 2,
                encryption_transform: 12,
                integrity_transform: Some(12),
                encryption_key_len: 16,
                integrity_key_len: 32,
            },
            keys: proletarius_ike::ChildSaKeyMaterial {
                initiator_encryption: initiator_encryption.clone(),
                initiator_integrity: initiator_integrity.clone(),
                responder_encryption: responder_encryption.clone(),
                responder_integrity: responder_integrity.clone(),
            },
        };
        let mut plane = child.into_data_plane().unwrap();
        let outbound = ipv4_packet([10, 0, 0, 2], [10, 0, 0, 9]);
        let encrypted = plane.seal_ip(&outbound).unwrap();
        let mut network_receiver = EspReceiver::new_cbc(
            2,
            Zeroizing::new(initiator_encryption),
            12,
            Zeroizing::new(initiator_integrity),
        )
        .unwrap();
        assert_eq!(&*network_receiver.open(&encrypted).unwrap(), &outbound);

        let inbound = ipv4_packet([10, 0, 0, 9], [10, 0, 0, 2]);
        let mut network_sender = EspSender::new_cbc(
            1,
            Zeroizing::new(responder_encryption),
            12,
            Zeroizing::new(responder_integrity),
        )
        .unwrap();
        assert_eq!(
            &*plane
                .open_esp(&network_sender.seal(&inbound).unwrap())
                .unwrap(),
            &inbound
        );
    }

    #[tokio::test]
    async fn socks_runtime_carries_mixed_traffic_and_detects_control_loss() {
        use tokio::{
            io::{AsyncReadExt, AsyncWriteExt},
            net::{TcpListener, UdpSocket},
            sync::{mpsc, oneshot, watch},
            time::{Duration, timeout},
        };
        for close_control in [false, true] {
            timeout(Duration::from_secs(3), async {
                let tcp = TcpListener::bind("127.0.0.1:0").await.unwrap();
                let proxy = tcp.local_addr().unwrap();
                let udp = UdpSocket::bind("127.0.0.1:0").await.unwrap();
                let relay = udp.local_addr().unwrap();
                let (release, done) = oneshot::channel::<()>();
                let mut outgoing = vec![0; 28];
                outgoing[0] = 0x45;
                outgoing[3] = 28;
                outgoing[9] = 17;
                outgoing[12..16].copy_from_slice(&[10, 0, 0, 2]);
                outgoing[16..20].copy_from_slice(&[10, 0, 0, 9]);
                outgoing[20..24].copy_from_slice(&[0x13, 0xc4, 0x13, 0xc4]);
                outgoing[25] = 8;
                let expected = outgoing.clone();
                let server = tokio::spawn(async move {
                    let (mut control, _) = tcp.accept().await.unwrap();
                    let mut greeting = [0; 3];
                    control.read_exact(&mut greeting).await.unwrap();
                    assert_eq!(greeting, [5, 1, 0]);
                    control.write_all(&[5, 0]).await.unwrap();
                    let mut associate = [0; 10];
                    control.read_exact(&mut associate).await.unwrap();
                    assert_eq!(&associate[..3], &[5, 3, 0]);
                    let mut reply = vec![5, 0, 0, 1, 127, 0, 0, 1];
                    reply.extend_from_slice(&relay.port().to_be_bytes());
                    control.write_all(&reply).await.unwrap();
                    let mut buffer = [0; 2048];
                    let (size, source) = udp.recv_from(&mut buffer).await.unwrap();
                    let packet = crate::decode_udp_datagram(&buffer[..size]).unwrap();
                    assert_eq!(
                        packet.target,
                        crate::SocksTarget::Domain("epdg.invalid".into())
                    );
                    assert_eq!(packet.port, 4500);
                    let mut receiver = EspReceiver::new(1, Zeroizing::new(vec![1; 20])).unwrap();
                    assert_eq!(&*receiver.open(&packet.payload).unwrap(), &expected);
                    let mut incoming = expected;
                    incoming[15] = 9;
                    incoming[19] = 2;
                    let mut sender = EspSender::new(2, Zeroizing::new(vec![2; 20])).unwrap();
                    let esp = sender.seal(&incoming).unwrap();
                    let mut bad = esp.clone();
                    *bad.last_mut().unwrap() ^= 1;
                    let header = proletarius_ike::IkeHeader {
                        initiator_spi: 1,
                        responder_spi: 2,
                        next_payload: 0,
                        version: 0x20,
                        exchange: proletarius_ike::ExchangeType::Informational,
                        flags: 0,
                        message_id: 0,
                    };
                    let ike =
                        proletarius_ike::sk::seal_ike_sk_gcm(header, 20, &[3; 20], &[]).unwrap();
                    for data in [
                        vec![0xff],
                        bad,
                        esp.clone(),
                        esp,
                        proletarius_ike::encode_natt(&ike),
                    ] {
                        udp.send_to(
                            &crate::encode_udp_domain("epdg.invalid", 4500, &data).unwrap(),
                            source,
                        )
                        .await
                        .unwrap();
                    }
                    done.await.unwrap();
                });
                let transport = EpdgDatagramTransport::open(
                    crate::ProxyMode::Socks5UdpDomain,
                    "epdg.invalid",
                    4500,
                    Some(proxy),
                )
                .await
                .unwrap();
                let selector = || {
                    vec![TrafficSelector {
                        protocol: 0,
                        start_port: 0,
                        end_port: 65535,
                        start_address: "0.0.0.0".parse().unwrap(),
                        end_address: "255.255.255.255".parse().unwrap(),
                    }]
                };
                let plane = ChildDataPlane {
                    sender: EspSender::new(1, Zeroizing::new(vec![1; 20])).unwrap(),
                    receiver: EspReceiver::new(2, Zeroizing::new(vec![2; 20])).unwrap(),
                    selectors: ChildTrafficSelectors {
                        initiator: selector(),
                        responder: selector(),
                    },
                    configuration: TunnelConfiguration {
                        ipv4_addresses: vec![Ipv4Addr::new(10, 0, 0, 2)],
                        ..Default::default()
                    },
                };
                let (commands, rx) = mpsc::channel(2);
                let (events, mut incoming) = mpsc::channel(4);
                let (_shutdown, shutdown_rx) = oneshot::channel();
                let (stop, watch) = watch::channel(false);
                let runtime = tokio::spawn(plane.run(transport, rx, events, shutdown_rx, watch));
                commands
                    .send(TunnelCommand::Ip(Zeroizing::new(outgoing)))
                    .await
                    .unwrap();
                assert!(matches!(incoming.recv().await.unwrap(), TunnelEvent::Ip(_)));
                assert!(matches!(
                    incoming.recv().await.unwrap(),
                    TunnelEvent::UnauthenticatedIke(_)
                ));
                if close_control {
                    release.send(()).unwrap();
                    server.await.unwrap();
                    assert!(matches!(
                        runtime.await.unwrap(),
                        Err(ChildDataError::Transport(DatagramTransportError::Relay(
                            crate::RelayError::ControlClosed
                        )))
                    ));
                } else {
                    stop.send(true).unwrap();
                    let stats = runtime.await.unwrap().unwrap();
                    assert_eq!(
                        (
                            stats.received_ip,
                            stats.received_ike,
                            stats.discarded_packets
                        ),
                        (1, 1, 2)
                    );
                    release.send(()).unwrap();
                    server.await.unwrap();
                }
            })
            .await
            .unwrap();
        }
    }
    #[tokio::test]
    async fn runtime_preserves_ike_events_discards_malformed_and_cancels() {
        use tokio::{
            net::UdpSocket,
            sync::{mpsc, oneshot, watch},
            time::{Duration, timeout},
        };
        for backpressure in [false, true] {
            timeout(Duration::from_secs(3), async {
                let peer = UdpSocket::bind("127.0.0.1:0").await.unwrap();
                let address = peer.local_addr().unwrap();
                let transport = EpdgDatagramTransport::open(
                    crate::ProxyMode::Direct(address.ip()),
                    "unused.invalid",
                    address.port(),
                    None,
                )
                .await
                .unwrap();
                let plane = ChildDataPlane {
                    sender: EspSender::new(1, Zeroizing::new(vec![1; 20])).unwrap(),
                    receiver: EspReceiver::new(2, Zeroizing::new(vec![2; 20])).unwrap(),
                    selectors: ChildTrafficSelectors {
                        initiator: vec![],
                        responder: vec![],
                    },
                    configuration: TunnelConfiguration::default(),
                };
                let (commands, rx) = mpsc::channel(1);
                let (events, mut incoming) = mpsc::channel(1);
                let (_shutdown, shutdown_rx) = oneshot::channel();
                let (stop, watch) = watch::channel(false);
                let task = tokio::spawn(plane.run(transport, rx, events, shutdown_rx, watch));
                let header = proletarius_ike::IkeHeader {
                    initiator_spi: 1,
                    responder_spi: 2,
                    next_payload: 0,
                    version: 0x20,
                    exchange: proletarius_ike::ExchangeType::Informational,
                    flags: proletarius_ike::IkeHeader::RESPONSE_FLAG,
                    message_id: 3,
                };
                let ike = proletarius_ike::sk::seal_ike_sk_gcm(header, 20, &[3; 20], &[]).unwrap();
                commands
                    .send(TunnelCommand::EncryptedIke(ike.clone()))
                    .await
                    .unwrap();
                let mut buffer = [0; 128];
                let (size, source) = peer.recv_from(&mut buffer).await.unwrap();
                assert_eq!(&buffer[..size], ike.as_slice());
                peer.send_to(&[1], source).await.unwrap();
                peer.send_to(&ike, source).await.unwrap();
                if backpressure {
                    peer.send_to(&ike, source).await.unwrap();
                    assert!(matches!(
                        task.await.unwrap(),
                        Err(ChildDataError::EventConsumerUnavailable)
                    ));
                } else {
                    match incoming.recv().await.unwrap() {
                        TunnelEvent::UnauthenticatedIke(packet) => assert_eq!(packet, ike),
                        _ => panic!("control packet misclassified"),
                    }
                    stop.send(true).unwrap();
                    let stats = task.await.unwrap().unwrap();
                    assert_eq!(stats.received_ike, 1);
                    assert_eq!(stats.discarded_packets, 1);
                }
            })
            .await
            .unwrap();
        }
    }
    #[test]
    fn policy_matches_protocol_port_address_and_family() {
        let allowed: IpAddr = "10.0.0.9".parse().unwrap();
        let selectors = vec![TrafficSelector {
            protocol: 17,
            start_port: 5060,
            end_port: 5060,
            start_address: allowed,
            end_address: allowed,
        }];
        assert!(matches_selector(&selectors, allowed, 17, Some(5060)));
        assert!(!matches_selector(&selectors, allowed, 6, Some(5060)));
        assert!(!matches_selector(&selectors, allowed, 17, Some(5061)));
        assert!(!matches_selector(&selectors, allowed, 17, None));
        assert!(!matches_selector(
            &selectors,
            "10.0.0.10".parse().unwrap(),
            17,
            Some(5060)
        ));
        assert!(!matches_selector(
            &selectors,
            "::a00:9".parse().unwrap(),
            17,
            Some(5060)
        ));
    }
}
