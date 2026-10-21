package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	usimcommand "github.com/damonto/uicc-go/usim/command"
	"github.com/damonto/uicc-go/usim/simfile"
)

// This script uses only synthetic card data. Its allowlist rejects every APDU
// outside EF_DIR, EF_ICCID, EF_IMSI and the final AUTHENTICATE, including SMSP,
// EF_AD, GID1 and ISIM inventory reads. One transmitter serves the whole script.
type authIdentityStep struct {
	request, response []byte
	err               error
}

type authIdentityTransmitter struct {
	t          *testing.T
	steps      []authIdentityStep
	next       int
	authCalls  int
	closeCalls int
}

func (f *authIdentityTransmitter) Transmit(ctx context.Context, req []byte) ([]byte, error) {
	f.t.Helper()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.closeCalls != 0 {
		f.t.Fatal("transmitter was closed between identity check and AUTHENTICATE")
	}
	if len(req) > 1 && req[1] == 0x88 {
		f.authCalls++
	}
	if f.next >= len(f.steps) {
		f.t.Fatalf("unexpected synthetic APDU: %X", req)
	}
	step := f.steps[f.next]
	f.next++
	if !bytes.Equal(req, step.request) {
		f.t.Fatalf("synthetic APDU %d = %X, want %X", f.next, req, step.request)
	}
	if step.err != nil {
		return nil, step.err
	}
	return append(append([]byte(nil), step.response...), 0x90, 0x00), nil
}

func (f *authIdentityTransmitter) Close() error {
	f.closeCalls++
	return nil
}

func marshalAuthIdentityFixture(t *testing.T, v interface{ MarshalBinary() ([]byte, error) }) []byte {
	t.Helper()
	b, err := v.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func authIdentityHex(t *testing.T, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newAuthIdentityFixture(t *testing.T, identity, isim, onlyISIM bool) (*authIdentityTransmitter, *usimInfo) {
	t.Helper()
	expected := &usimInfo{ICCID: "8900000000000000001", IMSI: "001010123456789"}
	usimAID := authIdentityHex(t, "A0000000871002")
	isimAID := authIdentityHex(t, "A0000000871004")
	master := marshalAuthIdentityFixture(t, simfile.FCI{FileStructure: simfile.StructureTransparent, FileType: simfile.FileTypeDFOrADF, FileSize: 2})
	apps := []simfile.EFDirRecord{{AID: usimAID, Label: "USIM"}, {AID: isimAID, Label: "ISIM"}}
	if onlyISIM {
		apps = apps[1:]
	}
	directory := marshalAuthIdentityFixture(t, simfile.FCI{FileStructure: simfile.StructureLinearFixed, FileType: simfile.FileTypeWorkingEF, RecordSize: 17, RecordCount: byte(len(apps)), FileSize: uint16(17 * len(apps))})
	f := &authIdentityTransmitter{t: t, steps: []authIdentityStep{
		{request: authIdentityHex(t, "00A40004023F00"), response: master},
		{request: authIdentityHex(t, "00A40004022F00"), response: directory},
	}}
	for i, app := range apps {
		f.steps = append(f.steps, authIdentityStep{request: authIdentityHex(t, fmt.Sprintf("00B2%02X0411", i+1)), response: marshalAuthIdentityFixture(t, app)})
	}
	if identity && !onlyISIM {
		iccid := marshalAuthIdentityFixture(t, simfile.ICCID(expected.ICCID))
		imsi := marshalAuthIdentityFixture(t, simfile.IMSI{Digits: expected.IMSI})
		iccidFCI := marshalAuthIdentityFixture(t, simfile.FCI{FileStructure: simfile.StructureTransparent, FileType: simfile.FileTypeWorkingEF, FileSize: uint16(len(iccid))})
		imsiFCI := marshalAuthIdentityFixture(t, simfile.FCI{FileStructure: simfile.StructureTransparent, FileType: simfile.FileTypeWorkingEF, FileSize: uint16(len(imsi))})
		f.steps = append(f.steps,
			authIdentityStep{request: authIdentityHex(t, "00A40004023F00"), response: master},
			authIdentityStep{request: authIdentityHex(t, "00A40004022FE2"), response: iccidFCI},
			authIdentityStep{request: authIdentityHex(t, fmt.Sprintf("00B00000%02X", len(iccid))), response: iccid},
			authIdentityStep{request: authIdentityHex(t, "00A4040407A0000000871002"), response: master},
			authIdentityStep{request: authIdentityHex(t, "00A40004026F07"), response: imsiFCI},
			authIdentityStep{request: authIdentityHex(t, fmt.Sprintf("00B00000%02X", len(imsi))), response: imsi},
		)
	}
	authAID := usimAID
	if isim {
		authAID = isimAID
	}
	f.steps = append(f.steps,
		authIdentityStep{request: marshalAuthIdentityFixture(t, usimcommand.SelectName{Name: authAID}), response: master},
		authIdentityStep{request: marshalAuthIdentityFixture(t, usimcommand.Authenticate3G{Rand: bytes.Repeat([]byte{1}, 16), AUTN: bytes.Repeat([]byte{2}, 16)}), response: marshalAuthIdentityFixture(t, usimcommand.Authenticate3GResult{RES: bytes.Repeat([]byte{3}, 8), CK: bytes.Repeat([]byte{4}, 16), IK: bytes.Repeat([]byte{5}, 16)})},
	)
	return f, expected
}

func runAuthIdentityFixture(f *authIdentityTransmitter, expected *usimInfo, isim bool) (*akaResult, error) {
	prefix, name := usimApplicationAIDPrefix, "USIM"
	if isim {
		prefix, name = isimApplicationAIDPrefix, "ISIM"
	}
	return authenticateReaderOnHandleIdentity(context.Background(), f, bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16), prefix, name, expected)
}

func TestMultiAuthIdentityUsesOnlyIdentityFilesOnSameHandle(t *testing.T) {
	for _, isim := range []bool{false, true} {
		t.Run(fmt.Sprintf("isim_%t", isim), func(t *testing.T) {
			f, expected := newAuthIdentityFixture(t, true, isim, false)
			result, err := runAuthIdentityFixture(f, expected, isim)
			if err != nil || result == nil || !bytes.Equal(result.RES, bytes.Repeat([]byte{3}, 8)) {
				t.Fatalf("authentication failed: %v", err)
			}
			if f.next != len(f.steps) || f.authCalls != 1 || f.closeCalls != 0 {
				t.Fatalf("steps=%d/%d auth=%d close=%d", f.next, len(f.steps), f.authCalls, f.closeCalls)
			}
		})
	}
}

func TestMultiAuthIdentityFailsClosedBeforeAuthenticate(t *testing.T) {
	readErr := errors.New("synthetic card read failed")
	for _, name := range []string{"wrong_iccid", "wrong_imsi", "empty_expected_imsi", "iccid_read_failure", "imsi_read_failure", "malformed_iccid", "malformed_imsi", "missing_usim"} {
		t.Run(name, func(t *testing.T) {
			f, expected := newAuthIdentityFixture(t, true, true, name == "missing_usim")
			wantSteps := 10 // EF_DIR and both identity files, never AUTHENTICATE.
			switch name {
			case "wrong_iccid":
				expected.ICCID = "8900000000000000002"
			case "wrong_imsi":
				expected.IMSI = "001010123456780"
			case "empty_expected_imsi":
				expected.IMSI = ""
			case "iccid_read_failure":
				f.steps[6].err, wantSteps = readErr, 7
			case "imsi_read_failure":
				f.steps[9].err = readErr
			case "malformed_iccid":
				f.steps[6].response, wantSteps = []byte{0xAA}, 7
			case "malformed_imsi":
				f.steps[9].response = []byte{0x08, 0x09}
			case "missing_usim":
				wantSteps = 3
			}
			result, err := runAuthIdentityFixture(f, expected, true)
			if err == nil || result != nil || f.authCalls != 0 || f.next != wantSteps {
				t.Fatalf("expected closed rejection: error=%v result=%t auth=%d steps=%d/%d", err, result != nil, f.authCalls, f.next, wantSteps)
			}
			if (name == "iccid_read_failure" || name == "imsi_read_failure") && !errors.Is(err, readErr) {
				t.Fatalf("lost read error: %v", err)
			}
		})
	}
}

func TestLegacyAuthWithoutExpectedIdentityKeepsOriginalAPDUSequence(t *testing.T) {
	for _, isim := range []bool{false, true} {
		t.Run(fmt.Sprintf("isim_%t", isim), func(t *testing.T) {
			f, _ := newAuthIdentityFixture(t, false, isim, false)
			result, err := runAuthIdentityFixture(f, nil, isim)
			if err != nil || result == nil || f.next != len(f.steps) || f.authCalls != 1 {
				t.Fatalf("legacy sequence changed: error=%v auth=%d steps=%d/%d", err, f.authCalls, f.next, len(f.steps))
			}
		})
	}
}
