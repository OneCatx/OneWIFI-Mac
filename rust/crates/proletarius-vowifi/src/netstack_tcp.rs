//! AsyncRead/AsyncWrite stream backed solely by a session's smoltcp socket.
use crate::netstack_runtime::{NetworkRuntime, PacketDirection, PacketTransformOutcome};
use smoltcp::{
    iface::{Interface, SocketHandle, SocketSet},
    socket::tcp,
    wire::IpEndpoint,
};
use std::{
    convert::Infallible,
    io,
    net::SocketAddr,
    pin::Pin,
    sync::Arc,
    task::{Context, Poll, Wake, Waker},
    time::Duration,
};
use tokio::{
    io::{AsyncRead, AsyncWrite, DuplexStream, ReadBuf},
    sync::{mpsc, oneshot, watch},
};
use zeroize::Zeroizing;

const BUFFER: usize = 16 * 1024;
const DYNAMIC_SOCKET_CAPACITY: usize = 16;
const COMMAND_CAPACITY: usize = DYNAMIC_SOCKET_CAPACITY;
const COMMANDS_PER_SERVICE: usize = DYNAMIC_SOCKET_CAPACITY;

struct NetworkWake(watch::Sender<u64>);
impl Wake for NetworkWake {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }
    fn wake_by_ref(self: &Arc<Self>) {
        self.0.send_modify(|n| *n = n.wrapping_add(1));
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct SocketIdentity {
    generation: u64,
    handle: SocketHandle,
}

enum TcpCommand {
    Dial {
        peer: SocketAddr,
        local_port: u16,
        reply: oneshot::Sender<io::Result<PendingTcpStream>>,
    },
    Listen {
        local_port: u16,
        expected_peer: Option<SocketAddr>,
        reply: oneshot::Sender<io::Result<PendingTcpListener>>,
    },
}

/// Bounded command endpoint for the single task which owns the userspace TCP
/// stack. Command completion means that a socket has been created (and, for a
/// listener, `listen` has succeeded); the TCP handshake is a separate await.
#[derive(Clone)]
pub struct TcpControl {
    commands: mpsc::Sender<TcpCommand>,
    wake: watch::Sender<u64>,
}

impl TcpControl {
    pub async fn dial(&self, peer: SocketAddr, local_port: u16) -> io::Result<PendingTcpStream> {
        validate_dial(peer, local_port)?;
        let (reply, result) = oneshot::channel();
        self.commands
            .send(TcpCommand::Dial {
                peer,
                local_port,
                reply,
            })
            .await
            .map_err(|_| runtime_closed())?;
        self.notify();
        result.await.map_err(|_| runtime_closed())?
    }

    pub async fn listen(
        &self,
        local_port: u16,
        expected_peer: Option<SocketAddr>,
    ) -> io::Result<PendingTcpListener> {
        validate_listener(local_port, expected_peer)?;
        let (reply, result) = oneshot::channel();
        self.commands
            .send(TcpCommand::Listen {
                local_port,
                expected_peer,
                reply,
            })
            .await
            .map_err(|_| runtime_closed())?;
        self.notify();
        result.await.map_err(|_| runtime_closed())?
    }

    fn notify(&self) {
        self.wake
            .send_modify(|revision| *revision = revision.wrapping_add(1));
    }
}

fn runtime_closed() -> io::Error {
    io::Error::new(io::ErrorKind::BrokenPipe, "native TCP runtime is closed")
}

fn validate_dial(peer: SocketAddr, local_port: u16) -> io::Result<()> {
    if peer.port() == 0 || local_port == 0 || peer.ip().is_unspecified() || peer.ip().is_multicast()
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid native TCP dial endpoint",
        ));
    }
    Ok(())
}

fn validate_listener(local_port: u16, expected_peer: Option<SocketAddr>) -> io::Result<()> {
    if local_port == 0
        || expected_peer.is_some_and(|peer| {
            peer.port() == 0 || peer.ip().is_unspecified() || peer.ip().is_multicast()
        })
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "invalid native TCP listener endpoint",
        ));
    }
    Ok(())
}

struct SocketLease {
    close: Option<oneshot::Sender<()>>,
    closed: Option<oneshot::Receiver<()>>,
    wake: watch::Sender<u64>,
}

impl Drop for SocketLease {
    fn drop(&mut self) {
        if let Some(close) = self.close.take() {
            let _ = close.send(());
            self.wake
                .send_modify(|revision| *revision = revision.wrapping_add(1));
        }
    }
}

impl SocketLease {
    fn begin_close(&mut self) -> PendingTcpClose {
        if let Some(close) = self.close.take() {
            let _ = close.send(());
            self.wake
                .send_modify(|revision| *revision = revision.wrapping_add(1));
        }
        match self.closed.take() {
            Some(closed) => PendingTcpClose {
                closed: Some(closed),
                outcome: None,
            },
            None => PendingTcpClose {
                closed: None,
                outcome: Some(TcpCloseOutcome::Failed {
                    kind: io::ErrorKind::BrokenPipe,
                    message: "native TCP close acknowledgement is unavailable".to_owned(),
                }),
            },
        }
    }

    async fn close(&mut self) -> io::Result<()> {
        self.begin_close().wait_cancel_safe().await
    }
}

enum TcpCloseOutcome {
    Completed,
    Failed {
        kind: io::ErrorKind,
        message: String,
    },
}

impl TcpCloseOutcome {
    fn result(&self) -> io::Result<()> {
        match self {
            Self::Completed => Ok(()),
            Self::Failed { kind, message } => Err(io::Error::new(*kind, message.clone())),
        }
    }
}

/// Move-only acknowledgement for one exact userspace TCP socket removal.
///
/// Creating this value has already synchronously signalled the socket entry
/// directly bound to the originating stream. Waiting is cancellation safe:
/// dropping a `wait_cancel_safe` future leaves the acknowledgement receiver in
/// this token, so another wait can resume. Once resolved, later waits replay
/// the same terminal outcome (`Ok(())` after removal, or the same error).
#[must_use = "wait for exact TCP socket removal before reusing its port or security state"]
pub struct PendingTcpClose {
    closed: Option<oneshot::Receiver<()>>,
    outcome: Option<TcpCloseOutcome>,
}

impl PendingTcpClose {
    pub async fn wait_cancel_safe(&mut self) -> io::Result<()> {
        if let Some(outcome) = &self.outcome {
            return outcome.result();
        }

        let result = match self.closed.as_mut() {
            Some(closed) => closed.await.map_err(|_| runtime_closed()),
            None => Err(runtime_closed()),
        };
        self.closed = None;
        self.outcome = Some(match &result {
            Ok(()) => TcpCloseOutcome::Completed,
            Err(error) => TcpCloseOutcome::Failed {
                kind: error.kind(),
                message: error.to_string(),
            },
        });
        result
    }
}

/// Move-only pending outgoing stream. Dropping this value, including by
/// cancelling `connect`, signals only its directly bound socket entry.
pub struct PendingTcpStream {
    stream: DuplexStream,
    ready: oneshot::Receiver<io::Result<()>>,
    lease: SocketLease,
}

impl PendingTcpStream {
    pub async fn connect(self) -> io::Result<ManagedTcpStream> {
        let Self {
            stream,
            ready,
            mut lease,
        } = self;
        let established = ready.await.unwrap_or_else(|_| {
            Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "native TCP dial ended before establishment",
            ))
        });
        if let Err(error) = established {
            // The ready error is terminal. Wait for exact-entry removal before
            // returning so a coordinator may safely clear an installed SA.
            let _ = lease.close().await;
            return Err(error);
        }
        Ok(ManagedTcpStream {
            stream,
            _lease: lease,
        })
    }

    /// Complete the handshake under the registration owner's absolute
    /// deadline and cancellation signal. Every terminal path waits for exact
    /// socket removal before returning an error.
    pub async fn connect_until(
        self,
        deadline: tokio::time::Instant,
        mut stop: watch::Receiver<bool>,
    ) -> io::Result<ManagedTcpStream> {
        let Self {
            stream,
            mut ready,
            mut lease,
        } = self;
        let established = loop {
            if *stop.borrow() || stop.has_changed().is_err() {
                break Err(io::Error::new(
                    io::ErrorKind::Interrupted,
                    "native TCP dial cancelled",
                ));
            }
            if tokio::time::Instant::now() >= deadline {
                break Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "native TCP dial deadline exceeded",
                ));
            }
            tokio::select! {
                biased;
                changed = stop.changed() => {
                    if changed.is_err() || *stop.borrow() {
                        break Err(io::Error::new(io::ErrorKind::Interrupted,"native TCP dial cancelled"));
                    }
                }
                _ = tokio::time::sleep_until(deadline) => {
                    break Err(io::Error::new(io::ErrorKind::TimedOut,"native TCP dial deadline exceeded"));
                }
                result = &mut ready => {
                    break result.unwrap_or_else(|_| Err(io::Error::new(
                        io::ErrorKind::ConnectionAborted,
                        "native TCP dial ended before establishment",
                    )));
                }
            }
        };
        if let Err(error) = established {
            let _ = lease.close().await;
            return Err(error);
        }
        Ok(ManagedTcpStream {
            stream,
            _lease: lease,
        })
    }

    /// Abort the pending socket and wait until the network owner has removed
    /// its exact entry from the SocketSet.
    pub async fn close(mut self) -> io::Result<()> {
        self.lease.close().await
    }
}

/// Move-only one-shot listener. It accepts at most one connection and may be
/// restricted to an exact peer address and source port. There is deliberately
/// no idle timeout while it waits for an incoming call. After accept/failure,
/// the registration coordinator must arm the next one-shot listener.
pub struct PendingTcpListener {
    stream: Option<DuplexStream>,
    ready: oneshot::Receiver<io::Result<()>>,
    lease: Option<SocketLease>,
}

impl PendingTcpListener {
    /// Wait for this one-shot listener without consuming its owner while the
    /// handshake is pending. Dropping the returned future is cancellation
    /// safe: the receiver, stream, and exact socket lease remain attached to
    /// `self`, so a later call can continue waiting for the same connection.
    ///
    /// A successful call consumes the one-shot listener state and returns its
    /// managed stream. A terminal listener error first waits for the network
    /// owner to remove the exact socket entry before it is returned.
    pub async fn accept_cancel_safe(&mut self) -> io::Result<ManagedTcpStream> {
        if self.stream.is_none() || self.lease.is_none() {
            return Err(io::Error::new(
                io::ErrorKind::NotConnected,
                "native TCP listener has already been consumed",
            ));
        }
        let established = (&mut self.ready).await.unwrap_or_else(|_| {
            Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "native TCP listener ended before accept",
            ))
        });
        let stream = self
            .stream
            .take()
            .expect("listener stream checked before awaiting accept");
        let mut lease = self
            .lease
            .take()
            .expect("listener lease checked before awaiting accept");
        if let Err(error) = established {
            drop(stream);
            let _ = lease.close().await;
            return Err(error);
        }
        Ok(ManagedTcpStream {
            stream,
            _lease: lease,
        })
    }

    /// Backwards-compatible consuming form for coordinators which do not need
    /// to race an incoming connection against other long-lived session work.
    pub async fn accept(mut self) -> io::Result<ManagedTcpStream> {
        self.accept_cancel_safe().await
    }

    /// Stop listening and wait until the exact socket is no longer owned by
    /// the network task.
    pub async fn close(mut self) -> io::Result<()> {
        // Closing a listener also makes its application half unavailable. The
        // exact socket lease is kept until its removal acknowledgement arrives.
        self.stream.take();
        match self.lease.as_mut() {
            Some(lease) => lease.close().await,
            None => Ok(()),
        }
    }
}

/// Application half of a userspace TCP connection. The socket remains bound
/// to this move-only value; dropping it cannot delete a later socket which
/// happens to reuse the same smoltcp handle.
pub struct ManagedTcpStream {
    stream: DuplexStream,
    _lease: SocketLease,
}

impl ManagedTcpStream {
    /// Signal this stream's exact socket entry synchronously and return its
    /// cancellation-safe removal acknowledgement.
    pub fn begin_close(self) -> PendingTcpClose {
        let Self { stream, mut _lease } = self;
        let pending = _lease.begin_close();
        drop(stream);
        pending
    }

    /// Abort this connection and wait for exact-entry removal. Session
    /// teardown uses this barrier before clearing the IMS security lease.
    pub async fn close(self) -> io::Result<()> {
        self.begin_close().wait_cancel_safe().await
    }
}

impl proletarius_ims::registration_connection::RegistrationTransportRetire for ManagedTcpStream {
    async fn retire(self) -> io::Result<()> {
        self.close().await
    }
}

impl AsyncRead for ManagedTcpStream {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_read(cx, buffer)
    }
}

impl AsyncWrite for ManagedTcpStream {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &[u8],
    ) -> Poll<Result<usize, io::Error>> {
        Pin::new(&mut self.stream).poll_write(cx, buffer)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<(), io::Error>> {
        Pin::new(&mut self.stream).poll_flush(cx)
    }

    fn poll_shutdown(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Result<(), io::Error>> {
        Pin::new(&mut self.stream).poll_shutdown(cx)
    }
}

enum EntryMode {
    Dial {
        peer: IpEndpoint,
        local_port: u16,
    },
    Listen {
        local_port: u16,
        expected_peer: Option<IpEndpoint>,
    },
}

struct DynamicEntry {
    identity: SocketIdentity,
    mode: EntryMode,
    stream: DuplexStream,
    ready: Option<oneshot::Sender<io::Result<()>>>,
    waker: Waker,
    started: tokio::time::Instant,
    established: bool,
    input_closed: bool,
    output_closed: bool,
    close: oneshot::Receiver<()>,
    closed: Option<oneshot::Sender<()>>,
}

impl DynamicEntry {
    fn fail_ready(&mut self, error: io::Error) {
        if let Some(ready) = self.ready.take() {
            let _ = ready.send(Err(error));
        }
    }

    fn poll(&mut self, sockets: &mut SocketSet<'static>) -> bool {
        let socket = sockets.get_mut::<tcp::Socket>(self.identity.handle);
        match self.close.try_recv() {
            Ok(()) | Err(oneshot::error::TryRecvError::Closed) => {
                socket.abort();
                return false;
            }
            Err(oneshot::error::TryRecvError::Empty) => {}
        }
        if self.ready.as_ref().is_some_and(oneshot::Sender::is_closed) {
            socket.abort();
            return false;
        }

        if !self.established
            && matches!(
                socket.state(),
                tcp::State::Established | tcp::State::CloseWait
            )
        {
            if let EntryMode::Listen {
                expected_peer: Some(expected),
                ..
            } = self.mode
                && socket.remote_endpoint() != Some(expected)
            {
                socket.abort();
                self.fail_ready(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "native TCP listener received an unexpected peer",
                ));
                return false;
            }
            self.established = true;
            if self
                .ready
                .take()
                .is_none_or(|ready| ready.send(Ok(())).is_err())
            {
                socket.abort();
                return false;
            }
        }

        if !self.established {
            if socket.state() == tcp::State::Closed {
                self.fail_ready(io::Error::new(
                    io::ErrorKind::ConnectionRefused,
                    "native TCP peer refused or reset the connection",
                ));
                return false;
            }
            if matches!(self.mode, EntryMode::Dial { .. })
                && self.started.elapsed() >= Duration::from_secs(30)
            {
                socket.abort();
                self.fail_ready(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "native TCP handshake timed out",
                ));
                return false;
            }
            return true;
        }

        if socket.state() == tcp::State::Closed {
            return false;
        }

        let mut cx = Context::from_waker(&self.waker);
        if socket.can_recv()
            && socket
                .recv(
                    |data| match Pin::new(&mut self.stream).poll_write(&mut cx, data) {
                        Poll::Ready(Ok(n)) => (n, Ok(())),
                        Poll::Ready(Err(error)) => (0, Err(error)),
                        Poll::Pending => (0, Ok(())),
                    },
                )
                .is_err()
        {
            socket.abort();
            return false;
        }
        if !socket.may_recv()
            && !self.output_closed
            && let Poll::Ready(result) = Pin::new(&mut self.stream).poll_shutdown(&mut cx)
        {
            if result.is_err() {
                socket.abort();
                return false;
            }
            self.output_closed = true;
        }
        if socket.can_send() && !self.input_closed {
            let mut eof = false;
            let sent = socket.send(|space| {
                let mut buffer = ReadBuf::new(space);
                match Pin::new(&mut self.stream).poll_read(&mut cx, &mut buffer) {
                    Poll::Ready(Ok(())) => {
                        let length = buffer.filled().len();
                        eof = length == 0;
                        (length, Ok(()))
                    }
                    Poll::Ready(Err(error)) => (0, Err(error)),
                    Poll::Pending => (0, Ok(())),
                }
            });
            if sent.is_err() {
                socket.abort();
                return false;
            }
            if eof {
                self.input_closed = true;
                socket.close();
            }
        }
        true
    }
}

/// Dynamic socket registry owned and serviced by exactly one tunnel network
/// task. It never turns a single TCP connection failure into a tunnel error.
pub struct TcpSocketRegistry {
    commands: mpsc::Receiver<TcpCommand>,
    wake: watch::Sender<u64>,
    next_generation: u64,
    entries: Vec<DynamicEntry>,
}

impl TcpSocketRegistry {
    pub fn new(wake: watch::Sender<u64>) -> (TcpControl, Self) {
        let (commands, command_receiver) = mpsc::channel(COMMAND_CAPACITY);
        (
            TcpControl {
                commands,
                wake: wake.clone(),
            },
            Self {
                commands: command_receiver,
                wake,
                next_generation: 1,
                entries: Vec::with_capacity(DYNAMIC_SOCKET_CAPACITY),
            },
        )
    }

    pub fn service(&mut self, interface: &mut Interface, sockets: &mut SocketSet<'static>) {
        self.apply_commands(interface, sockets);

        let mut index = 0;
        while index < self.entries.len() {
            if self.entries[index].poll(sockets) {
                index += 1;
            } else {
                self.remove_entry(index, sockets);
            }
        }
    }

    fn next_identity(&mut self, handle: SocketHandle) -> SocketIdentity {
        let identity = SocketIdentity {
            generation: self.next_generation,
            handle,
        };
        self.next_generation = self.next_generation.wrapping_add(1).max(1);
        identity
    }

    fn apply_commands(&mut self, interface: &mut Interface, sockets: &mut SocketSet<'static>) {
        for _ in 0..COMMANDS_PER_SERVICE {
            let command = match self.commands.try_recv() {
                Ok(command) => command,
                Err(mpsc::error::TryRecvError::Empty | mpsc::error::TryRecvError::Disconnected) => {
                    break;
                }
            };
            match command {
                TcpCommand::Dial {
                    peer,
                    local_port,
                    reply,
                } => self.create_dial(interface, sockets, peer, local_port, reply),
                TcpCommand::Listen {
                    local_port,
                    expected_peer,
                    reply,
                } => self.create_listener(sockets, local_port, expected_peer, reply),
            }
        }
    }

    fn has_capacity(&self, sockets: &SocketSet<'static>) -> bool {
        self.entries.len() < DYNAMIC_SOCKET_CAPACITY
            && sockets.iter().count() < DYNAMIC_SOCKET_CAPACITY
    }

    fn dial_conflicts(&self, peer: IpEndpoint, local_port: u16) -> bool {
        self.entries.iter().any(|entry| {
            matches!(
                entry.mode,
                EntryMode::Dial {
                    peer: existing_peer,
                    local_port: existing_port,
                } if existing_peer == peer && existing_port == local_port
            )
        })
    }

    fn listener_conflicts(&self, local_port: u16) -> bool {
        self.entries.iter().any(|entry| {
            !entry.established
                && matches!(
                    entry.mode,
                    EntryMode::Listen {
                        local_port: existing_port,
                        ..
                    } if existing_port == local_port
                )
        })
    }

    fn create_dial(
        &mut self,
        interface: &mut Interface,
        sockets: &mut SocketSet<'static>,
        peer: SocketAddr,
        local_port: u16,
        reply: oneshot::Sender<io::Result<PendingTcpStream>>,
    ) {
        if reply.is_closed() {
            return;
        }
        if !self.has_capacity(sockets) {
            let _ = reply.send(Err(io::Error::new(
                io::ErrorKind::WouldBlock,
                "native TCP socket capacity reached",
            )));
            return;
        }
        let endpoint = IpEndpoint::new(peer.ip().into(), peer.port());
        if self.dial_conflicts(endpoint, local_port) {
            let _ = reply.send(Err(io::Error::new(
                io::ErrorKind::AddrInUse,
                "native TCP four-tuple is already active",
            )));
            return;
        }
        let mut socket = new_socket();
        if socket
            .connect(interface.context(), endpoint, local_port)
            .is_err()
        {
            let _ = reply.send(Err(io::Error::new(
                io::ErrorKind::AddrNotAvailable,
                "native TCP source/remote unavailable",
            )));
            return;
        }
        let handle = sockets.add(socket);
        let identity = self.next_identity(handle);
        let (application, stream) = tokio::io::duplex(BUFFER);
        let (ready, receiver) = oneshot::channel();
        let (close, close_receiver) = oneshot::channel();
        let (closed, closed_receiver) = oneshot::channel();
        self.entries.push(DynamicEntry {
            identity,
            mode: EntryMode::Dial {
                peer: endpoint,
                local_port,
            },
            stream,
            ready: Some(ready),
            waker: Waker::from(Arc::new(NetworkWake(self.wake.clone()))),
            started: tokio::time::Instant::now(),
            established: false,
            input_closed: false,
            output_closed: false,
            close: close_receiver,
            closed: Some(closed),
        });
        let pending = PendingTcpStream {
            stream: application,
            ready: receiver,
            lease: SocketLease {
                close: Some(close),
                closed: Some(closed_receiver),
                wake: self.wake.clone(),
            },
        };
        if let Err(returned) = reply.send(Ok(pending)) {
            drop(returned);
            if let Some(index) = self
                .entries
                .iter()
                .position(|entry| entry.identity == identity)
            {
                self.remove_entry(index, sockets);
            }
        }
    }

    fn create_listener(
        &mut self,
        sockets: &mut SocketSet<'static>,
        local_port: u16,
        expected_peer: Option<SocketAddr>,
        reply: oneshot::Sender<io::Result<PendingTcpListener>>,
    ) {
        if reply.is_closed() {
            return;
        }
        if !self.has_capacity(sockets) {
            let _ = reply.send(Err(io::Error::new(
                io::ErrorKind::WouldBlock,
                "native TCP socket capacity reached",
            )));
            return;
        }
        if self.listener_conflicts(local_port) {
            let _ = reply.send(Err(io::Error::new(
                io::ErrorKind::AddrInUse,
                "native TCP listener port is already armed",
            )));
            return;
        }
        let mut socket = new_socket();
        if socket.listen(local_port).is_err() {
            let _ = reply.send(Err(io::Error::new(
                io::ErrorKind::AddrInUse,
                "native TCP listener unavailable",
            )));
            return;
        }
        let handle = sockets.add(socket);
        let identity = self.next_identity(handle);
        let (application, stream) = tokio::io::duplex(BUFFER);
        let (ready, receiver) = oneshot::channel();
        let (close, close_receiver) = oneshot::channel();
        let (closed, closed_receiver) = oneshot::channel();
        self.entries.push(DynamicEntry {
            identity,
            mode: EntryMode::Listen {
                local_port,
                expected_peer: expected_peer
                    .map(|peer| IpEndpoint::new(peer.ip().into(), peer.port())),
            },
            stream,
            ready: Some(ready),
            waker: Waker::from(Arc::new(NetworkWake(self.wake.clone()))),
            started: tokio::time::Instant::now(),
            established: false,
            input_closed: false,
            output_closed: false,
            close: close_receiver,
            closed: Some(closed),
        });
        let pending = PendingTcpListener {
            stream: Some(application),
            ready: receiver,
            lease: Some(SocketLease {
                close: Some(close),
                closed: Some(closed_receiver),
                wake: self.wake.clone(),
            }),
        };
        if let Err(returned) = reply.send(Ok(pending)) {
            drop(returned);
            if let Some(index) = self
                .entries
                .iter()
                .position(|entry| entry.identity == identity)
            {
                self.remove_entry(index, sockets);
            }
        }
    }

    fn remove_entry(&mut self, index: usize, sockets: &mut SocketSet<'static>) {
        let mut entry = self.entries.swap_remove(index);
        let socket = sockets.get_mut::<tcp::Socket>(entry.identity.handle);
        socket.abort();
        sockets.remove(entry.identity.handle);
        entry.fail_ready(io::Error::new(
            io::ErrorKind::ConnectionAborted,
            "native TCP socket was cancelled",
        ));
        if let Some(closed) = entry.closed.take() {
            let _ = closed.send(());
        }
    }
}

impl NetworkRuntime for TcpSocketRegistry {
    type ApplicationError = Infallible;

    fn service(
        &mut self,
        interface: &mut Interface,
        sockets: &mut SocketSet<'static>,
    ) -> Result<(), Self::ApplicationError> {
        TcpSocketRegistry::service(self, interface, sockets);
        Ok(())
    }

    fn transform_packet(
        &mut self,
        _direction: PacketDirection,
        packet: Zeroizing<Vec<u8>>,
    ) -> Result<PacketTransformOutcome, io::Error> {
        Ok(PacketTransformOutcome::Forward(packet))
    }
}

fn new_socket() -> tcp::Socket<'static> {
    tcp::Socket::new(
        tcp::SocketBuffer::new(vec![0; BUFFER]),
        tcp::SocketBuffer::new(vec![0; BUFFER]),
    )
}

#[cfg(test)]
struct ConnectingTcpStream {
    stream: DuplexStream,
    ready: oneshot::Receiver<io::Result<()>>,
}
#[cfg(test)]
impl ConnectingTcpStream {
    /// Resolves only after the TCP handshake, never merely after queueing SYN.
    async fn connect(self) -> io::Result<DuplexStream> {
        self.ready.await.map_err(|_| {
            io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "native TCP connection ended before establishment",
            )
        })??;
        Ok(self.stream)
    }
}

#[cfg(test)]
struct TcpStreamDriver {
    handle: SocketHandle,
    peer: IpEndpoint,
    local_port: u16,
    stream: DuplexStream,
    ready: Option<oneshot::Sender<io::Result<()>>>,
    waker: Waker,
    started: Option<tokio::time::Instant>,
    established: bool,
    input_closed: bool,
    output_closed: bool,
}

#[cfg(test)]
impl TcpStreamDriver {
    /// Install before starting TunnelNetwork. No system socket is opened.
    /// Pass into_service() and the wake RX to that network's run method.
    fn install(
        sockets: &mut SocketSet<'static>,
        peer: SocketAddr,
        local_port: u16,
    ) -> io::Result<(Self, ConnectingTcpStream, watch::Receiver<u64>)> {
        if sockets.iter().count() >= 16
            || peer.port() == 0
            || local_port == 0
            || peer.ip().is_unspecified()
            || peer.ip().is_multicast()
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "invalid native TCP endpoint or socket capacity",
            ));
        }
        let handle = sockets.add(tcp::Socket::new(
            tcp::SocketBuffer::new(vec![0; BUFFER]),
            tcp::SocketBuffer::new(vec![0; BUFFER]),
        ));
        let (application, stream) = tokio::io::duplex(BUFFER);
        let (ready, receiver) = oneshot::channel();
        let (wake_tx, wake_rx) = watch::channel(0);
        let waker = Waker::from(Arc::new(NetworkWake(wake_tx)));
        Ok((
            Self {
                handle,
                peer: IpEndpoint::new(peer.ip().into(), peer.port()),
                local_port,
                stream,
                ready: Some(ready),
                waker,
                started: None,
                established: false,
                input_closed: false,
                output_closed: false,
            },
            ConnectingTcpStream {
                stream: application,
                ready: receiver,
            },
            wake_rx,
        ))
    }

    /// Transfer ownership into the network future, so its termination drops
    /// the pending-connect sender and duplex endpoint, waking application IO.
    fn into_service(
        mut self,
    ) -> impl FnMut(&mut Interface, &mut SocketSet<'static>) -> io::Result<()> {
        move |interface, sockets| {
            let result = self.poll(interface, sockets);
            if let Err(error) = &result
                && let Some(ready) = self.ready.take()
            {
                let _ = ready.send(Err(io::Error::new(error.kind(), error.to_string())));
            }
            result
        }
    }

    fn poll(
        &mut self,
        interface: &mut Interface,
        sockets: &mut SocketSet<'static>,
    ) -> io::Result<()> {
        let socket = sockets.get_mut::<tcp::Socket>(self.handle);
        if self.ready.as_ref().is_some_and(|ready| ready.is_closed()) {
            socket.abort();
            return Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "native TCP connect cancelled",
            ));
        }
        if self.started.is_none() {
            socket
                .connect(interface.context(), self.peer, self.local_port)
                .map_err(|_| {
                    io::Error::new(
                        io::ErrorKind::AddrNotAvailable,
                        "native TCP source/remote unavailable",
                    )
                })?;
            self.started = Some(tokio::time::Instant::now());
        }
        if !self.established {
            if matches!(
                socket.state(),
                tcp::State::Established | tcp::State::CloseWait
            ) {
                self.established = true;
                if self.ready.take().unwrap().send(Ok(())).is_err() {
                    socket.abort();
                    return Err(io::Error::new(
                        io::ErrorKind::ConnectionAborted,
                        "native TCP consumer disappeared",
                    ));
                }
            } else if self.started.unwrap().elapsed() >= Duration::from_secs(30) {
                socket.abort();
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "native TCP connect timed out",
                ));
            }
        }
        if socket.state() == tcp::State::Closed && !(self.input_closed && self.output_closed) {
            return Err(io::Error::new(
                io::ErrorKind::ConnectionReset,
                "native TCP peer closed/reset",
            ));
        }
        let mut cx = Context::from_waker(&self.waker);
        if socket.can_recv() {
            socket
                .recv(
                    |data| match Pin::new(&mut self.stream).poll_write(&mut cx, data) {
                        Poll::Ready(Ok(n)) => (n, Ok(())),
                        Poll::Ready(Err(error)) => (0, Err(error)),
                        Poll::Pending => (0, Ok(())),
                    },
                )
                .map_err(|_| {
                    io::Error::new(io::ErrorKind::ConnectionReset, "native TCP receive failed")
                })??;
        }
        if self.established
            && !socket.may_recv()
            && !self.output_closed
            && let Poll::Ready(result) = Pin::new(&mut self.stream).poll_shutdown(&mut cx)
        {
            result?;
            self.output_closed = true;
        }
        if socket.can_send() && !self.input_closed {
            let mut eof = false;
            socket
                .send(|space| {
                    let mut buffer = ReadBuf::new(space);
                    match Pin::new(&mut self.stream).poll_read(&mut cx, &mut buffer) {
                        Poll::Ready(Ok(())) => {
                            let n = buffer.filled().len();
                            eof = n == 0;
                            (n, Ok(()))
                        }
                        Poll::Ready(Err(error)) => (0, Err(error)),
                        Poll::Pending => (0, Ok(())),
                    }
                })
                .map_err(|_| {
                    io::Error::new(io::ErrorKind::BrokenPipe, "native TCP send failed")
                })??;
            if eof {
                self.input_closed = true;
                socket.close();
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{TunnelConfiguration, netstack_runtime::TunnelNetwork};
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        sync::mpsc,
    };

    #[tokio::test]
    async fn cancelled_listener_accept_can_resume_and_receive_connection() {
        let (application, mut peer) = tokio::io::duplex(BUFFER);
        let (ready_sender, ready) = oneshot::channel();
        let (close_sender, close_receiver) = oneshot::channel();
        let (closed_sender, closed_receiver) = oneshot::channel();
        let (wake, _wake_receiver) = watch::channel(0);
        let mut listener = PendingTcpListener {
            stream: Some(application),
            ready,
            lease: Some(SocketLease {
                close: Some(close_sender),
                closed: Some(closed_receiver),
                wake,
            }),
        };

        {
            let mut cancelled = Box::pin(listener.accept_cancel_safe());
            let mut context = Context::from_waker(Waker::noop());
            assert!(matches!(
                cancelled.as_mut().poll(&mut context),
                Poll::Pending
            ));
        }

        ready_sender.send(Ok(())).unwrap();
        let mut accepted = listener.accept_cancel_safe().await.unwrap();
        accepted.write_all(b"INVITE").await.unwrap();
        let mut message = [0; 6];
        peer.read_exact(&mut message).await.unwrap();
        assert_eq!(&message, b"INVITE");
        let second_error = match listener.accept_cancel_safe().await {
            Ok(_) => panic!("one-shot listener accepted twice"),
            Err(error) => error,
        };
        assert_eq!(second_error.kind(), io::ErrorKind::NotConnected);

        let removal = async {
            close_receiver.await.unwrap();
            closed_sender.send(()).unwrap();
        };
        let ((), close_result) = tokio::join!(removal, accepted.close());
        close_result.unwrap();
    }

    #[tokio::test]
    async fn cancelled_exact_close_wait_can_resume_before_immediate_port_reuse() {
        let (wake_tx, _wake) = watch::channel(0);
        let (_control, mut registry) = TcpSocketRegistry::new(wake_tx.clone());
        let mut sockets = SocketSet::new(vec![]);
        let local_port = 5060;

        let handle = sockets.add(new_socket());
        let identity = registry.next_identity(handle);
        let (application, network_stream) = tokio::io::duplex(BUFFER);
        let (close, close_receiver) = oneshot::channel();
        let (closed, closed_receiver) = oneshot::channel();
        registry.entries.push(DynamicEntry {
            identity,
            mode: EntryMode::Listen {
                local_port,
                expected_peer: None,
            },
            stream: network_stream,
            ready: None,
            waker: Waker::from(Arc::new(NetworkWake(wake_tx.clone()))),
            started: tokio::time::Instant::now(),
            established: false,
            input_closed: false,
            output_closed: false,
            close: close_receiver,
            closed: Some(closed),
        });
        let stream = ManagedTcpStream {
            stream: application,
            _lease: SocketLease {
                close: Some(close),
                closed: Some(closed_receiver),
                wake: wake_tx,
            },
        };
        assert!(registry.listener_conflicts(local_port));

        // begin_close signals the exact entry before returning. Cancelling
        // only the first acknowledgement wait must not lose its receiver.
        let mut pending_close = stream.begin_close();
        {
            let mut cancelled = Box::pin(pending_close.wait_cancel_safe());
            let mut context = Context::from_waker(Waker::noop());
            assert!(matches!(
                cancelled.as_mut().poll(&mut context),
                Poll::Pending
            ));
        }

        assert!(!registry.entries[0].poll(&mut sockets));
        registry.remove_entry(0, &mut sockets);
        pending_close.wait_cancel_safe().await.unwrap();
        // Completion is explicit and idempotent for callers which need to
        // retry cleanup after a cancelled orchestration branch.
        pending_close.wait_cancel_safe().await.unwrap();
        assert!(registry.entries.is_empty());
        assert_eq!(sockets.iter().count(), 0);

        // Exact removal is a barrier: the same listen port can be allocated
        // immediately, without waiting for another network-service turn.
        let (reply, result) = oneshot::channel();
        registry.create_listener(&mut sockets, local_port, None, reply);
        let listener = result.await.unwrap().unwrap();
        assert_eq!(registry.entries.len(), 1);
        drop(listener);
        assert!(!registry.entries[0].poll(&mut sockets));
        registry.remove_entry(0, &mut sockets);
    }

    #[tokio::test]
    async fn late_close_signal_cannot_remove_reused_socket_handle() {
        let (wake_tx, _wake) = watch::channel(0);
        let (_control, mut registry) = TcpSocketRegistry::new(wake_tx.clone());
        let mut sockets = SocketSet::new(vec![]);

        let old_handle = sockets.add(new_socket());
        let old_identity = registry.next_identity(old_handle);
        let (_old_application, old_stream) = tokio::io::duplex(BUFFER);
        let (old_close, old_close_receiver) = oneshot::channel();
        let (old_closed, old_closed_receiver) = oneshot::channel();
        let old_lease = SocketLease {
            close: Some(old_close),
            closed: Some(old_closed_receiver),
            wake: wake_tx.clone(),
        };
        registry.entries.push(DynamicEntry {
            identity: old_identity,
            mode: EntryMode::Dial {
                peer: IpEndpoint::new("10.0.0.9".parse::<std::net::IpAddr>().unwrap().into(), 5060),
                local_port: 5050,
            },
            stream: old_stream,
            ready: None,
            waker: Waker::from(Arc::new(NetworkWake(wake_tx.clone()))),
            started: tokio::time::Instant::now(),
            established: true,
            input_closed: false,
            output_closed: false,
            close: old_close_receiver,
            closed: Some(old_closed),
        });
        registry.remove_entry(0, &mut sockets);

        let reused_handle = sockets.add(new_socket());
        assert_eq!(old_handle, reused_handle);
        let current_identity = registry.next_identity(reused_handle);
        assert_ne!(old_identity.generation, current_identity.generation);
        let (_application, stream) = tokio::io::duplex(BUFFER);
        let (current_close, current_close_receiver) = oneshot::channel();
        let (current_closed, current_closed_receiver) = oneshot::channel();
        let current_lease = SocketLease {
            close: Some(current_close),
            closed: Some(current_closed_receiver),
            wake: wake_tx.clone(),
        };
        registry.entries.push(DynamicEntry {
            identity: current_identity,
            mode: EntryMode::Dial {
                peer: IpEndpoint::new("10.0.0.9".parse::<std::net::IpAddr>().unwrap().into(), 5060),
                local_port: 5050,
            },
            stream,
            ready: None,
            waker: Waker::from(Arc::new(NetworkWake(wake_tx))),
            started: tokio::time::Instant::now(),
            established: true,
            input_closed: false,
            output_closed: false,
            close: current_close_receiver,
            closed: Some(current_closed),
        });

        // The old signal is wired directly to the removed entry, not routed
        // by the reusable SocketHandle. Dropping it cannot touch the new one.
        drop(old_lease);
        assert_eq!(registry.entries.len(), 1);
        assert_eq!(registry.entries[0].identity, current_identity);

        drop(current_lease);
        assert!(!registry.entries[0].poll(&mut sockets));
        registry.remove_entry(0, &mut sockets);
        assert!(registry.entries.is_empty());
        assert_eq!(sockets.iter().count(), 0);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn dynamic_registry_isolates_failures_and_reclaims_cancelled_sockets() {
        let config = |ip: &str| TunnelConfiguration {
            ipv4_addresses: vec![ip.parse().unwrap()],
            ..Default::default()
        };
        let client_network = TunnelNetwork::new(&config("10.0.0.2")).unwrap();
        let server_network = TunnelNetwork::new(&config("10.20.30.9")).unwrap();
        let (client_wake_tx, client_wake) = watch::channel(0);
        let (server_wake_tx, server_wake) = watch::channel(0);
        let (client_control, client_registry) = TcpSocketRegistry::new(client_wake_tx);
        let (server_control, server_registry) = TcpSocketRegistry::new(server_wake_tx);

        // Poll through command enqueue, then cancel before the registry can
        // acknowledge. The closed reply must make both commands no-ops when
        // their network tasks start.
        {
            let mut cancelled =
                Box::pin(client_control.dial("10.20.30.9:5098".parse().unwrap(), 29_998));
            let mut context = Context::from_waker(Waker::noop());
            assert!(matches!(
                cancelled.as_mut().poll(&mut context),
                Poll::Pending
            ));
        }
        {
            let mut cancelled = Box::pin(server_control.listen(5098, None));
            let mut context = Context::from_waker(Waker::noop());
            assert!(matches!(
                cancelled.as_mut().poll(&mut context),
                Poll::Pending
            ));
        }
        let (client_to_server, server_inbound) = mpsc::channel(256);
        let (server_to_client, client_inbound) = mpsc::channel(256);
        let (stop_tx, stop) = watch::channel(false);

        let client_runner = client_network.run_managed(
            SocketSet::new(vec![]),
            client_inbound,
            client_to_server,
            stop.clone(),
            client_wake,
            client_registry,
        );
        let server_runner = server_network.run_managed(
            SocketSet::new(vec![]),
            server_inbound,
            server_to_client,
            stop,
            server_wake,
            server_registry,
        );
        let application = async {
            // Each successful command ack creates a socket. Dropping its
            // pending value must reclaim it before repeated use can consume
            // the sixteen-socket capacity.
            for local_port in 30_000..30_100 {
                let pending = client_control
                    .dial("10.20.30.9:5099".parse().unwrap(), local_port)
                    .await
                    .unwrap();
                // Creating then dropping the handshake future models a
                // cancelled connect waiter, not merely an unused handle.
                drop(pending.connect());
            }

            // A real RST/refusal is scoped to just this socket; both network
            // tasks must remain available for the next dial/listen pair.
            let refused = client_control
                .dial("10.20.30.9:5099".parse().unwrap(), 31_000)
                .await
                .unwrap();
            let error = match refused.connect().await {
                Ok(_) => panic!("unopened port unexpectedly connected"),
                Err(error) => error,
            };
            assert_eq!(error.kind(), io::ErrorKind::ConnectionRefused);

            let mut listener = server_control
                .listen(5060, Some("10.0.0.2:32000".parse().unwrap()))
                .await
                .unwrap();

            // A long-lived session may poll the listener in `select!` and
            // cancel that wait whenever another event wins. Polling once and
            // dropping the future must preserve the same receiver and lease.
            {
                let mut cancelled = Box::pin(listener.accept_cancel_safe());
                let mut context = Context::from_waker(Waker::noop());
                assert!(matches!(
                    cancelled.as_mut().poll(&mut context),
                    Poll::Pending
                ));
            }
            let duplicate_listener = server_control.listen(5060, None).await;
            assert!(matches!(
                duplicate_listener,
                Err(error) if error.kind() == io::ErrorKind::AddrInUse
            ));
            let dial = client_control
                .dial("10.20.30.9:5060".parse().unwrap(), 32_000)
                .await
                .unwrap();
            let duplicate_dial = client_control
                .dial("10.20.30.9:5060".parse().unwrap(), 32_000)
                .await;
            assert!(matches!(
                duplicate_dial,
                Err(error) if error.kind() == io::ErrorKind::AddrInUse
            ));
            let (client, server) = tokio::join!(dial.connect(), listener.accept_cancel_safe());
            let mut client = client.unwrap();
            let mut server = server.unwrap();
            client.write_all(b"REGISTER").await.unwrap();
            let mut request = [0; 8];
            server.read_exact(&mut request).await.unwrap();
            assert_eq!(&request, b"REGISTER");
            server.write_all(b"200 OK").await.unwrap();
            let mut response = [0; 6];
            client.read_exact(&mut response).await.unwrap();
            assert_eq!(&response, b"200 OK");
            client.close().await.unwrap();
            server.close().await.unwrap();
            stop_tx.send(true).unwrap();
        };

        let (client_result, server_result, ()) =
            tokio::time::timeout(Duration::from_secs(10), async {
                tokio::join!(client_runner, server_runner, application)
            })
            .await
            .unwrap();
        client_result.unwrap();
        server_result.unwrap();
    }

    #[tokio::test]
    async fn async_stream_crosses_ip_stack_with_segmentation() {
        stream_case(true, false).await;
        stream_case(false, false).await;
    }

    #[tokio::test]
    async fn async_sip_stream_crosses_installed_ims_packet_transform() {
        stream_case(true, true).await;
    }

    async fn stream_case(accept: bool, secured: bool) {
        use crate::netstack_runtime::{PacketDirection, PacketTransformOutcome};
        use proletarius_ims::{
            aka_exchange::SecurityKeys,
            security_offer::parse_security_server,
            security_policy::{LocalEndpoints, SecurityPolicy},
            security_transport::SecurityTransport,
        };
        use zeroize::Zeroizing;
        let transform = |client: bool| {
            let (own, peer, c, s, rc, rs, pc, ps, rpc, rps) = if client {
                (
                    "10.0.0.2",
                    "10.20.30.9",
                    111,
                    222,
                    333,
                    444,
                    5060,
                    5061,
                    5061,
                    5060,
                )
            } else {
                (
                    "10.20.30.9",
                    "10.0.0.2",
                    333,
                    444,
                    111,
                    222,
                    5061,
                    5060,
                    5060,
                    5061,
                )
            };
            let offer=parse_security_server(&format!("ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;spi-c={rc};spi-s={rs};port-c={rpc};port-s={rps}")).unwrap().remove(0);
            let mut transport = SecurityTransport::new(
                SecurityPolicy::aes_sha1(
                    own.parse().unwrap(),
                    peer.parse().unwrap(),
                    LocalEndpoints {
                        spi_c: c,
                        spi_s: s,
                        port_c: pc,
                        port_s: ps,
                    },
                    &offer,
                    SecurityKeys {
                        ck: Zeroizing::new([1; 16]),
                        ik: Zeroizing::new([2; 16]),
                    },
                )
                .unwrap(),
            )
            .unwrap();
            move |direction: PacketDirection, packet: Zeroizing<Vec<u8>>| {
                if !secured {
                    return Ok(PacketTransformOutcome::Forward(packet));
                }
                let disposition = match direction {
                    PacketDirection::Inbound => transport.route_inbound(packet),
                    PacketDirection::Outbound => transport.route_outbound(packet),
                }
                .map_err(|e| io::Error::other(format!("{e:?}")))?;
                Ok(match disposition {
                    proletarius_ims::security_transport::TransportDisposition::Protected(
                        packet,
                    )
                    | proletarius_ims::security_transport::TransportDisposition::Passthrough(
                        packet,
                    ) => PacketTransformOutcome::Forward(packet),
                    proletarius_ims::security_transport::TransportDisposition::Drop(_) => {
                        PacketTransformOutcome::Discard
                    }
                })
            }
        };
        let config = |ip: &str| TunnelConfiguration {
            ipv4_addresses: vec![ip.parse().unwrap()],
            ..Default::default()
        };
        let a = TunnelNetwork::new(&config("10.0.0.2")).unwrap();
        let b = TunnelNetwork::new(&config("10.20.30.9")).unwrap();
        let mut sa = SocketSet::new(vec![]);
        let (driver, pending, wake) =
            TcpStreamDriver::install(&mut sa, "10.20.30.9:5060".parse().unwrap(), 5060).unwrap();
        let mut sb = SocketSet::new(vec![]);
        let mut server = tcp::Socket::new(
            tcp::SocketBuffer::new(vec![0; BUFFER]),
            tcp::SocketBuffer::new(vec![0; BUFFER]),
        );
        if accept {
            server.listen(5060).unwrap();
        }
        let handle = sb.add(server);
        let (ab, ab_rx) = mpsc::channel(4);
        let (ba, ba_rx) = mpsc::channel(4);
        if secured {
            ba.try_send(Zeroizing::new(vec![0; 20])).unwrap();
        }
        let (stop_tx, stop) = watch::channel(false);
        let (_server_wake, server_wake) = watch::channel(0);
        let client = a.run_transformed(
            sa,
            ba_rx,
            ab,
            stop.clone(),
            wake,
            driver.into_service(),
            transform(true),
        );
        let mut received = Vec::new();
        let mut sent = 0;
        let server = b.run_transformed(
            sb,
            ab_rx,
            ba,
            stop,
            server_wake,
            |_, sockets| {
                let socket = sockets.get_mut::<tcp::Socket>(handle);
                if socket.can_recv() {
                    socket
                        .recv(|data| {
                            received.extend_from_slice(data);
                            (data.len(), ())
                        })
                        .unwrap();
                }
                if socket.can_send() && sent < received.len() {
                    sent += socket.send_slice(&received[sent..]).unwrap();
                }
                Ok::<_, ()>(())
            },
            transform(false),
        );
        let application = async {
            if !accept {
                assert!(pending.connect().await.is_err());
                stop_tx.send(true).unwrap();
                return;
            }
            let mut stream = pending.connect().await.unwrap();
            let mut data =
                b"MESSAGE sip:ims.example SIP/2.0\r\nContent-Length: 5000\r\n\r\n".to_vec();
            data.extend((0..5000).map(|n| (n % 251) as u8));
            stream.write_all(&data).await.unwrap();
            {
                use proletarius_ims::stream::{SipStreamItem, SipStreamReader};
                let mut reader = SipStreamReader::new(&mut stream);
                let Some(SipStreamItem::Message(reply)) = reader.next().await.unwrap() else {
                    panic!("missing SIP frame");
                };
                assert_eq!(&*reply, &data);
            }
            stream.shutdown().await.unwrap();
            stop_tx.send(true).unwrap();
        };
        let (a, b, ()) = tokio::time::timeout(Duration::from_secs(5), async {
            tokio::join!(client, server, application)
        })
        .await
        .unwrap();
        if accept {
            let a = a.unwrap();
            let b = b.unwrap();
            assert_eq!(a.inbound_discarded, u64::from(secured));
            assert_eq!(b.inbound_discarded, 0);
            if secured {
                assert!(a.max_inbound_wire_bytes > 1280);
                assert!(a.max_inbound_wire_bytes <= 1320);
                assert!(b.max_outbound_wire_bytes > 1280);
                assert!(b.max_outbound_wire_bytes <= 1320);
            }
            assert_eq!(
                received.len(),
                5000 + b"MESSAGE sip:ims.example SIP/2.0\r\nContent-Length: 5000\r\n\r\n".len()
            );
        } else {
            assert!(matches!(
                a,
                Err(crate::netstack_runtime::NetworkRunError::Application(_))
            ));
            // Closing the client endpoint can reach the peer before its stop
            // watcher; either explicit cancellation or channel EOF is valid.
            assert!(
                b.is_ok()
                    || matches!(
                        b,
                        Err(crate::netstack_runtime::NetworkRunError::TunnelClosed)
                    )
            );
            assert!(received.is_empty());
        }
    }
}
