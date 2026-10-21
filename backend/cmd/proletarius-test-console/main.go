// proletarius-test-console is an explicit, local-only diagnostic client.
// Start a separate development backend first; the production app's ephemeral
// token is intentionally not exported to this tool.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type action struct {
	method, path string
	mutates      bool
}

var actions = map[string]action{
	"scan":                 {"GET", "/api/reader/list", false},
	"bluetooth-status":     {"GET", "/api/reader/ble/status", false},
	"bluetooth-scan":       {"POST", "/api/reader/ble/scan", true},
	"bluetooth-connect":    {"POST", "/api/reader/ble/connect", true},
	"bluetooth-disconnect": {"POST", "/api/reader/ble/disconnect", true},
	"card":                 {"GET", "/api/reader/info", false},
	"profiles":             {"GET", "/api/reader/esim", false},
	"download-status":      {"GET", "/api/reader/esim/operation", false},
	"network":              {"GET", "/api/network", false},
	"ims":                  {"GET", "/api/reader/vowifi", false},
	"vowifi":               {"GET", "/api/reader/vowifi", false},
	"sms":                  {"GET", "/api/sms/status", false},
	"call":                 {"GET", "/api/reader/vowifi/calls/status", false},
	"connect":              {"POST", "/api/reader/connect", true},
	"disconnect":           {"POST", "/api/reader/disconnect", true},
	"refresh-card":         {"POST", "/api/reader/refresh", true},
	"switch":               {"POST", "/api/reader/esim/switch", true},
	"download":             {"POST", "/api/reader/esim/download", true},
	"sms-send":             {"POST", "/api/sms/send", true},
	"call-dial":            {"POST", "/api/reader/vowifi/calls/dial", true},
}

func main() {
	socket := flag.String("socket", "", "development backend Unix socket path")
	input := flag.String("json", "", "JSON request body for state-changing actions")
	execute := flag.Bool("execute", false, "confirm a state-changing operation")
	flag.Parse()
	if err := run(*socket, *input, *execute, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(socket, input string, execute bool, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: proletarius-test-console -socket /path/to/dev.sock [--execute --json '{...}'] ACTION")
	}
	selected, ok := actions[args[0]]
	if !ok {
		return fmt.Errorf("unknown action %q", args[0])
	}
	if selected.mutates && !execute {
		return errors.New("state-changing action requires --execute")
	}
	if strings.TrimSpace(socket) == "" {
		return errors.New("development backend socket path required")
	}
	if selected.mutates && input == "" && args[0] != "disconnect" && args[0] != "refresh-card" && args[0] != "bluetooth-scan" && args[0] != "bluetooth-disconnect" {
		return errors.New("action requires --json request body")
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}
	request, err := http.NewRequest(selected.method, "http://localhost"+selected.path, bytes.NewBufferString(input))
	if err != nil {
		return err
	}
	if input != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token := os.Getenv("PROLETARIUS_APP_TOKEN"); token != "" {
		request.Header.Set("X-Proletarius-App-Token", token)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// Avoid printing raw SIM/IMS/SMS data by default. Opt in explicitly when
	// using a private local terminal for diagnostics.
	if os.Getenv("PROLETARIUS_TEST_CONSOLE_SHOW_BODY") == "1" {
		_, err = io.Copy(os.Stdout, io.LimitReader(response.Body, 1<<20))
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("action=%s status=%d (body hidden; set PROLETARIUS_TEST_CONSOLE_SHOW_BODY=1 to inspect)\n", args[0], response.StatusCode)
	}
	if response.StatusCode >= 400 {
		return fmt.Errorf("%s returned HTTP %d", args[0], response.StatusCode)
	}
	return nil
}
