package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"proletarius.local/core/internal/device"
	"strings"
	"testing"
)

func TestNativeSMSSubmitParity(t *testing.T) {
	binary := os.Getenv("PROLETARIUS_SUBMIT_FIXTURE")
	if binary == "" {
		t.Skip("set PROLETARIUS_SUBMIT_FIXTURE to the Rust offline example")
	}
	texts := []string{"BAL", "PAC", "£€{}[]\\~|^", "中文", "😀", strings.Repeat("A", 160), strings.Repeat("A", 161), strings.Repeat("A", 152) + "^" + strings.Repeat("B", 8), strings.Repeat("中", 71), strings.Repeat("中", 66) + "😀" + strings.Repeat("文", 4)}
	for _, recipient := range []string{"888", "65075", "+44 (7700) 900123"} {
		for index, text := range texts {
			expected, err := device.PrepareSMSSubmitTPDUs(recipient, text)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := exec.Command(binary, recipient, text).Output()
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(string(raw))
			if len(lines) != len(expected) {
				t.Fatalf("case %d: part count differs", index)
			}
			for part, line := range lines {
				actual, err := hex.DecodeString(line)
				if err != nil {
					t.Fatal(err)
				}
				wanted := append([]byte(nil), expected[part].TPDU...)
				if wanted[0]&0x40 != 0 {
					// Only normalize the random concatenation reference, not payload or flags.
					userData := 7 + (int(wanted[2])+1)/2
					wanted[userData+3] = 42
				}
				if !bytes.Equal(actual, wanted) {
					t.Fatalf("case %d part %d: Rust %x Go %x", index, part, actual, wanted)
				}
			}
		}
	}
}
