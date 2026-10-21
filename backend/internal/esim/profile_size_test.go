package esim

import (
	"testing"

	sgp22 "github.com/damonto/euicc-go/v2"
)

func TestEstimateProfileDownloadSizeUsesSMdpEstimateFirst(t *testing.T) {
	metadata := &sgp22.ProfileInfo{EstimatedProfileSize: 123456}
	if got := estimateProfileDownloadSize("unknown.example", "", metadata); got != 123456 {
		t.Fatalf("estimated size=%d want SM-DP+ estimate 123456", got)
	}
}

func TestEstimateProfileDownloadSizeUsesNekokoReferenceFallback(t *testing.T) {
	metadata := &sgp22.ProfileInfo{
		ServiceProviderName: "COSMOTE",
		ProfileOwner:        sgp22.OperatorId{PLMN: []byte{0x02, 0xF2, 0x10}},
	}
	got := estimateProfileDownloadSize(
		"https://rsp-0003.oberthur.net/",
		"89044045930000000000001714825038",
		metadata,
	)
	if got != 39008 {
		t.Fatalf("estimated size=%d want 39008 from Nekoko reference table", got)
	}
}

func TestEstimateProfileDownloadSizeDoesNotGuessUnknownSMdp(t *testing.T) {
	metadata := &sgp22.ProfileInfo{ServiceProviderName: "Unknown"}
	if got := estimateProfileDownloadSize("unlisted.example", "", metadata); got != 0 {
		t.Fatalf("estimated size=%d want 0 when reference data has no match", got)
	}
}
