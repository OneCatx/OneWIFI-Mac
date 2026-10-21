package imscore

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func rawSMSRequest(tpdu []byte) *sip.Request {
	req := sip.NewRequest(sip.MESSAGE, sip.Uri{Host: "ims.example.test"})
	req.AppendHeader(sip.NewHeader("Content-Type", imsSMSContentType))
	req.AppendHeader(sip.NewHeader("From", "<sip:smsc@ims.example.test>"))
	id := sip.CallIDHeader("network-transaction@example.test")
	req.AppendHeader(&id)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 73, MethodName: sip.MESSAGE})
	req.SetBody(append([]byte{1, 42, 0, 0, byte(len(tpdu))}, tpdu...))
	return req
}

func TestRawSMSPersistsEveryFragmentAndReportBeforeAck(t *testing.T) {
	fixtures := map[string][]byte{
		"deliver":     {0, 3, 0x81, 0x88, 0xf8, 0, 8, 0x62, 0x90, 0x60, 0, 0, 0, 0, 2, 0, 0x41},
		"fragment":    {0x40, 3, 0x81, 0x88, 0xf8, 0, 8, 0x62, 0x90, 0x60, 0, 0, 0, 0, 8, 5, 0, 3, 9, 2, 1, 0, 0x41},
		"report":      {2, 1, 4, 0x91, 0x44, 0x77, 0x62, 0x80, 0x62, 0x61, 0x21, 0, 0, 0x62, 0x80, 0x62, 0x61, 0x21, 0, 0, 0},
		"undecodable": {0},
	}
	for name, tpdu := range fixtures {
		for _, fail := range []bool{false, true} {
			t.Run(name+map[bool]string{true: "/disk-failure", false: "/durable"}[fail], func(t *testing.T) {
				req := rawSMSRequest(tpdu)
				original := append([]byte(nil), req.Body()...)
				persisted, acknowledged := false, false
				rt := &transportRuntime{}
				rt.cfg.IncomingSMS = func(string, string, time.Time) error {
					t.Fatal("raw delivery duplicated through legacy callback")
					return nil
				}
				rt.cfg.IncomingRawSMS = func(event IncomingSMSEvent) error {
					if !bytes.Equal(event.RPDU, original) || !bytes.Equal(event.TPDU, tpdu) || event.SIPCallID != "network-transaction@example.test" || event.SIPCSeq != 73 || event.RPReference != 42 {
						t.Fatalf("lost network transaction: %+v", event)
					}
					if name == "fragment" && (event.Concat == nil || event.Concat.Reference != 9 || event.Concat.Total != 2 || event.Concat.Sequence != 1) {
						t.Fatalf("fragment metadata=%+v", event.Concat)
					}
					if name == "report" && (!event.StatusReport || event.MessageReference == nil || *event.MessageReference != 1) {
						t.Fatal("report correlation lost")
					}
					if name == "undecodable" && (event.Decoded || event.DecodeError == "") {
						t.Fatal("undecodable raw event was not retained")
					}
					event.RPDU[0] = 255
					if fail {
						return errors.New("disk unavailable")
					}
					persisted = true
					return nil
				}
				rt.sendRPAck = func(string, byte) {
					if !persisted {
						t.Fatal("RP-ACK before durability")
					}
					acknowledged = true
				}
				rt.handleIncomingRequest(req)
				if acknowledged == fail {
					t.Fatalf("ack=%v failed=%v", acknowledged, fail)
				}
				if !bytes.Equal(req.Body(), original) {
					t.Fatal("raw callback received borrowed network buffer")
				}
			})
		}
	}
}
