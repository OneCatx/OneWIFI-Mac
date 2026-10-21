package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func multiToken() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

type multiRPCError struct {
	Status int
	Body   string
}

func (e *multiRPCError) Error() string {
	return fmt.Sprintf("多待存储拒绝请求 (%d): %.500s", e.Status, e.Body)
}

type multiStoreClient struct {
	client *http.Client
	token  string
}

func newMultiStoreClient() *multiStoreClient {
	socket := os.Getenv("PROLETARIUS_CARRIER_SOCKET")
	return &multiStoreClient{token: os.Getenv("SWITCH_INTERNAL_TOKEN"), client: &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}}
}
func (c *multiStoreClient) call(ctx context.Context, path string, input, output any) error {
	if c.token == "" {
		return fmt.Errorf("多待持久化服务尚未就绪")
	}
	method := http.MethodGet
	var data []byte
	var err error
	if input != nil {
		method = http.MethodPost
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/internal/multistandby/"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-Switch-Internal-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &multiRPCError{Status: resp.StatusCode, Body: string(body)}
	}
	if output != nil {
		return json.Unmarshal(body, output)
	}
	return nil
}

// retry uses exactly the same event_id and payload. No retry can duplicate a
// store transition or a physical SMS submission.
func (c *multiStoreClient) durable(ctx context.Context, path string, input, output any) error {
	var err error
	for i := 0; i < 3; i++ {
		err = c.call(ctx, "store/"+path, input, output)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	return err
}

type multiStoredMessage struct {
	MessageID string `json:"message_id"`
	ProfileID string `json:"profile_id"`
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	State     string `json:"state"`
}

func (c *multiStoreClient) reservation(ctx context.Context, action, token string) error {
	var err error
	for i := 0; i < 3; i++ {
		err = c.call(ctx, "reader-reservation/"+action, map[string]any{"owner_token": token}, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	return err
}
