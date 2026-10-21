package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeMSISDNRecordInternational(t *testing.T) {
	record := make([]byte, 32)
	for index := range record {
		record[index] = 0xFF
	}
	footer := record[len(record)-14:]
	footer[0] = 7
	footer[1] = 0x91
	copy(footer[2:], []byte{0x44, 0x77, 0x00, 0x09, 0x10, 0x32})
	footer[13] = 0xFF

	if got, want := decodeMSISDNRecord(record), "+447700900123"; got != want {
		t.Fatalf("decodeMSISDNRecord() = %q, want %q", got, want)
	}
}

func TestDecodeMSISDNRecordRejectsExtension(t *testing.T) {
	record := make([]byte, 14)
	for index := range record {
		record[index] = 0xFF
	}
	record[0] = 2
	record[1] = 0x91
	record[2] = 0x21
	record[13] = 1
	if got := decodeMSISDNRecord(record); got != "" {
		t.Fatalf("decodeMSISDNRecord() = %q, want empty for extension record", got)
	}
}

func TestUpdatePhoneNumberRejectsStaleGeneration(t *testing.T) {
	manager := &readerManager{
		readerName: "reader",
		generation: 4,
		cachedUSIM: &usimInfo{ICCID: "1"},
	}
	if manager.UpdatePhoneNumber(3, "+447700900123") {
		t.Fatal("UpdatePhoneNumber() accepted stale generation")
	}
	if got := manager.USIMInfo().PhoneNumber; got != "" {
		t.Fatalf("PhoneNumber = %q after stale update, want empty", got)
	}
	if !manager.UpdatePhoneNumber(4, "+447700900123") {
		t.Fatal("UpdatePhoneNumber() rejected current generation")
	}
	if got := manager.USIMInfo().PhoneNumber; got != "+447700900123" {
		t.Fatalf("PhoneNumber = %q, want registered number", got)
	}
}

func TestModulePhoneFallbackUsesProfileNoteByICCID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile-notes.json")
	data := []byte(`{"8901234567890123456":{"label":"Travel","phone":"+447700900123","tags":""}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	application := &app{profileNotesPath: path}
	if got := application.profilePhoneForICCID("8901234567890123456"); got != "+447700900123" {
		t.Fatalf("profilePhoneForICCID() = %q, want saved phone", got)
	}
}

func TestIsMissingPhoneNumber(t *testing.T) {
	for _, value := range []string{"", " ", "-", "unknown", "号码未知", "未知号码", "N/A", "0000000000", "+0000000000", "+"} {
		if !isMissingPhoneNumber(value) {
			t.Errorf("isMissingPhoneNumber(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"+447700900123", "123456789"} {
		if isMissingPhoneNumber(value) {
			t.Errorf("isMissingPhoneNumber(%q) = true, want false", value)
		}
	}
}
