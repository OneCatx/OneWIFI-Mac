package estkble

import (
	"bytes"
	"testing"
)

func TestAPDURequestBoundary(t *testing.T) {
	for _, length := range []int{0, 20, 255, 256, 65535} {
		payload := bytes.Repeat([]byte{0xab}, length)
		frame, err := APDURequest(payload)
		if err != nil || len(frame) != length+3 || frame[0] != 4 || int(frame[1])+int(frame[2])*256 != length || !bytes.Equal(frame[3:], payload) {
			t.Fatalf("length %d: frame=%v err=%v", length, frame[:min(len(frame), 4)], err)
		}
	}
	if _, err := APDURequest(make([]byte, 65536)); err == nil {
		t.Fatal("oversized APDU accepted")
	}
}

func TestReplyFragments(t *testing.T) {
	var reply Reply
	for _, part := range [][]byte{{4, 5, 0, 1, 2}, {3, 4}, {5}} {
		complete, err := reply.Push(part)
		if err != nil {
			t.Fatal(err)
		}
		if complete != (part[0] == 5) {
			t.Fatalf("premature completion: %v", complete)
		}
	}
	if !bytes.Equal(reply.Bytes(), []byte{1, 2, 3, 4, 5}) {
		t.Fatal("invalid reassembly")
	}
}

func TestReplyRejectsMalformed(t *testing.T) {
	for _, parts := range [][][]byte{{{4, 1}}, {{4, 1, 0, 1, 2}}} {
		var reply Reply
		if _, err := reply.Push(parts[0]); err == nil {
			t.Fatalf("accepted %v", parts)
		}
	}
}
