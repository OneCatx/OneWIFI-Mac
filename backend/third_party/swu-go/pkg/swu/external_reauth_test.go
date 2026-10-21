package swu

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/1239t/swu-go/pkg/ikev2"
	"go.uber.org/zap"
)

func TestExternalReauthOwnerControlsWholeSessionRetry(t *testing.T) {
	for _, external := range []bool{false, true} {
		s := NewSession(&Config{ExternalReauthOwner: external}, zap.NewNop())
		attempts := 0
		err := s.connectLoop(context.Background(), func() error {
			attempts++
			if attempts == 1 {
				return fmt.Errorf("test: %w", ErrReauth)
			}
			return nil
		})
		defer s.cancel()
		if external {
			if attempts != 1 || !errors.Is(err, ErrReauth) {
				t.Fatalf("pool did not receive reauth: attempts=%d err=%v", attempts, err)
			}
			if s.ctx.Err() == nil {
				t.Fatal("external generation workers were not canceled")
			}
			select {
			case <-s.done:
			default:
				t.Fatal("external generation completion not published")
			}
		} else if attempts != 2 || err != nil {
			t.Fatalf("legacy retry changed: attempts=%d err=%v", attempts, err)
		}
	}
}

func TestInitialContactCanBeSuppressedForParallelPool(t *testing.T) {
	for _, skip := range []bool{false, true} {
		s := NewSession(&Config{SIM: &effectiveConfigSIM{}, APN: "ims", SkipInitialContact: skip}, zap.NewNop())
		payloads, err := s.buildIKEAuthInitPayloads()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, payload := range payloads {
			if notify, ok := payload.(*ikev2.EncryptedPayloadNotify); ok && notify.NotifyType == ikev2.INITIAL_CONTACT {
				found = true
			}
		}
		if found == skip {
			t.Fatalf("skip=%v found=%v", skip, found)
		}
	}
}
