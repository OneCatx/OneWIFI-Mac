package estkble

import (
	"bytes"
	"context"
	"testing"
	"time"
)

type scriptExchange struct {
	commands  [][]byte
	responses [][]byte
	deadline  time.Time
}

func (s *scriptExchange) Exchange(ctx context.Context, command []byte) ([]byte, error) {
	s.deadline, _ = ctx.Deadline()
	s.commands = append(s.commands, append([]byte(nil), command...))
	answer := s.responses[0]
	s.responses = s.responses[1:]
	return answer, nil
}

func TestChannelUsesProfileTransferTimeoutBudget(t *testing.T) {
	tx := &scriptExchange{responses: [][]byte{{0x90, 0x00}}}
	ch := NewChannel(tx)
	started := time.Now()
	if _, err := ch.Transmit([]byte{0, 0xA4, 0, 0}); err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(tx.deadline)
	if remaining < 120*time.Second || remaining > apduExchangeTimeout || tx.deadline.Before(started) {
		t.Fatalf("remaining timeout=%s; expected about %s", remaining, apduExchangeTimeout)
	}
}

func TestLogicalChannel(t *testing.T) {
	tx := &scriptExchange{responses: [][]byte{{1, 0x90, 0}, {0x90, 0}, {0x90, 0}}}
	ch := NewChannel(tx)
	if err := ch.Connect(); err != nil {
		t.Fatal(err)
	}
	id, err := ch.OpenLogicalChannel([]byte{0xA0, 0x00})
	if err != nil || id != 1 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if !bytes.Equal(tx.commands[1], []byte{1, 0xA4, 4, 0, 2, 0xA0, 0}) {
		t.Fatalf("select=%X", tx.commands[1])
	}
	if err := ch.CloseLogicalChannel(id); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tx.commands[2], []byte{0, 0x70, 0x80, 1, 0}) {
		t.Fatalf("close=%X", tx.commands[2])
	}
}
