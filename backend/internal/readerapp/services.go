// Package readerapp defines reader-facing capabilities without coupling
// telephony to USB PC/SC or Bluetooth GATT implementations.
package readerapp

import (
	"context"
	"errors"
	"time"

	"proletarius.local/core/internal/esim"
)

type ReaderID string

type CardInfo struct {
	ICCID       string
	IMSI        string
	EID         string
	PhoneNumber string
	Present     bool
}

type Profile struct {
	ICCID   string
	Name    string
	Enabled bool
}

type ProfileSwitchResult struct {
	TargetICCID    string
	Phase          string
	SwitchAccepted bool
}

// CardService describes card operations independently of the transport.
// Implementations must serialize changes to the active eUICC application.
type CardService interface {
	Detect(context.Context, ReaderID) (bool, error)
	Info(context.Context, ReaderID) (CardInfo, error)
	Overview(context.Context, ReaderID) (*esim.EsimOverview, error)
	RefreshOverview(context.Context, ReaderID) error
	Profiles(context.Context, ReaderID) ([]Profile, error)
	ActiveProfile(context.Context, ReaderID) (Profile, error)
	SwitchProfile(context.Context, ReaderID, string, string) (ProfileSwitchResult, error)
}

type SIMState string
type RegistrationState string

type NetworkSnapshot struct {
	SIM      SIMState
	Cellular RegistrationState
	VoLTE    RegistrationState
	VoWiFi   RegistrationState
}

type IMSSnapshot struct {
	Registration RegistrationState
	UpdatedAt    time.Time
}

type NetworkService interface {
	Snapshot(context.Context) (NetworkSnapshot, error)
}
type IMSService interface {
	Snapshot(context.Context) (IMSSnapshot, error)
}

// Telephony is the only capability CallService and SmsService may depend on.
// A reader is one possible identity provider, never a direct call/SMS dependency.
type Telephony interface {
	Dial(context.Context, string) error
	Hangup(context.Context) error
	SendSMS(context.Context, string, string) error
}

type CallService struct{ telephony Telephony }
type SmsService struct{ telephony Telephony }

func NewCallService(telephony Telephony) (*CallService, error) {
	if telephony == nil {
		return nil, errors.New("telephony capability unavailable")
	}
	return &CallService{telephony: telephony}, nil
}
func NewSmsService(telephony Telephony) (*SmsService, error) {
	if telephony == nil {
		return nil, errors.New("telephony capability unavailable")
	}
	return &SmsService{telephony: telephony}, nil
}
func (s *CallService) Dial(ctx context.Context, number string) error {
	return s.telephony.Dial(ctx, number)
}
func (s *CallService) Hangup(ctx context.Context) error { return s.telephony.Hangup(ctx) }
func (s *SmsService) Send(ctx context.Context, number, body string) error {
	return s.telephony.SendSMS(ctx, number, body)
}
