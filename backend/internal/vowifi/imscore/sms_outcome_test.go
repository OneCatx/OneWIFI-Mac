package imscore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"proletarius.local/core/internal/vowifi/messaging"
)

func TestSMSSubmittedOutcomeIsNotRetried(t *testing.T) {
	for _, test := range []struct {
		name     string
		codes    []int // 0 means the response is lost after the write.
		unknown  bool
		accepted int
		failed   bool
	}{
		{"accepted", []int{202, 202}, false, 2, false},
		{"rejected before any acceptance", []int{403}, false, 0, true},
		{"partial acceptance", []int{202, 500}, true, 1, true},
		{"response lost", []int{0}, true, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, _ := runtimeTestPolicy(t)
			rt := &transportRuntime{policy: policy, tcpWriteCh: make(chan sipWriteTask), responses: make(map[string]map[uint64]chan *sip.Response)}
			service := &Service{cfg: Config{LocalIP: net.ParseIP("192.0.2.10"), PCSCFAddr: "192.0.2.20:5060", HomeDomain: "ims.example.test", PublicURI: "sip:fixture@ims.example.test"}, transportRuntime: rt}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			writes := make(chan error, 1)
			go func() {
				for _, code := range test.codes {
					var task sipWriteTask
					select {
					case task = <-rt.tcpWriteCh:
					case <-ctx.Done():
						writes <- ctx.Err()
						return
					}
					message, err := sip.ParseMessage(task.payload)
					if err != nil {
						writes <- err
						return
					}
					request, ok := message.(*sip.Request)
					if !ok {
						writes <- fmt.Errorf("not a SIP request")
						return
					}
					task.done <- nil
					if code == 0 {
						cancel()
						break
					}
					rt.publishResponse(sip.NewResponseFromRequest(request, code, "Fixture", nil))
				}
				writes <- nil
			}()
			outcome, err := service.sendSMSOnRegisteredTransport(ctx, "tel:+10000000000", "fixture", []messaging.SMSPart{{Body: []byte{1}}, {Body: []byte{2}}})
			if (err != nil) != test.failed {
				t.Fatalf("outcome=%+v error=%v", outcome, err)
			}
			var unknown *messaging.SendOutcomeUnknownError
			if errors.As(err, &unknown) != test.unknown {
				t.Fatalf("unexpected uncertainty: %v", err)
			}
			if unknown != nil && (unknown.AcceptedParts != test.accepted || unknown.TotalParts != 2) {
				t.Fatalf("parts=%+v", unknown)
			}
			if writeErr := <-writes; writeErr != nil {
				t.Fatal(writeErr)
			}
			if len(rt.responses) != 0 {
				t.Fatal("response subscriptions leaked")
			}
		})
	}
}
