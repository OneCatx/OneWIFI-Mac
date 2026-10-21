package estkble

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Client talks only to the app-owned, token-authenticated BLE bridge socket.
// One request per connection keeps the bridge's APDU ownership explicit.
type Client struct {
	Socket string
	Token  string
}

const (
	defaultBridgeRequestTimeout = 35 * time.Second
	// Profile-management APDUs may remain in the reader while it transfers a
	// large eSIM package. Keep the bridge deadline longer than the Swift-side
	// APDU watchdog (120s), so the watchdog can invalidate the GATT session and
	// return a useful error instead of leaving its pending reply occupied.
	bridgeAPDURequestTimeout = 135 * time.Second
)

func requestTimeout(action string) time.Duration {
	if action == "apdu" || action == "reset" {
		return bridgeAPDURequestTimeout
	}
	return defaultBridgeRequestTimeout
}

type bridgeRequest struct {
	Token  string `json:"token"`
	Action string `json:"action"`
	Hex    string `json:"hex,omitempty"`
	Reader string `json:"reader,omitempty"`
}
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type bridgeResponse struct {
	OK        bool     `json:"ok"`
	Reader    string   `json:"reader,omitempty"`
	Devices   []Device `json:"devices,omitempty"`
	State     string   `json:"state,omitempty"`
	Hex       string   `json:"hex,omitempty"`
	ErrorCode string   `json:"errorCode,omitempty"`
}

func (c Client) request(ctx context.Context, action string, data []byte, reader string) (bridgeResponse, error) {
	if strings.TrimSpace(c.Socket) == "" || len(c.Token) < 32 {
		return bridgeResponse{}, errors.New("BLE bridge is not configured")
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return bridgeResponse{}, fmt.Errorf("BLE bridge unavailable: %w", err)
	}
	defer conn.Close()
	// Cancel only this request's local wait. The GATT bridge still owns any
	// submitted APDU until it completes or its watchdog retires it; never
	// reconnect/replay here, since the card may already have applied a write.
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancellation()
	deadline := time.Now().Add(requestTimeout(action))
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	if err := conn.SetDeadline(deadline); err != nil {
		if ctx.Err() != nil {
			return bridgeResponse{}, fmt.Errorf("BLE bridge request: %w", ctx.Err())
		}
		return bridgeResponse{}, err
	}
	request := bridgeRequest{Token: c.Token, Action: action, Reader: reader}
	if action == "apdu" {
		request.Hex = hex.EncodeToString(data)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		if ctx.Err() != nil {
			return bridgeResponse{}, fmt.Errorf("BLE bridge request: %w", ctx.Err())
		}
		return bridgeResponse{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 160_000)).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return bridgeResponse{}, fmt.Errorf("BLE bridge response: %w", ctx.Err())
		}
		return bridgeResponse{}, fmt.Errorf("BLE bridge response: %w", err)
	}
	var response bridgeResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return bridgeResponse{}, fmt.Errorf("BLE bridge malformed response: %w", err)
	}
	if !response.OK {
		if response.ErrorCode == "" {
			response.ErrorCode = "ble_unknown_error"
		}
		return bridgeResponse{}, fmt.Errorf("BLE bridge: %s", response.ErrorCode)
	}
	return response, nil
}

func (c Client) Reader(ctx context.Context) (string, error) {
	response, err := c.request(ctx, "status", nil, "")
	if err != nil {
		return "", err
	}
	return response.Reader, nil
}

func (c Client) Exchange(ctx context.Context, apdu []byte) ([]byte, error) {
	if len(apdu) == 0 || len(apdu) > MaxPayload {
		return nil, errors.New("invalid BLE APDU length")
	}
	response, err := c.request(ctx, "apdu", apdu, "")
	if err != nil {
		return nil, err
	}
	answer, err := hex.DecodeString(response.Hex)
	if err != nil {
		return nil, fmt.Errorf("BLE bridge response hex: %w", err)
	}
	return answer, nil
}

func (c Client) Reset(ctx context.Context) error {
	_, err := c.request(ctx, "reset", nil, "")
	return err
}

func (c Client) Devices(ctx context.Context) ([]Device, string, error) {
	response, err := c.request(ctx, "status", nil, "")
	if err != nil {
		return nil, "", err
	}
	return response.Devices, response.Reader, nil
}

func (c Client) Status(ctx context.Context) ([]Device, string, string, string, error) {
	response, err := c.request(ctx, "status", nil, "")
	if err != nil {
		return nil, "", "", "", err
	}
	return response.Devices, response.Reader, response.State, response.ErrorCode, nil
}

func (c Client) Scan(ctx context.Context) error {
	_, err := c.request(ctx, "scan", nil, "")
	return err
}

func (c Client) Connect(ctx context.Context, readerUUID string) error {
	if strings.TrimSpace(readerUUID) == "" {
		return errors.New("BLE reader UUID required")
	}
	_, err := c.request(ctx, "connect", nil, readerUUID)
	return err
}

func (c Client) Disconnect(ctx context.Context) error {
	_, err := c.request(ctx, "disconnect", nil, "")
	return err
}
