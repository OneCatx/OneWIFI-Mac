package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damonto/uicc-go/apdu"
	"github.com/damonto/uicc-go/usim/simfile"
)

func TestListModuleEFDirApplicationsSkipsOnlyInaccessibleRecords(t *testing.T) {
	const recordCount = 2
	wantAID := []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02, 0xFF}
	record, err := (simfile.EFDirRecord{AID: wantAID, Label: "USIM"}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var reads []uint16
	applications, err := listModuleEFDirApplications(context.Background(), recordCount, uint16(len(record)),
		func(_ context.Context, recordID uint16) ([]byte, error) {
			reads = append(reads, recordID)
			if recordID == 2 {
				return nil, apdu.StatusError{SW: 0x6986}
			}
			return record, nil
		})
	if err != nil {
		t.Fatalf("listModuleEFDirApplications() error = %v", err)
	}
	if len(reads) != recordCount || reads[0] != 1 || reads[1] != 2 {
		t.Fatalf("records read = %v, want [1 2]", reads)
	}
	if len(applications) != 1 || string(applications[0].AID) != string(wantAID) || applications[0].Label != "USIM" {
		t.Fatalf("applications = %+v, want one valid USIM AID", applications)
	}
}

func TestListModuleEFDirApplicationsRequiresAnAccessibleAID(t *testing.T) {
	_, err := listModuleEFDirApplications(context.Background(), 2, 12,
		func(context.Context, uint16) ([]byte, error) {
			return nil, apdu.StatusError{SW: 0x6986}
		})
	if err == nil || err.Error() != "reading EF_DIR: no accessible application AID" {
		t.Fatalf("listModuleEFDirApplications() error = %v, want no accessible AID", err)
	}
	_, err = listModuleEFDirApplications(context.Background(), 1, 12,
		func(context.Context, uint16) ([]byte, error) { return nil, errors.New("transport failed") })
	if err == nil || err.Error() != "reading EF_DIR record 1: transport failed" {
		t.Fatalf("listModuleEFDirApplications() error = %v, want transport error", err)
	}
}

func TestModuleSIMProfileChecksShareAKAChannelLock(t *testing.T) {
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	command := func(command string, _ time.Duration) (string, error) {
		current := inFlight.Add(1)
		for {
			previous := maxInFlight.Load()
			if current <= previous || maxInFlight.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		switch command {
		case "AT+QCCID":
			return "+QCCID: 8988212345678901234\r\nOK", nil
		case "AT+CIMI":
			return "234301234567890\r\nOK", nil
		default:
			t.Fatalf("unexpected AT command %q", command)
			return "", nil
		}
	}
	sim := &moduleVoWiFiSIM{
		info:      &usimInfo{ICCID: "8988212345678901234", IMSI: "234301234567890"},
		atCommand: command,
	}

	// AKA holds this same mutex across profile validation and the APDU
	// transaction. Concurrent status/call/SMS validation must wait behind it.
	sim.mu.Lock()
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		finished <- sim.validateProfile(context.Background())
	}()
	<-started
	select {
	case err := <-finished:
		sim.mu.Unlock()
		t.Fatalf("profile check escaped the active AKA lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	sim.mu.Unlock()
	if err := <-finished; err != nil {
		t.Fatalf("profile validation failed: %v", err)
	}

	// Two public validations must also serialize their QCCID/CIMI pairs.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sim.validateProfile(context.Background()); err != nil {
				t.Errorf("profile validation failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("SIM AT/APDU commands overlapped: max in-flight=%d, want 1", got)
	}
}

func TestModuleSIMTransactionsSerializeAcrossSessions(t *testing.T) {
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	command := func(command string, _ time.Duration) (string, error) {
		current := inFlight.Add(1)
		for {
			previous := maxInFlight.Load()
			if current <= previous || maxInFlight.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		switch command {
		case "AT+QCCID":
			return "+QCCID: 8988212345678901234\r\nOK", nil
		case "AT+CIMI":
			return "234301234567890\r\nOK", nil
		default:
			t.Fatalf("unexpected AT command %q", command)
			return "", nil
		}
	}
	sharedApp := &app{}
	makeSIM := func() *moduleVoWiFiSIM {
		return &moduleVoWiFiSIM{
			app:       sharedApp,
			info:      &usimInfo{ICCID: "8988212345678901234", IMSI: "234301234567890"},
			atCommand: command,
		}
	}
	first, second := makeSIM(), makeSIM()
	var wg sync.WaitGroup
	for _, sim := range []*moduleVoWiFiSIM{first, second} {
		wg.Add(1)
		go func(sim *moduleVoWiFiSIM) {
			defer wg.Done()
			if err := sim.validateProfile(context.Background()); err != nil {
				t.Errorf("profile validation failed: %v", err)
			}
		}(sim)
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("separate SIM sessions overlapped on one module AT channel: max in-flight=%d", got)
	}
}

func TestResolveModuleServiceCenterUsesOnlyActiveProfileSources(t *testing.T) {
	tests := []struct {
		name       string
		address    string
		target     string
		atResponse string
		wantAddr   string
		wantTarget string
	}{
		{
			name:    "preserve card PSI and address",
			address: "+447700900123", target: "sip:smsc@example.test",
			atResponse: `+CSCA: "+441234567890",145`,
			wantAddr:   "+447700900123", wantTarget: "sip:smsc@example.test",
		},
		{
			name:       "AT SMSC fallback when card address is absent",
			atResponse: "+CSCA: \"+441234567890\",145\r\nOK",
			wantAddr:   "+441234567890", wantTarget: "tel:+441234567890",
		},
		{
			name:       "do not invent a route from invalid AT response",
			atResponse: "+CSCA: \"smsc.example.test\",145\r\nOK",
		},
		{
			name: "do not reuse previous profile values",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, target := resolveModuleServiceCenter(test.address, test.target, test.atResponse)
			if address != test.wantAddr || target != test.wantTarget {
				t.Fatalf("resolveModuleServiceCenter()=(%q,%q), want (%q,%q)", address, target, test.wantAddr, test.wantTarget)
			}
		})
	}
}

func TestModuleCSCAParserRejectsMalformedValues(t *testing.T) {
	for _, response := range []string{
		`+CSCA: "",145`,
		`+CSCA: "00000000000",145`,
		`+CSCA: "+44ABC1234567",145`,
		`+CSCA: +441234567890,145`,
	} {
		if got := parseModuleCSCA(response); got != "" {
			t.Fatalf("parseModuleCSCA(%q)=%q, want empty", response, got)
		}
	}
}
