package readerapp

import "testing"

func TestSessionRegistryIsolatesReaderAndSIM(t *testing.T) {
	registry := NewSessionRegistry[string]()
	readerOneSIMOne, _ := NewSessionKey("reader-1", "iccid-1")
	readerOneSIMTwo, _ := NewSessionKey("reader-1", "iccid-2")
	readerTwoSIMOne, _ := NewSessionKey("reader-2", "iccid-1")
	for key, value := range map[SessionKey]string{
		readerOneSIMOne: "r1-s1", readerOneSIMTwo: "r1-s2", readerTwoSIMOne: "r2-s1",
	} {
		if err := registry.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := registry.Get(readerOneSIMOne); got != "r1-s1" {
		t.Fatalf("reader 1 SIM 1 overwritten: %q", got)
	}
	if got, _ := registry.Get(readerOneSIMTwo); got != "r1-s2" {
		t.Fatalf("reader 1 SIM 2 overwritten: %q", got)
	}
	if got, _ := registry.Get(readerTwoSIMOne); got != "r2-s1" {
		t.Fatalf("reader 2 SIM 1 overwritten: %q", got)
	}
	registry.Delete(readerOneSIMOne)
	if _, ok := registry.Get(readerOneSIMOne); ok {
		t.Fatal("deleted session remains visible")
	}
	if len(registry.Keys()) != 2 {
		t.Fatalf("unrelated sessions were removed: %v", registry.Keys())
	}
}

func TestSessionKeyRejectsMissingIsolationIdentity(t *testing.T) {
	for _, input := range [][2]string{{"", "iccid"}, {"reader", ""}} {
		if _, err := NewSessionKey(ReaderID(input[0]), input[1]); err == nil {
			t.Fatalf("accepted incomplete key %#v", input)
		}
	}
}
