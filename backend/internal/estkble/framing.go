// Package estkble implements the ESTKme-RED BLE APDU wire framing. GATT
// transport and card ownership are deliberately kept separate from framing.
package estkble

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	ServiceUUID = "4553"
	NotifyUUID  = "544b"
	WriteUUID   = "6d65"
	MaxPayload  = 65535
)

var (
	Claim    = []byte{2, 6, 0, 'E', 'S', 'T', 'K', 'm', 'e'}
	PowerOn  = []byte{3, 2, 0, 1, 1}
	PowerOff = []byte{3, 0, 0}
	Disclaim = []byte{2, 0, 0}
)

// APDURequest prefixes the APDU with command 4 and a uint16 little-endian
// length, as used by NekokoLPA's ESTKme-RED transport.
func APDURequest(apdu []byte) ([]byte, error) {
	if len(apdu) > MaxPayload {
		return nil, fmt.Errorf("ESTK APDU exceeds %d bytes", MaxPayload)
	}
	frame := make([]byte, len(apdu)+3)
	frame[0] = 4
	binary.LittleEndian.PutUint16(frame[1:3], uint16(len(apdu)))
	copy(frame[3:], apdu)
	return frame, nil
}

// Reply accumulates one framed notification followed by raw continuation
// notifications. A malformed or oversized frame fails closed.
type Reply struct {
	expected int
	data     []byte
	started  bool
}

func (r *Reply) Push(notification []byte) (bool, error) {
	if !r.started {
		if len(notification) < 3 {
			return false, errors.New("ESTK response header truncated")
		}
		r.started = true
		r.expected = int(binary.LittleEndian.Uint16(notification[1:3]))
		notification = notification[3:]
	}
	if len(notification) > r.expected-len(r.data) {
		return false, errors.New("ESTK response exceeds declared length")
	}
	r.data = append(r.data, notification...)
	return len(r.data) == r.expected, nil
}

func (r *Reply) Bytes() []byte { return append([]byte(nil), r.data...) }
