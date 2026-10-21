package estkble

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Exchanger is implemented by the macOS GATT bridge. Calls must preserve
// request/response ordering; a card's currently selected app is shared state.
type Exchanger interface {
	Exchange(context.Context, []byte) ([]byte, error)
}

// Channel adapts an ESTK BLE APDU exchanger to euicc-go's SmartCardChannel.
// It does not claim or power-cycle the reader: that belongs to its transport
// owner so parallel LPA and AKA operations cannot reset each other's card.
type Channel struct {
	mu     sync.Mutex
	tx     Exchanger
	closed bool
}

const apduExchangeTimeout = 125 * time.Second

func NewChannel(tx Exchanger) *Channel { return &Channel{tx: tx} }

func (c *Channel) Connect() error {
	if c.tx == nil {
		return errors.New("ESTK BLE APDU transport unavailable")
	}
	return nil
}

func (c *Channel) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *Channel) Transmit(command []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("ESTK BLE channel closed")
	}
	if c.tx == nil {
		return nil, errors.New("ESTK BLE APDU transport unavailable")
	}
	// The macOS GATT owner permits a single APDU to run for up to 120s during
	// profile transfer. Leave a small margin here so its timeout/cleanup error
	// reaches the caller rather than this adapter abandoning the exchange first.
	ctx, cancel := context.WithTimeout(context.Background(), apduExchangeTimeout)
	defer cancel()
	return c.tx.Exchange(ctx, command)
}

func (c *Channel) OpenLogicalChannel(aid []byte) (byte, error) {
	if len(aid) == 0 || len(aid) > 255 {
		return 0, errors.New("invalid ESTK AID length")
	}
	response, err := c.Transmit([]byte{0, 0x70, 0, 0, 1})
	if err != nil {
		return 0, err
	}
	if len(response) < 3 || response[len(response)-2] != 0x90 || response[len(response)-1] != 0 {
		return 0, fmt.Errorf("ESTK open logical channel failed: %X", response)
	}
	channel := response[0]
	if channel == 0 || channel > 19 {
		return 0, fmt.Errorf("invalid ESTK logical channel %d", channel)
	}
	cla := channel
	if channel >= 4 {
		cla = 0x40 | (channel - 4)
	}
	selectAPDU := append([]byte{cla, 0xA4, 4, 0, byte(len(aid))}, aid...)
	response, err = c.Transmit(selectAPDU)
	if err != nil || len(response) < 2 || (response[len(response)-2] != 0x90 && response[len(response)-2] != 0x61) {
		_ = c.CloseLogicalChannel(channel)
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("ESTK select AID failed: %X", response)
	}
	return channel, nil
}

func (c *Channel) CloseLogicalChannel(channel byte) error {
	if channel == 0 {
		// Basic channel: nothing to close.
		return nil
	}
	if channel > 19 {
		return errors.New("invalid ESTK logical channel")
	}
	response, err := c.Transmit([]byte{0, 0x70, 0x80, channel, 0})
	if err != nil {
		return err
	}
	if len(response) < 2 || response[len(response)-2] != 0x90 || response[len(response)-1] != 0 {
		return fmt.Errorf("ESTK close logical channel failed: %X", response)
	}
	return nil
}
