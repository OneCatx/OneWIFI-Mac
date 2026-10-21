package esim

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	sgp22 "github.com/damonto/euicc-go/v2"
)

// NekokoLPA2 uses this public reference table only when the SM-DP+ response
// omits its optional estimatedProfileSize field.
//
//go:embed data/reference_sizes_simple.json
var profileSizeReferenceJSON []byte

type profileSizeReferenceDatabase struct {
	ReferenceEUM string                     `json:"reference_eum"`
	Results      []profileSizeReferenceItem `json:"results"`
}

type profileSizeReferenceItem struct {
	PLMN                string           `json:"plmn"`
	ServiceProviderName string           `json:"serviceProviderName"`
	RSP                 string           `json:"rsp"`
	ReferenceSize       int64            `json:"reference_size"`
	EUMSizes            map[string]int64 `json:"eum_sizes"`
}

var (
	profileSizeReferenceOnce sync.Once
	profileSizeReferenceData profileSizeReferenceDatabase
	profileSizeReferenceErr  error
)

func estimateProfileDownloadSize(smdp, eid string, metadata *sgp22.ProfileInfo) int64 {
	if metadata == nil {
		return 0
	}
	if metadata.EstimatedProfileSize > 0 {
		return metadata.EstimatedProfileSize
	}
	profileSizeReferenceOnce.Do(func() {
		profileSizeReferenceErr = json.Unmarshal(profileSizeReferenceJSON, &profileSizeReferenceData)
	})
	if profileSizeReferenceErr != nil {
		return 0
	}

	smdp = normalizeProfileSizeRSP(smdp)
	spn := strings.TrimSpace(metadata.ServiceProviderName)
	plmn := strings.TrimSpace(metadata.ProfileOwner.MCC() + metadata.ProfileOwner.MNC())
	if !allDecimalDigits(plmn) || (len(plmn) != 5 && len(plmn) != 6) {
		plmn = ""
	}

	var best *profileSizeReferenceItem
	bestScore := 0
	for index := range profileSizeReferenceData.Results {
		item := &profileSizeReferenceData.Results[index]
		rsp := normalizeProfileSizeRSP(item.RSP)
		if smdp == "" || rsp != smdp {
			continue
		}
		score := 1
		if plmn != "" && item.PLMN == plmn {
			score += 2
		}
		if spn != "" && item.ServiceProviderName != "" {
			want := strings.ToLower(strings.TrimSpace(item.ServiceProviderName))
			got := strings.ToLower(spn)
			if got == want || strings.Contains(got, want) {
				score += 3
			}
		}
		if score > bestScore {
			best = item
			bestScore = score
		}
	}
	if best == nil || best.ReferenceSize <= 0 {
		return 0
	}

	eid = strings.TrimSpace(eid)
	if len(eid) >= 8 {
		if size := best.EUMSizes[eid[:8]]; size > 0 {
			return size
		}
		if strings.EqualFold(eid[:8], profileSizeReferenceData.ReferenceEUM) {
			return best.ReferenceSize
		}
	}
	return best.ReferenceSize
}

func (m *Manager) profileSizeEstimate(smdp string, aid []byte, metadata *sgp22.ProfileInfo) int64 {
	eid := ""
	if m != nil {
		m.cacheMu.RLock()
		if m.chipInfoCache != nil {
			for _, candidate := range m.chipInfoCache.EIDs {
				if len(aid) > 0 && strings.EqualFold(candidate.AIDHex, fmt.Sprintf("%X", aid)) {
					eid = candidate.EID
					break
				}
			}
			if eid == "" && len(m.chipInfoCache.EIDs) > 0 {
				eid = m.chipInfoCache.EIDs[0].EID
			}
		}
		m.cacheMu.RUnlock()
	}
	return estimateProfileDownloadSize(smdp, eid, metadata)
}

func normalizeProfileSizeRSP(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	return strings.TrimSuffix(strings.Trim(value, "/"), "/")
}

func allDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
