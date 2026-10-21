package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"proletarius.local/core/internal/vowifi/imscore"
)

func TestCallHistoryKeepsModuleAndReaderSessionsSeparate(t *testing.T) {
	a := &app{callLog: newCallLog(filepath.Join(t.TempDir(), "calls.json"))}

	a.beginDeviceCall(callDeviceModule, "out", "+441111")
	a.trackReaderCallState(imscore.CallState{State: "incoming", Direction: "incoming", Peer: "+442222"})
	a.trackReaderCallState(imscore.CallState{State: "active", Direction: "incoming", Peer: "+442222"})
	a.endDeviceCall(callDeviceModule)
	a.trackReaderCallState(imscore.CallState{State: "ended", Direction: "incoming", Peer: "+442222"})

	records := a.callLog.snapshot()
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	byDevice := make(map[string]callRecord, len(records))
	for _, record := range records {
		byDevice[record.Device] = record
	}
	if got := byDevice[callDeviceModule]; got.Number != "+441111" || got.Direction != "out" {
		t.Fatalf("module record = %#v", got)
	}
	if got := byDevice[callDeviceReader]; got.Number != "+442222" || got.Direction != "in" || !got.Answered {
		t.Fatalf("reader record = %#v", got)
	}
}

func TestIndependentCallLogsSerializeWithoutLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	logs := []*callLog{newCallLog(path), newCallLog(path)}
	var wait sync.WaitGroup
	for index := 0; index < 40; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			logs[index%len(logs)].add(callRecord{ID: fmt.Sprintf("call-%d", index)})
		}(index)
	}
	wait.Wait()
	records := newCallLog(path).snapshot()
	if len(records) != 40 {
		t.Fatalf("record count = %d, want 40", len(records))
	}
}

func TestCorruptCallArchiveIsNotOverwrittenByMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	corrupt := []byte("{broken")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	log := newCallLog(path)
	log.add(callRecord{ID: "must-not-overwrite"})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("corrupt archive was overwritten: %q", got)
	}
}

func TestLegacyCallHistoryWithoutDeviceStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.json")
	log := newCallLog(path)
	log.add(callRecord{ID: "legacy", Direction: "out", Number: "+443333"})

	reloaded := newCallLog(path).snapshot()
	if len(reloaded) != 1 || reloaded[0].Device != "" || reloaded[0].Number != "+443333" {
		t.Fatalf("legacy record = %#v", reloaded)
	}
}
