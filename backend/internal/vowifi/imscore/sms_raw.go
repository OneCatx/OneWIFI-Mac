package imscore

import (
	"strings"
	"time"

	"github.com/1239t/swu-go/pkg/logger"
	"github.com/emiago/sipgo/sip"
	"proletarius.local/core/internal/device"
	"proletarius.local/core/pkg/smscodec"
)

func (rt *transportRuntime) handleIncomingRawSMS(req *sip.Request, reference byte, originator string, tpdu []byte) {
	event := IncomingSMSEvent{
		RPDU: append([]byte(nil), req.Body()...), TPDU: append([]byte(nil), tpdu...),
		RPReference: reference, Originator: originator, Timestamp: time.Now(),
	}
	if header := req.CallID(); header != nil {
		event.SIPCallID = header.Value()
	}
	if header := req.CSeq(); header != nil {
		event.SIPCSeq = header.SeqNo
	}
	if len(tpdu) > 0 {
		event.StatusReport = tpdu[0]&3 == 2
	}
	if native := rt.cfg.DecodeIncomingSMS; native != nil && len(tpdu) > 0 && (tpdu[0]&3 == 0 || tpdu[0]&3 == 2) {
		decoded, handled, err := native(tpdu)
		if err != nil {
			event.DecodeError = "native TPDU decoding failed"
		} else if handled {
			event.Decoded = true
			event.Sender, event.Content = strings.TrimSpace(decoded.Sender), decoded.Content
			event.StatusReport, event.DeliveryStatus, event.StatusCode = decoded.StatusReport, decoded.DeliveryStatus, decoded.StatusCode
			if decoded.HasTimestamp {
				event.Timestamp, event.HasTimestamp = decoded.Timestamp, true
			}
		}
	}
	// The established decoder additionally exposes report correlation and UDH.
	// Raw bytes remain durable even when either decoder cannot interpret them.
	if message, err := device.DecodeSMSDeliverTPDU(tpdu); err == nil {
		event.Recipient = strings.TrimSpace(message.To)
		if !event.Decoded {
			event.Decoded = true
			event.DecodeError = ""
			event.Sender, event.Content = strings.TrimSpace(message.From), message.Text
			event.StatusReport = message.Direction == device.SMSDirectionStatusReport
			event.DeliveryStatus = message.DeliveryStatus
			if message.StatusCode != nil {
				event.StatusCode = byte(*message.StatusCode)
			}
			if message.ServiceCenterTimestamp != nil {
				event.Timestamp, event.HasTimestamp = *message.ServiceCenterTimestamp, true
			}
		}
		if message.MessageReference != nil {
			reference := *message.MessageReference
			event.MessageReference = &reference
		}
		if !event.HasTimestamp && message.ServiceCenterTimestamp != nil {
			event.Timestamp, event.HasTimestamp = *message.ServiceCenterTimestamp, true
		}
		if message.Concat != nil {
			event.Concat = &SMSConcatInfo{Reference: message.Concat.Reference, Total: message.Concat.Total, Sequence: message.Concat.Sequence}
		}
	} else if !event.Decoded {
		event.DecodeError = "TPDU decoding failed"
	}
	if len(tpdu) > 0 && tpdu[0]&3 == 0 {
		if _, _, _, concat, err := smscodec.DecodeDeliverTPDU(tpdu); err == nil && concat.IsConcat {
			event.Concat = &SMSConcatInfo{Reference: concat.Ref, RefBits: concat.RefBits, Total: concat.Total, Sequence: concat.Seq}
		}
	}
	if event.Sender == "" {
		event.Sender = originator
	}
	if err := rt.cfg.IncomingRawSMS(event); err != nil {
		logger.Warn("IMS raw SMS persistence failed; RP-ACK withheld",
			logger.String("trace_id", strings.TrimSpace(rt.cfg.TraceID)))
		return
	}
	if rt.sendRPAck != nil {
		if target := sipMessagePeer(req); target != "" {
			rt.sendRPAck(target, reference)
		}
	}
}
