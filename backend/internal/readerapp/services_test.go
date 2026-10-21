package readerapp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

type fakeTelephony struct{ dial, sms int }

func (f *fakeTelephony) Dial(context.Context, string) error            { f.dial++; return nil }
func (f *fakeTelephony) Hangup(context.Context) error                  { return nil }
func (f *fakeTelephony) SendSMS(context.Context, string, string) error { f.sms++; return nil }

func TestTelephonyServicesDoNotNeedReader(t *testing.T) {
	tx := &fakeTelephony{}
	calls, err := NewCallService(tx)
	if err != nil {
		t.Fatal(err)
	}
	sms, err := NewSmsService(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := calls.Dial(context.Background(), "888"); err != nil {
		t.Fatal(err)
	}
	if err := sms.Send(context.Background(), "888", "BAL"); err != nil {
		t.Fatal(err)
	}
	if tx.dial != 1 || tx.sms != 1 {
		t.Fatalf("dial=%d sms=%d", tx.dial, tx.sms)
	}
	if _, err := NewCallService(nil); err == nil {
		t.Fatal("nil telephony accepted")
	}
}

func TestDownloadStateMachine(t *testing.T) {
	svc := NewCardDownloadServiceWithPlan(DownloadPlan{AutoActivate: true, RegisterIMS: true})
	for _, state := range []DownloadState{Authenticating, Downloading, Installing, Activating, Registering, Completed} {
		if err := svc.Advance(state, ""); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	if err := svc.Advance(Failed, "late"); err == nil {
		t.Fatal("terminal state mutated")
	}
	if svc.Snapshot().State != Completed {
		t.Fatal("completion lost")
	}

	svc = NewCardDownloadService()
	if err := svc.Advance(Completed, ""); err == nil {
		t.Fatal("completed before installation")
	}
	if err := svc.Advance(Failed, ""); err == nil {
		t.Fatal("failure without error code")
	}
	if err := svc.Advance(Failed, "network_timeout"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Advance(Preparing, ""); err == nil {
		t.Fatal("failed operation restarted")
	}
}

func TestInstallOnlyDownloadDoesNotClaimActivation(t *testing.T) {
	svc := NewCardDownloadService()
	for _, state := range []DownloadState{Authenticating, Downloading, Installing, Completed} {
		if err := svc.Advance(state, ""); err != nil {
			t.Fatal(err)
		}
	}
	if svc.Snapshot().State != Completed {
		t.Fatal("install-only download did not complete")
	}
}

func TestOperationLogFields(t *testing.T) {
	var buffer bytes.Buffer
	if err := NewLogger(&buffer).Write("bluetooth", "scan", "ready", "ok", ""); err != nil {
		t.Fatal(err)
	}
	var event OperationLog
	if err := json.Unmarshal(buffer.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Timestamp.IsZero() || event.Module != "bluetooth" || event.Action != "scan" || event.State != "ready" || event.Result != "ok" {
		t.Fatalf("event=%+v", event)
	}
}
