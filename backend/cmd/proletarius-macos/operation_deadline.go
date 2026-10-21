package main

import (
	"errors"
	"net/http"
	"time"
)

const (
	defaultResponseWriteTimeout = 30 * time.Second
	// Leave a response margin before the Rust gateway's 180s bound. Request
	// bodies still use the server's 30s ReadTimeout. This changes only the
	// response wait, not a card/SMS operation's ownership or retry policy.
	longResponseWriteTimeout = 175 * time.Second
)

func responseWriteTimeout(method, path string) time.Duration {
	if method != http.MethodPost && method != http.MethodPatch && method != http.MethodDelete {
		return defaultResponseWriteTimeout
	}
	switch path {
	case "/api/sms/send", "/api/vowifi", "/api/reader/vowifi",
		"/api/esim/switch", "/api/esim/download", "/api/esim/profile",
		"/api/reader/esim/switch", "/api/reader/esim/download", "/api/reader/esim/profile":
		return longResponseWriteTimeout
	default:
		return defaultResponseWriteTimeout
	}
}

func operationResponseDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if budget := responseWriteTimeout(r.Method, r.URL.Path); budget > defaultResponseWriteTimeout {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				writeCodedError(w, http.StatusServiceUnavailable, "operation_deadline_unavailable", "暂时无法提交操作，请稍后重试", true)
				return // Do not submit a card/SMS write if its response setup failed.
			}
		}
		next.ServeHTTP(w, r)
	})
}
