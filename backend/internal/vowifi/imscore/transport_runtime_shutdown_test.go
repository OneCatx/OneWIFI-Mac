package imscore

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

type portSReadTrackingConn struct {
	net.Conn
	reads  chan struct{}
	closes atomic.Int32
}

func (c *portSReadTrackingConn) Read(p []byte) (int, error) {
	c.reads <- struct{}{}
	return c.Conn.Read(p)
}

func (c *portSReadTrackingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func startRuntimeWithOpenPortSPush(t *testing.T) (*transportRuntime, net.Conn, *portSReadTrackingConn, <-chan *sip.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listener := newQueuedPortSListener()
	local, peer := net.Pipe()
	conn := &portSReadTrackingConn{Conn: local, reads: make(chan struct{}, 8)}
	rt := &transportRuntime{
		cancel: cancel, inboundSIPCh: make(chan []byte, 8),
		responses: make(map[string]map[uint64]chan *sip.Response),
	}
	rt.healthy.Store(true)
	responses, unsubscribe, err := rt.subscribeResponses("shutdown-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = peer.Close() // also unblocks the old implementation on test failure
		rt.Close()
		unsubscribe()
	})
	rt.wg.Add(1)
	go rt.runPortSListener(ctx, nil, listener)
	listener.accepted <- conn
	select {
	case <-conn.reads:
	case <-time.After(time.Second):
		t.Fatal("accepted Port-S connection did not begin reading")
	}
	return rt, peer, conn, responses
}

func sendShutdownTestSIP(t *testing.T, peer net.Conn, conn *portSReadTrackingConn, responses <-chan *sip.Response) {
	t.Helper()
	if err := peer.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := peer.Write([]byte("SIP/2.0 200 OK\r\nVia: SIP/2.0/TCP 192.0.2.20:5060;branch=z9hG4bK-shutdown\r\nFrom: <sip:test@ims.example.test>;tag=source\r\nTo: <sip:test@ims.example.test>;tag=target\r\nCall-ID: shutdown-test\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.StatusCode != 200 {
			t.Fatalf("unexpected SIP response: %d", response.StatusCode)
		}
	case <-time.After(time.Second):
		t.Fatal("Port-S response was not delivered")
	}
	select {
	case <-conn.reads:
	case <-time.After(time.Second):
		t.Fatal("Port-S connection did not return to its open read")
	}
}

func TestTransportRuntimeCloseCancelsActivePortSReadWithoutStoppingOtherRuntime(t *testing.T) {
	first, firstPeer, firstConn, firstResponses := startRuntimeWithOpenPortSPush(t)
	second, secondPeer, secondConn, secondResponses := startRuntimeWithOpenPortSPush(t)
	sendShutdownTestSIP(t, firstPeer, firstConn, firstResponses)
	sendShutdownTestSIP(t, secondPeer, secondConn, secondResponses)
	closed := make(chan struct{})
	go func() { first.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("runtime Close blocked on an accepted Port-S connection")
	}
	if firstConn.closes.Load() != 1 {
		t.Fatalf("stopped Port-S connection closed %d times", firstConn.closes.Load())
	}
	if !second.Healthy() || secondConn.closes.Load() != 0 {
		t.Fatal("closing one runtime changed the other runtime")
	}
	sendShutdownTestSIP(t, secondPeer, secondConn, secondResponses)
}

func TestPortSNormalEOFCompletesBeforeRuntimeCancellation(t *testing.T) {
	rt, peer, conn, responses := startRuntimeWithOpenPortSPush(t)
	sendShutdownTestSIP(t, peer, conn, responses)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for conn.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if conn.closes.Load() != 1 {
		t.Fatal("normal EOF did not release the owned connection")
	}
	if !rt.Healthy() {
		t.Fatal("normal Port-S EOF incorrectly failed the runtime")
	}
	rt.Close()
	if conn.closes.Load() != 1 {
		t.Fatal("later cancellation closed an already released connection again")
	}
}
