package main

import (
	"context"
	"testing"
)

func TestResolveRuntimeDTMFFallbackIsRFC4733Compatible(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	for digit, want := range map[string]byte{
		"0": 0, "8": 8, "*": 10, "#": 11,
	} {
		got, err := resolveRuntimeDTMF(context.Background(), digit)
		if err != nil || got != want {
			t.Fatalf("digit %q got=%d err=%v, want=%d", digit, got, err, want)
		}
	}
}

func TestResolveRuntimeDTMFRejectsAmbiguousInput(t *testing.T) {
	t.Setenv("PROLETARIUS_CARRIER_SOCKET", "")
	for _, input := range []string{"", "12", "A", "#0"} {
		if _, err := resolveRuntimeDTMF(context.Background(), input); err == nil {
			t.Fatalf("input %q was accepted", input)
		}
	}
}
