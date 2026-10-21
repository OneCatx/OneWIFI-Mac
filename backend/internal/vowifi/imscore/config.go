package imscore

import (
	"context"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
	"proletarius.local/core/internal/vowifi/messaging"
	"proletarius.local/core/internal/vowifi/policy"
	"proletarius.local/core/internal/vowifi/sim"
	"proletarius.local/core/internal/vowifi/voiceclient"
)

// Config configures the RE-based imscore IMS register + messaging service.
type Config struct {
	// SMSOnly disables voice capabilities, dialog handling and media allocation.
	// The zero value preserves the established voice-capable session.
	SMSOnly  bool
	DeviceID string
	TraceID  string

	LocalIP   net.IP
	Dataplane voiceclient.PacketDataplane
	PCSCFAddr string
	// TransportPCSCFAddr overrides the TCP destination for REGISTER when the
	// logical registrar (PCSCFAddr) is the UE inner IPv6 and userspace netstack
	// cannot hairpin to itself.
	TransportPCSCFAddr string
	// RegistrarCandidates is the ordered IKE/ePDG P-CSCF list used for initial
	// REGISTER probing when the first node returns a location/forbidden reject.
	RegistrarCandidates []string

	Realm      string
	PrivateID  string
	PublicURI  string
	HomeDomain string
	// CallPhoneContext applies only to local/national dial strings.
	CallPhoneContext string
	IMSI             string

	AKA sim.AKAProvider

	Template policy.IMSRegisterTemplate

	MCC    string
	MNC    string
	CellID string

	SIPInstanceURN string
	UserAgent      string
	// LocalPort is the preferred source port for the first REGISTER attempt.
	// Later registrar attempts use an ephemeral port so a failed TCP connection
	// cannot leave the configured port unusable in TIME_WAIT.
	LocalPort int
	// Transport is the resolved SIP transport (currently TCP for the reader
	// userspace dataplane).
	Transport string
	// TCPKeepaliveSeconds and OptionsPingIntervalSeconds are retained in the
	// internal runtime configuration for the dialer/lifecycle layers.
	TCPKeepaliveSeconds        int
	OptionsPingIntervalSeconds int
	// SecurityVerify is populated only after the authenticated REGISTER. It is
	// echoed on protected in-dialog-independent requests such as OPTIONS.
	SecurityVerify string

	RegisterExpirySeconds int

	DeliveryStore      messaging.DeliveryStore
	DecodeIncomingRPDU func([]byte) (byte, string, []byte, error)
	DecodeIncomingSMS  func([]byte) (InboundSMS, bool, error)
	// DTMFEvent validates a keypad digit before RFC 4733 control packets are
	// emitted. It is never called from the PCM/RTP audio hot path.
	DTMFEvent func(context.Context, string) (byte, error)
	// ValidateRegisterResponse verifies SIP framing and transaction identity
	// through the Rust protocol gate before REGISTER state changes.
	ValidateRegisterResponse func(context.Context, []byte, string, uint32) error
	// IncomingSMS receives decoded IMS SMS-DELIVER messages after the SIP
	// transaction has been acknowledged.
	// IncomingRawSMS replaces IncomingSMS when present. It must durably store
	// the event before returning nil; failure withholds the network RP-ACK.
	IncomingRawSMS     func(IncomingSMSEvent) error
	IncomingSMS        func(sender, content string, at time.Time) error
	IncomingSIPRequest func(*sip.Request)
	// IncomingSIPHandler owns dialog-forming and in-dialog requests. The
	// responder always writes to the same secured IMS connection.
	IncomingSIPHandler func(*sip.Request, SIPResponder)
	// IncomingRPAck is installed before the Port-C reader starts so inbound
	// SMS acknowledgements never race a post-start callback assignment.
	IncomingRPAck func(target string, reference byte)
}

// InboundSMS is a decoded SMS-DELIVER or SMS-STATUS-REPORT TPDU.
// HasTimestamp distinguishes a decoded service-centre timestamp from zero time.
type InboundSMS struct {
	Sender         string
	Content        string
	Timestamp      time.Time
	HasTimestamp   bool
	StatusReport   bool
	DeliveryStatus string
	StatusCode     byte
}

// SIPResponder sends a fully-formed UAS response through the serialized
// port-c writer. It is safe to retain for an asynchronous answer/reject action.
type SIPResponder func(*sip.Response) error

// IncomingSMSEvent preserves one network delivery transaction, including each
// concatenated fragment and delivery report. RPDU and TPDU are owned copies.
// SIP identity plus RPDU is suitable input for a profile-scoped dedup key; text
// alone is not, because two legitimate messages can have identical contents.
type IncomingSMSEvent struct {
	RPDU, TPDU                             []byte
	SIPCallID                              string
	SIPCSeq                                uint32
	RPReference                            byte
	Originator, Sender, Recipient, Content string
	Timestamp                              time.Time
	HasTimestamp, Decoded, StatusReport    bool
	DeliveryStatus                         string
	StatusCode                             byte
	MessageReference                       *int
	Concat                                 *SMSConcatInfo
	DecodeError                            string
}

type SMSConcatInfo struct {
	Reference, RefBits, Total, Sequence int
}
