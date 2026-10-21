package imscore

import "testing"

func TestNormalizeAssociatedIdentity(t *testing.T) {
	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{"<tel:+44 7700-900123>", "+447700900123", true},
		{"<sip:+447700900123@ims.example;user=phone>", "+447700900123", true},
		{"tel:%2B447700900123", "+447700900123", true},
		{"sip:234150000000000@ims.example", "", false},
		{"+447700900123", "", false},
	}
	for _, test := range tests {
		got, ok := normalizeAssociatedIdentity(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("normalizeAssociatedIdentity(%q) = (%q, %v), want (%q, %v)",
				test.value, got, ok, test.want, test.ok)
		}
	}
}

func TestSplitAssociatedHeaderValues(t *testing.T) {
	got := splitAssociatedHeaderValues(
		`<sip:234150000000000@ims.example>, "phone" <tel:+447700900123>`)
	if len(got) != 2 {
		t.Fatalf("splitAssociatedHeaderValues() = %#v, want 2 values", got)
	}
}
