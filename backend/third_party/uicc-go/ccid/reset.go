package ccid

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ElMostafaIdrassi/goscard"
)

// ResetCard opens an exclusive card handle and closes it with the requested
// reset disposition. It shares Open/ListReaders' library reference count,
// keeping PC/SC loaded until both the card and context have been released.
func ResetCard(ctx context.Context, readerName string, disposition goscard.SCardDisposition) error {
	return resetCardWithContext(ctx, readerName, disposition, openCardResetContext)
}

type cardResetContext interface {
	connectExclusive(string) (cardResetConnection, error)
	release() error
}

type cardResetConnection interface {
	disconnect(goscard.SCardDisposition) error
}

func resetCardWithContext(ctx context.Context, readerName string, disposition goscard.SCardDisposition, open func() (cardResetContext, error)) (result error) {
	if ctx == nil {
		ctx = context.Background()
	}
	readerName = strings.TrimSpace(readerName)
	if readerName == "" {
		return errors.New("resetting reader: reader name is empty")
	}
	if disposition != goscard.SCardResetCard && disposition != goscard.SCardUnpowerCard {
		return errors.New("resetting reader: unsupported reset disposition")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("resetting reader: %w", err)
	}
	resource, err := open()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, resource.release()) }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("resetting reader: %w", err)
	}
	card, err := resource.connectExclusive(readerName)
	if err != nil {
		return fmt.Errorf("connecting exclusively to %s: %w", readerName, err)
	}
	// After acquiring the handle, cleanup must complete even on cancellation.
	defer func() { result = errors.Join(result, card.disconnect(disposition)) }()
	return ctx.Err()
}

type pcscCardResetContext struct{ context goscard.Context }
type pcscCardResetConnection struct{ card goscard.Card }

func openCardResetContext() (cardResetContext, error) {
	ctx, err := newContext()
	if err != nil {
		return nil, err
	}
	return &pcscCardResetContext{context: ctx}, nil
}

func (c *pcscCardResetContext) connectExclusive(readerName string) (cardResetConnection, error) {
	card, _, err := c.context.Connect(readerName, goscard.SCardShareExclusive, goscard.SCardProtocolAny)
	if err != nil {
		return nil, err
	}
	return &pcscCardResetConnection{card: card}, nil
}

func (c *pcscCardResetContext) release() error {
	defer releasePCSC()
	if _, err := c.context.Release(); err != nil {
		return fmt.Errorf("releasing reset PC/SC context: %w", err)
	}
	return nil
}

func (c *pcscCardResetConnection) disconnect(disposition goscard.SCardDisposition) error {
	if _, err := c.card.Disconnect(disposition); err != nil {
		return fmt.Errorf("resetting and disconnecting card: %w", err)
	}
	return nil
}
