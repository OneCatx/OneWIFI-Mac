//! Single-owner native tunnel startup. Carrier/config and SIM ownership are
//! supplied by the service layer; this module never reads a card or DNS itself.
use std::{future::Future, time::Duration};

use proletarius_ike::{PayloadFrame, eap_aka::EapAkaChallenge};
use tokio::sync::{mpsc, watch};
use zeroize::Zeroizing;

use crate::{
    ChildDataError, ChildDataPlane, DatagramTransportError, EapHandshakeError,
    EpdgDatagramTransport, IkeAuthError, IkeAuthSession, Ikev2EapConversation, SaInitError,
    SimAkaResult, TunnelConfiguration, TunnelRunStats, perform_sa_init_group14,
};

#[derive(Debug)]
pub enum TunnelStartupError<E> {
    Configuration(NativeConfigError),
    Cancelled,
    Deadline,
    SaInit(SaInitError),
    Transport(DatagramTransportError),
    Eap(EapHandshakeError<E>),
    Auth(IkeAuthError),
    DataPlane(ChildDataError),
}

#[derive(Debug)]
pub enum NativeConfigError {
    Proposals(proletarius_ike::ProposalConfigError),
    Auth(crate::AuthConfigError),
    Identity(crate::Ikev2EapError),
    EntropyUnavailable,
}

#[derive(Debug)]
pub enum NativeNetworkError<E> {
    Configuration(crate::netstack_runtime::NetworkConfigError),
    Tunnel(IkeAuthError),
    Network(crate::netstack_runtime::NetworkRunError<E>),
}

/// Effective settings, not defaults: carrier selection and user overrides
/// must be resolved before constructing this value. No Debug for identity.
pub struct NativeTunnelSettings<'a> {
    pub identity: &'a [u8],
    pub apn: &'a str,
    pub ike_proposals: &'a [String],
    pub esp_proposals: &'a [String],
    pub ip_stack: crate::RequestedIpStack,
    pub checkcode_mode: proletarius_ike::eap_aka::AkaCheckcodeMode,
    pub initial_contact: bool,
}

struct AuthFrames(Vec<PayloadFrame>);
impl Drop for AuthFrames {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        for frame in &mut self.0 {
            frame.body.zeroize();
        }
    }
}

/// Safe service-facing entry: OS entropy generates fresh SPIs/nonce for each
/// attempt, and the exact same configured identity feeds IKE IDi and EAP.
pub async fn start_configured_native_tunnel<F, Fut, E>(
    transport: EpdgDatagramTransport,
    settings: NativeTunnelSettings<'_>,
    authenticate: F,
    stop: watch::Receiver<bool>,
) -> Result<NativeTunnel, TunnelStartupError<E>>
where
    F: FnMut(EapAkaChallenge) -> Fut,
    Fut: Future<Output = Result<SimAkaResult, E>>,
{
    use proletarius_ike::{ProposalProtocol, build_native_proposals};
    let prepare = || -> Result<_, NativeConfigError> {
        let mut random = [0; 44];
        getrandom::fill(&mut random).map_err(|_| NativeConfigError::EntropyUnavailable)?;
        // Reject zero rather than biasing random SPI bits or retrying forever.
        let spi = u64::from_be_bytes(random[..8].try_into().unwrap());
        if spi == 0 || random[8..12] == [0; 4] {
            return Err(NativeConfigError::EntropyUnavailable);
        }
        let ike = build_native_proposals(settings.ike_proposals, ProposalProtocol::Ike, &[])
            .map_err(NativeConfigError::Proposals)?;
        let esp = build_native_proposals(
            settings.esp_proposals,
            ProposalProtocol::Esp,
            &random[8..12],
        )
        .map_err(NativeConfigError::Proposals)?;
        let auth = crate::build_initial_auth_frames(crate::InitialAuthParameters {
            identity: settings.identity,
            apn: settings.apn,
            child_proposals: &esp,
            ip_stack: settings.ip_stack,
            initial_contact: settings.initial_contact,
        })
        .map_err(NativeConfigError::Auth)?;
        let conversation = Ikev2EapConversation::new(settings.identity, settings.checkcode_mode)
            .map_err(NativeConfigError::Identity)?;
        Ok((
            spi,
            vec![
                PayloadFrame {
                    kind: 33,
                    critical: false,
                    body: ike,
                },
                PayloadFrame {
                    kind: 40,
                    critical: false,
                    body: random[12..].to_vec(),
                },
            ],
            auth,
            conversation,
        ))
    };
    if *stop.borrow() || stop.has_changed().is_err() {
        return Err(TunnelStartupError::Cancelled);
    }
    let (spi, sa, auth, conversation) = prepare().map_err(TunnelStartupError::Configuration)?;
    let auth = AuthFrames(auth);
    start_native_tunnel(
        transport,
        spi,
        &sa,
        &auth.0,
        conversation,
        authenticate,
        stop,
    )
    .await
}

/// No public constructor: an ESP plane cannot be paired with another SIM's
/// IKE keys by a caller. No Clone/Debug to duplicate counters or expose keys.
pub struct NativeTunnel {
    session: IkeAuthSession,
    plane: ChildDataPlane,
    transport: EpdgDatagramTransport,
}

impl NativeTunnel {
    /// One structured owner for the encrypted tunnel and userspace sockets.
    /// No detached tasks: either side finishing drops the other side, its
    /// keys, sockets and queues. Only the selected transport can emit packets.
    #[allow(clippy::too_many_arguments)]
    pub async fn run_network<F, E>(
        self,
        sockets: smoltcp::iface::SocketSet<'static>,
        stop: watch::Receiver<bool>,
        wake: watch::Receiver<u64>,
        probe_interval: Duration,
        retry_interval: Duration,
        attempts: u8,
        service: F,
    ) -> Result<(), NativeNetworkError<E>>
    where
        F: FnMut(
            &mut smoltcp::iface::Interface,
            &mut smoltcp::iface::SocketSet<'static>,
        ) -> Result<(), E>,
    {
        self.run_network_managed(
            sockets,
            stop,
            wake,
            probe_interval,
            retry_interval,
            attempts,
            crate::netstack_runtime::identity_runtime(service),
        )
        .await
    }

    /// Production-capable dynamic runtime entry. The supplied runtime and the
    /// authenticated IKE/ESP tunnel are cancelled together; dropping either
    /// side releases sockets, queued commands and installed IMS keys.
    #[allow(clippy::too_many_arguments)]
    pub async fn run_network_managed<R>(
        self,
        sockets: smoltcp::iface::SocketSet<'static>,
        stop: watch::Receiver<bool>,
        wake: watch::Receiver<u64>,
        probe_interval: Duration,
        retry_interval: Duration,
        attempts: u8,
        runtime: R,
    ) -> Result<(), NativeNetworkError<R::ApplicationError>>
    where
        R: crate::netstack_runtime::NetworkRuntime,
    {
        let network = crate::netstack_runtime::TunnelNetwork::new(self.configuration())
            .map_err(NativeNetworkError::Configuration)?;
        let (out_tx, out_rx) = mpsc::channel(crate::netstack_device::QUEUE_PACKETS);
        let (in_tx, in_rx) = mpsc::channel(crate::netstack_device::QUEUE_PACKETS);
        let tunnel = self.run(
            out_rx,
            in_tx,
            stop.clone(),
            probe_interval,
            retry_interval,
            attempts,
        );
        // External cancellation is owned by the IKE runtime so it can transmit
        // the protected DELETE before the socket stack drops its channels.
        // Once the tunnel returns, channel closure releases this network future.
        let (_network_stop, network_stop_rx) = watch::channel(false);
        let network = network.run_managed(sockets, in_rx, out_tx, network_stop_rx, wake, runtime);
        tokio::pin!(tunnel, network);
        tokio::select! {
            // Prefer the authenticated peer/DPD error over the resulting
            // network-channel closure when both futures finish together.
            biased;
            result = &mut tunnel => result.map(|_| ()).map_err(NativeNetworkError::Tunnel),
            result = &mut network => {
                if *stop.borrow() || stop.has_changed().is_err() {
                    // A normal stop may also close the runtime wake source.
                    // Do not let that race drop an in-flight protected DELETE.
                    tokio::time::timeout(Duration::from_secs(6), &mut tunnel)
                        .await
                        .map_err(|_| NativeNetworkError::Tunnel(IkeAuthError::Timeout))?
                        .map(|_| ())
                        .map_err(NativeNetworkError::Tunnel)
                } else {
                    result.map(|_| ()).map_err(NativeNetworkError::Network)
                }
            },
        }
    }

    /// Start the owned TCP/IP runtime with an initially empty socket set.
    /// Dynamic socket services such as IMS/TCP can then allocate sockets
    /// through their control handles without the daemon depending directly on
    /// smoltcp types.
    #[allow(clippy::too_many_arguments)]
    pub async fn run_network_with_runtime<R>(
        self,
        stop: watch::Receiver<bool>,
        wake: watch::Receiver<u64>,
        probe_interval: Duration,
        retry_interval: Duration,
        attempts: u8,
        runtime: R,
    ) -> Result<(), NativeNetworkError<R::ApplicationError>>
    where
        R: crate::netstack_runtime::NetworkRuntime,
    {
        self.run_network_managed(
            smoltcp::iface::SocketSet::new(vec![]),
            stop,
            wake,
            probe_interval,
            retry_interval,
            attempts,
            runtime,
        )
        .await
    }

    pub fn configuration(&self) -> &TunnelConfiguration {
        self.plane.configuration()
    }

    pub async fn run(
        self,
        outbound: mpsc::Receiver<Zeroizing<Vec<u8>>>,
        inbound: mpsc::Sender<Zeroizing<Vec<u8>>>,
        stop: watch::Receiver<bool>,
        probe_interval: Duration,
        retry_interval: Duration,
        attempts: u8,
    ) -> Result<TunnelRunStats, IkeAuthError> {
        self.session
            .run_tunnel(
                self.plane,
                self.transport,
                outbound,
                inbound,
                stop,
                probe_interval,
                retry_interval,
                attempts,
            )
            .await
    }
}

/// Uses the already selected direct/TUN/SOCKS transport for every phase. NAT-T
/// changes only its ePDG port; it never resolves a second address or bypasses
/// the selected proxy. Callers supply cryptographically generated SPI/nonce
/// and validated, supported proposal/identity frames from effective config.
///
/// The SIM provider must retain its device lock if a hardware call outlives
/// cancellation, as required by authenticate_eap. Success means an IKE/ESP
/// tunnel, not IMS registration or working SMS/calls.
pub(crate) async fn start_native_tunnel<F, Fut, E>(
    mut transport: EpdgDatagramTransport,
    initiator_spi: u64,
    sa_init_frames: &[PayloadFrame],
    initial_auth_frames: &[PayloadFrame],
    conversation: Ikev2EapConversation,
    authenticate: F,
    mut stop: watch::Receiver<bool>,
) -> Result<NativeTunnel, TunnelStartupError<E>>
where
    F: FnMut(EapAkaChallenge) -> Fut,
    Fut: Future<Output = Result<SimAkaResult, E>>,
{
    if *stop.borrow() || stop.has_changed().is_err() {
        return Err(TunnelStartupError::Cancelled);
    }
    let startup = async move {
        let schedule = perform_sa_init_group14(
            &transport,
            initiator_spi,
            sa_init_frames,
            3,
            Duration::from_secs(5),
        )
        .await
        .map_err(TunnelStartupError::SaInit)?;
        transport
            .switch_port(4500)
            .await
            .map_err(TunnelStartupError::Transport)?;
        let session = IkeAuthSession::from_sa_init(schedule).map_err(TunnelStartupError::Auth)?;
        let (session, eap) = session
            .authenticate_eap(&transport, initial_auth_frames, conversation, authenticate)
            .await
            .map_err(TunnelStartupError::Eap)?;
        let (session, child) = session
            .finish_eap_authentication(&transport, eap)
            .await
            .map_err(TunnelStartupError::Auth)?;
        let plane = child
            .into_data_plane()
            .map_err(TunnelStartupError::DataPlane)?;
        Ok(NativeTunnel {
            session,
            plane,
            transport,
        })
    };
    tokio::pin!(startup);
    let cancelled = async {
        loop {
            if stop.changed().await.is_err() || *stop.borrow() {
                break;
            }
        }
    };
    tokio::select! {
        biased;
        _ = cancelled => Err(TunnelStartupError::Cancelled),
        result = tokio::time::timeout(Duration::from_secs(180), &mut startup) => {
            result.map_err(|_| TunnelStartupError::Deadline)?
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ProxyMode;
    use proletarius_ike::eap_aka::AkaCheckcodeMode;
    use std::net::{IpAddr, Ipv4Addr};

    #[tokio::test]
    async fn startup_cancellation_before_and_during_sa_init_releases_transport() {
        for already_cancelled in [true, false] {
            let peer = tokio::net::UdpSocket::bind("127.0.0.1:0").await.unwrap();
            let transport = EpdgDatagramTransport::open(
                ProxyMode::Direct(IpAddr::V4(Ipv4Addr::LOCALHOST)),
                "unused.invalid",
                peer.local_addr().unwrap().port(),
                None,
            )
            .await
            .unwrap();
            let (stop_tx, stop_rx) = watch::channel(already_cancelled);
            let frames = vec![
                PayloadFrame {
                    kind: 33,
                    critical: false,
                    body: vec![
                        0, 0, 0, 36, 1, 1, 0, 3, 3, 0, 0, 12, 1, 0, 0, 20, 0x80, 14, 0, 128, 3, 0,
                        0, 8, 2, 0, 0, 5, 0, 0, 0, 8, 4, 0, 0, 14,
                    ],
                },
                PayloadFrame {
                    kind: 40,
                    critical: false,
                    body: vec![7; 32],
                },
            ];
            let remote = async {
                let mut buf = [0; 2048];
                if !already_cancelled {
                    let (n, _) = peer.recv_from(&mut buf).await.unwrap();
                    let (header, _) = proletarius_ike::IkeHeader::decode(&buf[..n]).unwrap();
                    assert_eq!(header.exchange, proletarius_ike::ExchangeType::IkeSaInit);
                    stop_tx.send(true).unwrap();
                }
                assert!(
                    tokio::time::timeout(Duration::from_millis(80), peer.recv_from(&mut buf))
                        .await
                        .is_err()
                );
            };
            let local = start_native_tunnel(
                transport,
                7,
                &frames,
                &[],
                Ikev2EapConversation::new(b"test@example.invalid", AkaCheckcodeMode::Minimal)
                    .unwrap(),
                |_| async {
                    panic!("cancelled startup must not access SIM");
                    #[allow(unreachable_code)]
                    Ok::<SimAkaResult, ()>(unreachable!())
                },
                stop_rx,
            );
            let (result, ()) = tokio::time::timeout(Duration::from_secs(3), async {
                tokio::join!(local, remote)
            })
            .await
            .unwrap();
            assert!(matches!(result, Err(TunnelStartupError::Cancelled)));
        }
    }
}
