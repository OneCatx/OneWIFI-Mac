package main

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

const (
	appTokenEnvironment = "PROLETARIUS_APP_TOKEN"
	appTokenHeader      = "X-Proletarius-App-Token"
)

// localAccessMiddleware restricts the private backend socket to the native
// application when it supplies a launch token. Development builds without a
// token remain usable from the local API probe.
func localAccessMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := strings.TrimSpace(os.Getenv(appTokenEnvironment))
		provided := r.Header.Get(appTokenHeader)
		if expected != "" && !localTokenMatches(provided, expected) {
			writeCodedError(w, http.StatusUnauthorized, "authentication_required",
				"需要 Switch App 本机凭证", false)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localTokenMatches(provided, expected string) bool {
	if provided == "" || expected == "" || len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
