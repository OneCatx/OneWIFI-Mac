package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/damonto/uicc-go/ccid"
	usimcard "github.com/damonto/uicc-go/usim/card"
	"proletarius.local/core/internal/estkble"
)

const bleReaderPrefix = "BLE:"

func isBLEReader(name string) bool { return strings.HasPrefix(name, bleReaderPrefix) }

// errBLEBridgeTransition means the app-owned CoreBluetooth manager still
// intends to restore the selected GATT/APDU path. It is deliberately distinct
// from a confirmed empty bridge: hot-unplug callers must not turn a normal
// reconnect window into a card/session teardown.
var errBLEBridgeTransition = errors.New("BLE bridge reconnect in progress")

func readerBLEClient() estkble.Client {
	return estkble.Client{Socket: os.Getenv("PROLETARIUS_BLE_SOCKET"), Token: os.Getenv("PROLETARIUS_APP_TOKEN")}
}

// connectedBLEReader distinguishes a confirmed terminal bridge state (empty
// reader, nil error) from an unavailable or transitional bridge. Callers that
// decide whether to retire a live session must never treat the latter as a
// physical unplug.
func connectedBLEReader(ctx context.Context) (string, error) {
	_, reader, state, _, err := readerBLEClient().Status(ctx)
	if err != nil {
		return "", err
	}
	state = strings.ToLower(strings.TrimSpace(state))
	switch state {
	case "connected":
		if reader == "" {
			return "", errors.New("BLE bridge reported connected without a reader identity")
		}
	case "connecting", "discovering", "reconnecting":
		if reader != "" {
			return "", errors.New("BLE bridge reported a reader during a transitional state")
		}
		return "", fmt.Errorf("%w: %s", errBLEBridgeTransition, state)
	case "idle", "scanning", "disconnected", "failed":
		if reader != "" {
			return "", errors.New("BLE bridge reported a reader while disconnected")
		}
	case "":
		return "", errors.New("BLE bridge response omitted connection state")
	default:
		return "", fmt.Errorf("BLE bridge returned an unknown connection state %q", state)
	}
	if reader != "" && !isBLEReader(reader) {
		return "", errors.New("BLE bridge returned an invalid reader identity")
	}
	return reader, nil
}

func listConnectedBLEReader(ctx context.Context) string {
	reader, err := connectedBLEReader(ctx)
	if err != nil {
		return ""
	}
	return reader
}

type bleUSIMTransmitter struct{ client estkble.Client }

func (b *bleUSIMTransmitter) Transmit(ctx context.Context, apdu []byte) ([]byte, error) {
	return b.client.Exchange(ctx, apdu)
}
func (b *bleUSIMTransmitter) Close() error { return nil }

func openReaderTransmitter(ctx context.Context, name string) (usimcard.Transmitter, error) {
	if !isBLEReader(name) {
		return ccid.Open(ctx, name)
	}
	if listConnectedBLEReader(ctx) != name {
		return nil, errors.New("ESTK BLE reader disconnected")
	}
	return &bleUSIMTransmitter{client: readerBLEClient()}, nil
}
