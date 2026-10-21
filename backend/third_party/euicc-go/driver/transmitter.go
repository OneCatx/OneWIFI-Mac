package driver

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	"github.com/damonto/euicc-go/bertlv"
	sgp22 "github.com/damonto/euicc-go/v2"
	uiccapdu "github.com/damonto/uicc-go/apdu"
)

type SmartCardChannel interface {
	Connect() error
	Disconnect() error
	OpenLogicalChannel(AID []byte) (byte, error)
	Transmit(command []byte) ([]byte, error)
	CloseLogicalChannel(channel byte) error
}

type Transmitter interface {
	sgp22.Transmitter
	Close() error
}

type transmitter struct {
	card   io.ReadWriteCloser
	logger *slog.Logger
}

func NewTransmitter(logger *slog.Logger, channel SmartCardChannel, AID []byte, MSS int) (Transmitter, error) {
	t, err := newCardTransmitter(channel, AID, MSS)
	if err != nil {
		return nil, err
	}
	return &transmitter{card: t, logger: logger}, nil
}

func (t *transmitter) Transmit(request bertlv.Marshaler, response bertlv.Unmarshaler) error {
	req, err := request.MarshalBERTLV()
	if err != nil {
		return err
	}
	bs, err := t.TransmitRaw(req.Bytes())
	if err != nil {
		return err
	}
	var tlv bertlv.TLV
	if err := tlv.UnmarshalBinary(bs); err != nil {
		return err
	}
	return response.UnmarshalBERTLV(&tlv)
}

func (t *transmitter) TransmitRaw(command []byte) ([]byte, error) {
	t.logger.Debug("[APDU] sending", "command", fmt.Sprintf("%X", command))
	if _, err := t.card.Write(command); err != nil {
		return nil, err
	}
	bs, err := io.ReadAll(t.card)
	if err != nil {
		return nil, err
	}
	t.logger.Debug("[APDU] received", "response", fmt.Sprintf("%X", bs))
	return bs, err
}

func (t *transmitter) Close() error {
	return t.card.Close()
}

type cardTransmitter struct {
	MSS            int
	mu             sync.Mutex
	channel        SmartCardChannel
	logicalChannel byte
	response       *bytes.Buffer
}

func newCardTransmitter(channel SmartCardChannel, AID []byte, MSS int) (io.ReadWriteCloser, error) {
	// Reject invalid framing before acquiring a card or opening a channel.
	// lpa.Options.Normalize still maps its optional MSS=0 to the default 254.
	if MSS <= 0 || MSS > 254 {
		return nil, fmt.Errorf("invalid maximum APDU size: %d", MSS)
	}
	var err error
	if err = channel.Connect(); err != nil {
		return nil, err
	}
	var transmitter cardTransmitter
	transmitter.channel = channel
	if transmitter.logicalChannel, err = channel.OpenLogicalChannel(AID); err != nil {
		return nil, err
	}
	transmitter.MSS = MSS
	return &transmitter, nil
}

func (t *cardTransmitter) Read(p []byte) (n int, err error) {
	return t.response.Read(p)
}

func (t *cardTransmitter) Write(command []byte) (int, error) {
	if t.MSS <= 0 || t.MSS > 254 {
		return 0, fmt.Errorf("invalid maximum APDU size: %d", t.MSS)
	}
	if len(command) == 0 {
		return 0, fmt.Errorf("empty STORE DATA command")
	}
	var n int
	t.response = new(bytes.Buffer)
	request := uiccapdu.Request{CLA: 0x80, INS: 0xE2}
	var response uiccapdu.Response
	var err error
	for request.Data = range slices.Chunk(command, t.MSS) {
		// P2 is a byte-sized counter and intentionally wraps. It cannot identify
		// the last frame: exact MSS multiples previously had no final frame,
		// and long payloads could flag an intermediate frame as final.
		if request.P1 = 0x11; n+len(request.Data) == len(command) {
			request.P1 = 0x91
		}
		if response, err = t.transmit(&request); err != nil {
			break
		}
		request.P2++
		n += len(request.Data)
		if !response.HasMore() {
			t.response.Write(response.Data())
			continue
		}
		if err = t.readCommandResponse(t.response, response.SW2()); err != nil {
			break
		}
	}
	return n, err
}

func (t *cardTransmitter) transmit(request *uiccapdu.Request) (uiccapdu.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setChannelToCLA(request, t.logicalChannel)
	command, err := request.MarshalBinary()
	if err != nil {
		return nil, err
	}
	b, err := t.channel.Transmit(command)
	if err != nil {
		return nil, err
	}
	response := uiccapdu.Response(b)
	if !response.OK() && !response.HasMore() {
		err = fmt.Errorf("returned an unexpected response with status %04X", response.SW())
	}
	return response, err
}

func (t *cardTransmitter) setChannelToCLA(request *uiccapdu.Request, channel byte) {
	if channel < 4 {
		request.CLA = (request.CLA & 0x9C) | channel
	} else if channel < 20 {
		request.CLA = (request.CLA & 0xB0) | 0x40 | (channel - 4)
	}
}

func (t *cardTransmitter) readCommandResponse(w io.Writer, le byte) error {
	var err error
	var request uiccapdu.Request
	var response uiccapdu.Response
	request.CLA = 0x80
	request.INS = 0xC0
	request.Le = &le
	for {
		if response, err = t.transmit(&request); err != nil {
			return err
		}
		if _, err = w.Write(response.Data()); err != nil {
			return err
		}
		if !response.HasMore() {
			break
		}
		*request.Le = response.SW2()
	}
	return nil
}

func (t *cardTransmitter) Close() error {
	if err := t.channel.CloseLogicalChannel(t.logicalChannel); err != nil {
		return err
	}
	return t.channel.Disconnect()
}
