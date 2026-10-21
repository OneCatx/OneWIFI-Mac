//go:build darwin && cgo

package qmi

import "testing"

func TestQMIInterfaceCandidateForDJIQDC507(t *testing.T) {
	tests := []struct {
		name                       string
		class, subclass, protocol  byte
		interrupt, bulkIn, bulkOut bool
		want                       bool
	}{
		{name: "QMI signature with interrupt", class: 0xff, subclass: 0xff, protocol: 0xff, interrupt: true, bulkIn: true, bulkOut: true, want: true},
		{name: "diagnostic bulk pair only", class: 0xff, subclass: 0xff, protocol: 0xff, bulkIn: true, bulkOut: true},
		{name: "AT interface", class: 0xff, subclass: 0x00, protocol: 0x00, interrupt: true, bulkIn: true, bulkOut: true},
		{name: "ADB interface", class: 0xff, subclass: 0x42, protocol: 0x01, bulkIn: true, bulkOut: true},
		{name: "incomplete QMI endpoints", class: 0xff, subclass: 0xff, protocol: 0xff, interrupt: true, bulkIn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := qmiInterfaceCandidate(qmiLibusbVendorDJI, qmiLibusbProductDJI,
				tt.class, tt.subclass, tt.protocol, tt.interrupt, tt.bulkIn, tt.bulkOut)
			if got != tt.want {
				t.Fatalf("candidate = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestQMIInterfaceCandidateForGenericQuectel(t *testing.T) {
	if !qmiInterfaceCandidate(qmiLibusbVendorQC, qmiLibusbProductQC, 0xff, 0, 0, false, true, true) {
		t.Fatal("generic Quectel QMI bulk interface should remain eligible")
	}
}
