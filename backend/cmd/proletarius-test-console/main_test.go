package main

import "testing"

func TestConsoleRejectsUnsafeRequests(t *testing.T) {
	if err := run("/missing", "", false, []string{"switch"}); err == nil {
		t.Fatal("switch without --execute accepted")
	}
	if err := run("/missing", "", true, []string{"switch"}); err == nil {
		t.Fatal("switch without body accepted")
	}
	if err := run("/missing", "", false, []string{"bluetooth-scan"}); err == nil {
		t.Fatal("BLE scan without --execute accepted")
	}
}
