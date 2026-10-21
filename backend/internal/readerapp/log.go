package readerapp

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

type OperationLog struct {
	Timestamp time.Time `json:"timestamp"`
	Module    string    `json:"module"`
	Action    string    `json:"action"`
	State     string    `json:"state"`
	Result    string    `json:"result"`
	ErrorCode string    `json:"errorCode,omitempty"`
}

// Logger emits only structured metadata, never APDUs, IMS secrets, phone
// numbers, SMS text, activation codes, or raw carrier responses.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewLogger(writer io.Writer) *Logger { return &Logger{writer: writer} }
func (l *Logger) Write(module, action, state, result, errorCode string) error {
	entry := OperationLog{Timestamp: time.Now().UTC(), Module: module,
		Action: action, State: state, Result: result, ErrorCode: errorCode}
	l.mu.Lock()
	defer l.mu.Unlock()
	return json.NewEncoder(l.writer).Encode(entry)
}
