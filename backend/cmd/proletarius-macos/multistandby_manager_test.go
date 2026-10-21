package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"proletarius.local/core/internal/vowifi/imscore"
	"strings"
	"sync"
	"testing"
	"time"
)

type multiTestTransport func(*http.Request) (*http.Response, error)

func (f multiTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func multiTestStore(handle func(*http.Request) int) *multiStoreClient {
	return &multiStoreClient{token: "fixture-internal", client: &http.Client{Transport: multiTestTransport(func(r *http.Request) (*http.Response, error) {
		code := handle(r)
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{"revision":1}`)), Header: make(http.Header)}, nil
	})}}
}
func TestMultiSendUnavailableProfileNeverFallsBack(t *testing.T) {
	a := &app{}
	m := a.multiStandby()
	called := false
	m.store = multiTestStore(func(*http.Request) int { called = true; return 200 })
	m.runtimes["online-other"] = &multiRuntime{desired: true}
	req := httptest.NewRequest("POST", "/api/multistandby/messages/send", strings.NewReader(`{"profile_id":"offline-selected","phone":"+12025550123","body":"fixture","client_request_id":"test-id"}`))
	w := httptest.NewRecorder()
	a.multiSend(w, req)
	if w.Code != 409 || called {
		t.Fatalf("code=%d queue_called=%t", w.Code, called)
	}
}
func TestMultiInboundIdentityAndConcatReferenceReuse(t *testing.T) {
	m := (&app{}).multiStandby()
	var writes []map[string]any
	m.store = multiTestStore(func(r *http.Request) int {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writes = append(writes, body)
		if r.Header.Get("X-Switch-Internal-Token") != "fixture-internal" {
			t.Fatal("missing private credential")
		}
		return 200
	})
	event := imscore.IncomingSMSEvent{RPDU: []byte{1, 2, 3}, TPDU: []byte{4, 5}, SIPCallID: "call-a", SIPCSeq: 1, Sender: "sender", Content: "first", Decoded: true, Timestamp: time.Unix(1700000000, 0), Concat: &imscore.SMSConcatInfo{Reference: 7, RefBits: 8, Total: 2, Sequence: 1}}
	p := multiProfile{ProfileID: "profile-A"}
	if err := m.inbound(context.Background(), p, event); err != nil {
		t.Fatal(err)
	}
	event.SIPCallID = "network-retry"
	event.SIPCSeq = 2
	if err := m.inbound(context.Background(), p, event); err != nil {
		t.Fatal(err)
	}
	event.Timestamp = event.Timestamp.Add(time.Minute)
	event.RPDU = []byte{1, 2, 4}
	if err := m.inbound(context.Background(), p, event); err != nil {
		t.Fatal(err)
	}
	p.ProfileID = "profile-B"
	if err := m.inbound(context.Background(), p, event); err != nil {
		t.Fatal(err)
	}
	if writes[0]["dedup_key"] != writes[1]["dedup_key"] {
		t.Fatal("SIP retry changed network dedup identity")
	}
	if writes[0]["message_id"] == writes[2]["message_id"] {
		t.Fatal("reused concat reference aliased old message")
	}
	if writes[2]["message_id"] == writes[3]["message_id"] {
		t.Fatal("profiles share message ownership")
	}
	if writes[0]["event_id"] == writes[1]["event_id"] {
		t.Fatal("separate receive transaction reused RPC event id")
	}
}
func TestMultiStopDrainsEvenWhenDesiredPersistenceFails(t *testing.T) {
	m := (&app{}).multiStandby()
	m.store = multiTestStore(func(*http.Request) int { return 503 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &multiRuntime{profile: multiProfile{ProfileID: "p"}, desired: true, stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	m.runtimes["p"] = r
	drained := make(chan struct{})
	go func() {
		select {
		case <-r.stop:
		case <-ctx.Done():
		}
		close(drained)
		close(r.done)
	}()
	if err := m.stop(context.Background(), []multiProfile{{ProfileID: "p"}}); err == nil {
		t.Fatal("persistence failure hidden")
	}
	select {
	case <-drained:
	default:
		t.Fatal("runtime still live after Stop")
	}
	if len(m.runtimes) != 0 {
		t.Fatal("runtime was not retired")
	}
}
func TestMultiShutdownPreservesDesiredIntent(t *testing.T) {
	m := (&app{}).multiStandby()
	var mu sync.Mutex
	paths := []string{}
	m.store = multiTestStore(func(r *http.Request) int { mu.Lock(); paths = append(paths, r.URL.Path); mu.Unlock(); return 200 })
	if err := m.stopWithDesired(context.Background(), []multiProfile{{ProfileID: "p"}}, true); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("shutdown changed persisted intention: %v", paths)
	}
}
func TestMultiOperationRejectsWrongCardAndUnknownProfile(t *testing.T) {
	a := &app{}
	m := a.multiStandby()
	m.profiles["p"] = multiProfile{ProfileID: "p", CardID: "card-A"}
	for _, body := range []string{`{"card_id":"card-B","all":true,"client_request_id":"r1"}`, `{"card_id":"card-A","profile_ids":["unknown"],"client_request_id":"r2"}`, `{"card_id":"card-A","profile_ids":["p"],"all":true,"client_request_id":"r3"}`} {
		w := httptest.NewRecorder()
		a.multiOperation(w, httptest.NewRequest("POST", "/api/multistandby/start", strings.NewReader(body)))
		if w.Code < 400 {
			t.Fatalf("accepted invalid selection: %s", body)
		}
	}
}

func TestMultiQueuedStartCannotOutliveShutdown(t *testing.T) {
	m := (&app{}).multiStandby()
	m.closed = true
	called := false
	m.store = multiTestStore(func(*http.Request) int { called = true; return 200 })
	if err := m.start(context.Background(), []multiProfile{{ProfileID: "p", CardID: "c"}}); err == nil {
		t.Fatal("start accepted after shutdown")
	}
	if called {
		t.Fatal("shutdown race reached physical owner service")
	}
}

func TestMultiUndecodedInboundSurfacesRetainedRecord(t *testing.T) {
	m := (&app{}).multiStandby()
	m.runtimes["p"] = &multiRuntime{}
	m.store = &multiStoreClient{token: "fixture", client: &http.Client{Transport: multiTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"state":"undecoded"}`)), Header: make(http.Header)}, nil
	})}}
	if err := m.inbound(context.Background(), multiProfile{ProfileID: "p"}, imscore.IncomingSMSEvent{RPDU: []byte{1}, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if m.runtimes["p"].warning == "" {
		t.Fatal("retained undecoded record hidden from UI status")
	}
}

func TestMultiShutdownReleasesOrphanOwnerWithoutProfiles(t *testing.T) {
	a := &app{}
	m := a.multiStandby()
	m.ownerToken = multiToken()
	released := false
	m.store = multiTestStore(func(r *http.Request) int {
		if strings.HasSuffix(r.URL.Path, "/reader-reservation/release") {
			released = true
		}
		return 200
	})
	if err := a.stopMultiStandbyForShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !released || m.ownerToken != "" {
		t.Fatal("orphan owner survived empty-profile shutdown")
	}
}
