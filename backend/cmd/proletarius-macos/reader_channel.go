package main

import (
	"errors"
	"sync"

	"github.com/damonto/euicc-go/driver"
)

// The ESTKme factory allocates one physical connection per LPA/SIM-auth
// channel. Wrap only those owned connections; BLE and persistent shared
// exchangers retain their existing lifetimes.
func ownReaderChannel(channel driver.SmartCardChannel) driver.SmartCardChannel {
	return &ownedReaderChannel{SmartCardChannel: channel}
}

type ownedReaderChannel struct {
	driver.SmartCardChannel
	mu               sync.Mutex
	logicalCloseDone bool
	logicalCloseErr  error
	disconnected     bool
	disconnectErr    error
}

func (c *ownedReaderChannel) CloseLogicalChannel(channel byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logicalCloseDone || c.disconnected {
		return errors.Join(c.logicalCloseErr, c.disconnectErr)
	}
	c.logicalCloseDone = true
	c.logicalCloseErr = c.SmartCardChannel.CloseLogicalChannel(channel)
	if c.logicalCloseErr != nil {
		// LPA's transmitter returns immediately on a logical-close error.
		// A reset can invalidate that logical channel while leaving our
		// shared PC/SC handle alive, so release the owned handle here.
		return errors.Join(c.logicalCloseErr, c.disconnectLocked())
	}
	return nil
}

func (c *ownedReaderChannel) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disconnectLocked()
}

func (c *ownedReaderChannel) disconnectLocked() error {
	if !c.disconnected {
		c.disconnected = true
		c.disconnectErr = c.SmartCardChannel.Disconnect()
	}
	return c.disconnectErr
}
