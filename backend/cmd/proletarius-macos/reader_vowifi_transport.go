package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/1239t/swu-go/pkg/ipsec"
	"github.com/1239t/swu-go/pkg/swu"

	"proletarius.local/core/internal/vowifi/socks5"
)

var readerSelectTransport = selectRuntimeReaderTransport

// readerSWUTransportSelection keeps Rust's immutable decision metadata with
// the Go compatibility transport. In particular, proxySource is authoritative
// for deciding whether Go may touch Surge; a loopback port alone never proves
// that Surge owns it.
type readerSWUTransportSelection struct {
	factory     func(string, string) (swu.Transport, error)
	mode        string
	proxySource string
	proxyError  string
	errorCode   string
}

// selectReaderSWUTransport prefers the configured proxy paths, then falls
// back to a normal UDP socket when the proxy is unavailable. A proxy is
// intentionally optional because some Wi-Fi/hotspot networks route ePDG
// directly. Fake-IP answers are never used for direct sockets.
func selectReaderSWUTransport(ctx context.Context, proxyAddr, epdgHost string) (func(string, string) (swu.Transport, error), string, error) {
	selection, err := selectReaderSWUTransportWithSelection(ctx, proxyAddr, epdgHost)
	if err != nil {
		return nil, "", err
	}
	return selection.factory, selection.mode, nil
}

func selectReaderSWUTransportWithSelection(ctx context.Context, proxyAddr, epdgHost string) (readerSWUTransportSelection, error) {
	proxyAddr = strings.TrimSpace(proxyAddr)
	selection, err := readerSelectTransport(ctx, proxyAddr, strings.TrimSpace(epdgHost))
	if err != nil {
		// A rejected Rust decision still contains safe, structured metadata.
		// Preserve it for startReservedVoWiFi so it can make one deliberately
		// narrow Surge Enhanced Mode recovery decision. All ordinary errors
		// remain opaque failures and cannot trigger another application's CLI.
		var rejected *nativeTransportSelectionError
		if errors.As(err, &rejected) {
			return readerSWUTransportSelection{
				mode:        rejected.selection.Mode,
				proxySource: rejected.selection.ProxySource,
				proxyError:  rejected.selection.ProxyError,
				errorCode:   rejected.selection.ErrorCode,
			}, err
		}
		return readerSWUTransportSelection{}, err
	}
	switch selection.Mode {
	case "socks5-udp-domain":
		// Rust is the sole automatic macOS/Surge SOCKS discoverer. Use only
		// the endpoint it resolved and verified with UDP ASSOCIATE; never
		// re-resolve an ePDG name or guess an HTTP/PAC proxy here.
		proxyAddr = strings.TrimSpace(selection.ProxyEndpoint)
		if proxyAddr == "" {
			return readerSWUTransportSelection{}, fmt.Errorf("Rust 选择了 SOCKS5 传输，但缺少已验证代理端点")
		}
		if err := validateReaderVoWiFiProxy(proxyAddr); err != nil {
			return readerSWUTransportSelection{}, fmt.Errorf("Rust 返回的 SOCKS5 代理端点无效: %w", err)
		}
		if selection.ProxySource == "" {
			return readerSWUTransportSelection{}, fmt.Errorf("Rust 选择了 SOCKS5 传输，但缺少已验证代理来源")
		}
		return readerSWUTransportSelection{
			factory:     newReaderSWUProxyTransportFactory(proxyAddr),
			mode:        selection.Mode,
			proxySource: selection.ProxySource,
			proxyError:  selection.ProxyError,
			errorCode:   selection.ErrorCode,
		}, nil
	case "enhanced-mode-fake-ip", "enhanced-mode-utun", "direct":
		remoteIP := net.ParseIP(selection.Address)
		if remoteIP == nil {
			return readerSWUTransportSelection{}, fmt.Errorf("Rust 返回了无效 ePDG 地址 %q", selection.Address)
		}
		if selection.Mode == "enhanced-mode-utun" {
			if !strings.HasPrefix(selection.Interface, "utun") {
				return readerSWUTransportSelection{}, fmt.Errorf("Rust 返回了无效增强模式接口 %q", selection.Interface)
			}
		}
		return readerSWUTransportSelection{
			factory:     newReaderSWUTransparentTransportFactory(remoteIP),
			mode:        selection.Mode,
			proxySource: selection.ProxySource,
			proxyError:  selection.ProxyError,
			errorCode:   selection.ErrorCode,
		}, nil
	default:
		return readerSWUTransportSelection{}, fmt.Errorf("Rust 返回了未知 ePDG 传输模式 %q", selection.Mode)
	}
}

// newReaderSWUTransparentTransportFactory sends packets to the verified
// system-resolved address without binding a local IP or interface. Surge and
// other Network Extension proxies must see an ordinary system-routed socket in
// order to transparently capture IKE/NAT-T; IP_BOUND_IF on their utun bypasses
// that interception and leaves IKE_INIT unanswered.
func newReaderSWUTransparentTransportFactory(remoteIP net.IP) func(string, string) (swu.Transport, error) {
	verifiedIP := append(net.IP(nil), remoteIP...)
	return func(_, remote string) (swu.Transport, error) {
		_, portText, err := net.SplitHostPort(remote)
		if err != nil {
			return nil, fmt.Errorf("解析 ePDG 地址 %q: %w", remote, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("ePDG 端口无效: %q", portText)
		}
		manager, err := ipsec.NewSocketManager("", net.JoinHostPort(verifiedIP.String(), strconv.Itoa(port)), "")
		if err != nil {
			return nil, err
		}
		return manager, nil
	}
}

// newReaderSWUProxyTransportFactory creates the optional explicit SOCKS path.
// The ePDG hostname stays a hostname in SOCKS ATYP=DOMAIN packets; this code
// never resolves it independently or substitutes a supposedly "real" IP.
func newReaderSWUProxyTransportFactory(proxyAddr string) func(string, string) (swu.Transport, error) {
	return func(_, remote string) (swu.Transport, error) {
		host, portText, err := net.SplitHostPort(remote)
		if err != nil {
			return nil, fmt.Errorf("解析 ePDG 地址 %q: %w", remote, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("ePDG 端口无效: %q", portText)
		}
		relay, err := socks5.NewUDPRelay(proxyAddr)
		if err != nil {
			return nil, err
		}
		return &readerSWUProxyTransport{
			relay:      relay,
			remoteHost: host,
			remoteIP:   net.ParseIP(host),
			remotePort: port,
			ikePackets: make(chan []byte, 100),
			espPackets: make(chan []byte, 1000),
			netEvents:  make(chan ipsec.NetEvent, 4),
			closed:     make(chan struct{}),
		}, nil
	}
}

type readerSWUProxyTransport struct {
	relay *socks5.UDPRelay

	remoteMu   sync.RWMutex
	remoteHost string
	remoteIP   net.IP
	remotePort int

	ikePackets chan []byte
	espPackets chan []byte
	netEvents  chan ipsec.NetEvent
	closed     chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

func (t *readerSWUProxyTransport) Start() {
	t.startOnce.Do(func() {
		t.wg.Add(1)
		go t.readLoop()
	})
}

func (t *readerSWUProxyTransport) Stop() {
	t.stopOnce.Do(func() {
		close(t.closed)
		_ = t.relay.Close()
		t.wg.Wait()
		close(t.ikePackets)
		close(t.espPackets)
		close(t.netEvents)
	})
}

func (t *readerSWUProxyTransport) IKEPackets() <-chan []byte { return t.ikePackets }
func (t *readerSWUProxyTransport) ESPPackets() <-chan []byte { return t.espPackets }
func (t *readerSWUProxyTransport) NetEventsChan() <-chan ipsec.NetEvent {
	return t.netEvents
}

func (t *readerSWUProxyTransport) SendIKE(payload []byte) error {
	if t.RemotePort() == 4500 {
		packet := make([]byte, 4+len(payload))
		copy(packet[4:], payload)
		return t.send(packet)
	}
	return t.send(payload)
}

func (t *readerSWUProxyTransport) SendESP(payload []byte) error {
	// RFC 3948 ESP-in-UDP has no Non-ESP marker.
	return t.send(payload)
}

func (t *readerSWUProxyTransport) SendNATKeepalive() error {
	return t.send([]byte{0xff})
}

func (t *readerSWUProxyTransport) send(payload []byte) error {
	t.remoteMu.RLock()
	host, ip, port := t.remoteHost, append(net.IP(nil), t.remoteIP...), t.remotePort
	t.remoteMu.RUnlock()
	if ip != nil {
		return t.relay.SendTo(payload, ip, port)
	}
	return t.relay.SendToDomain(payload, host, port)
}

func (t *readerSWUProxyTransport) SetRemotePort(port int) {
	if port < 1 || port > 65535 {
		return
	}
	t.remoteMu.Lock()
	t.remotePort = port
	t.remoteMu.Unlock()
}

func (t *readerSWUProxyTransport) LocalPort() uint16 { return 0 }
func (t *readerSWUProxyTransport) LocalIP() net.IP   { return nil }
func (t *readerSWUProxyTransport) RemoteIP() net.IP {
	t.remoteMu.RLock()
	defer t.remoteMu.RUnlock()
	return append(net.IP(nil), t.remoteIP...)
}
func (t *readerSWUProxyTransport) RemotePort() int {
	t.remoteMu.RLock()
	defer t.remoteMu.RUnlock()
	return t.remotePort
}
func (t *readerSWUProxyTransport) LocalAddrString() string { return "socks5://dynamic" }
func (t *readerSWUProxyTransport) RemoteAddrString() string {
	t.remoteMu.RLock()
	defer t.remoteMu.RUnlock()
	return net.JoinHostPort(t.remoteHost, strconv.Itoa(t.remotePort))
}

func (t *readerSWUProxyTransport) readLoop() {
	defer t.wg.Done()
	buf := make([]byte, 65535)
	for {
		payload, _, sourcePort, err := t.relay.ReadFrom(buf)
		if err != nil {
			select {
			case <-t.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			t.reportNetworkDown(fmt.Errorf("SOCKS5 UDP relay read: %w", err))
			return
		}
		if sourcePort > 0 && sourcePort != t.RemotePort() {
			t.SetRemotePort(sourcePort)
		}
		if len(payload) == 1 && payload[0] == 0xff {
			continue
		}
		if ike, ok := readerIKEPayload(payload); ok {
			select {
			case t.ikePackets <- ike:
			case <-t.closed:
				return
			}
			continue
		}
		if len(payload) == 0 {
			continue
		}
		esp := append([]byte(nil), payload...)
		select {
		case t.espPackets <- esp:
		case <-t.closed:
			return
		}
	}
}

func (t *readerSWUProxyTransport) reportNetworkDown(err error) {
	if err == nil {
		return
	}
	select {
	case t.netEvents <- ipsec.NetEvent{Type: ipsec.EventNetworkDown, Reason: err.Error()}:
	case <-t.closed:
	default:
	}
}

func readerIKEPayload(packet []byte) ([]byte, bool) {
	if len(packet) >= 4 && binary.BigEndian.Uint32(packet[:4]) == 0 {
		packet = packet[4:]
	}
	if !looksLikeReaderIKE(packet) {
		return nil, false
	}
	return append([]byte(nil), packet...), true
}

func looksLikeReaderIKE(packet []byte) bool {
	if len(packet) < 28 || packet[17]>>4 != 2 {
		return false
	}
	exchange := packet[18]
	if exchange < 34 || exchange > 37 {
		return false
	}
	declared := int(binary.BigEndian.Uint32(packet[24:28]))
	return declared >= 28 && declared <= len(packet)
}

var _ swu.Transport = (*readerSWUProxyTransport)(nil)
