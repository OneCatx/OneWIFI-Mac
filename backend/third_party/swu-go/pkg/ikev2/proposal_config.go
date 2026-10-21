package ikev2

import (
	"fmt"
	"strings"
)

// BuildIKEProposals parses strongSwan-style proposal strings. A non-empty
// configured list is authoritative: it is never merged with the defaults.
// Empty configuration retains the built-in compatibility proposal set.
func BuildIKEProposals(configured []string, spi []byte) ([]*Proposal, error) {
	if len(configured) == 0 {
		return CreateMultiProposalIKE(spi), nil
	}

	out := make([]*Proposal, 0, len(configured))
	for i, raw := range configured {
		spec, err := parseProposalSpec(raw, ProtoIKE)
		if err != nil {
			return nil, fmt.Errorf("IKE proposal %d %q: %w", i+1, raw, err)
		}
		out = append(out, spec.proposal(uint8(i+1), ProtoIKE, spi))
	}
	return out, nil
}

// BuildESPProposals parses strongSwan-style ESP proposal strings. As with
// IKE, a non-empty list replaces the complete default list and preserves the
// configured order.
func BuildESPProposals(configured []string, spi []byte) ([]*Proposal, error) {
	if len(configured) == 0 {
		return CreateMultiProposalESP(spi), nil
	}

	out := make([]*Proposal, 0, len(configured))
	for i, raw := range configured {
		spec, err := parseProposalSpec(raw, ProtoESP)
		if err != nil {
			return nil, fmt.Errorf("ESP proposal %d %q: %w", i+1, raw, err)
		}
		out = append(out, spec.proposal(uint8(i+1), ProtoESP, spi))
	}
	return out, nil
}

// ProposalDHGroup returns the single DH transform selected for an IKE
// proposal. The initiator uses this value for its accompanying KE payload.
func ProposalDHGroup(proposal *Proposal) (AlgorithmType, error) {
	if proposal == nil {
		return 0, fmt.Errorf("proposal is nil")
	}
	var group AlgorithmType
	for _, transform := range proposal.Transforms {
		if transform.Type != TransformTypeDH {
			continue
		}
		if group != 0 {
			return 0, fmt.Errorf("proposal contains multiple DH transforms")
		}
		group = transform.ID
	}
	if group == 0 {
		return 0, fmt.Errorf("proposal does not contain a DH transform")
	}
	return group, nil
}

type configuredProposal struct {
	encryption    AlgorithmType
	encryptionKey int
	aead          bool
	integrity     AlgorithmType
	prf           AlgorithmType
	dh            AlgorithmType
	esn           AlgorithmType
	hasESN        bool
}

func (s configuredProposal) proposal(number uint8, protocol ProtocolID, spi []byte) *Proposal {
	p := NewProposal(number, protocol, append([]byte(nil), spi...))
	p.AddTransformWithKeyLen(TransformTypeEncr, s.encryption, s.encryptionKey)
	if !s.aead {
		p.AddTransform(TransformTypeInteg, s.integrity, 0)
	}
	if protocol == ProtoIKE {
		p.AddTransform(TransformTypePRF, s.prf, 0)
		p.AddTransform(TransformTypeDH, s.dh, 0)
	} else {
		esn := AlgorithmType(0)
		if s.hasESN {
			esn = s.esn
		}
		p.AddTransform(TransformTypeESN, esn, 0)
	}
	return p
}

func parseProposalSpec(raw string, protocol ProtocolID) (configuredProposal, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return configuredProposal{}, fmt.Errorf("proposal must not be empty")
	}

	var out configuredProposal
	var hasEncryption, hasIntegrity, hasPRF, hasDH bool
	for _, token := range strings.Split(raw, "-") {
		token = strings.TrimSpace(token)
		if token == "" {
			return configuredProposal{}, fmt.Errorf("empty algorithm token")
		}

		if id, keyLen, aead, ok := parseEncryptionToken(token); ok {
			if hasEncryption {
				return configuredProposal{}, fmt.Errorf("multiple encryption algorithms")
			}
			out.encryption, out.encryptionKey, out.aead = id, keyLen, aead
			hasEncryption = true
			continue
		}
		if id, ok := parseIntegrityToken(token); ok {
			if hasIntegrity {
				return configuredProposal{}, fmt.Errorf("multiple integrity algorithms")
			}
			out.integrity, hasIntegrity = id, true
			continue
		}
		if id, ok := parsePRFToken(token); ok {
			if protocol != ProtoIKE {
				return configuredProposal{}, fmt.Errorf("PRF is not valid in an ESP proposal")
			}
			if hasPRF {
				return configuredProposal{}, fmt.Errorf("multiple PRF algorithms")
			}
			out.prf, hasPRF = id, true
			continue
		}
		if id, known, supported := parseDHToken(token); known {
			if protocol != ProtoIKE {
				return configuredProposal{}, fmt.Errorf("DH group is not valid in an initial ESP proposal")
			}
			if !supported {
				return configuredProposal{}, fmt.Errorf("DH group %q is known but not supported by this runtime", token)
			}
			if hasDH {
				return configuredProposal{}, fmt.Errorf("multiple DH groups")
			}
			out.dh, hasDH = id, true
			continue
		}
		if token == "esn" || token == "noesn" {
			if protocol != ProtoESP {
				return configuredProposal{}, fmt.Errorf("%s is only valid in an ESP proposal", token)
			}
			if out.hasESN {
				return configuredProposal{}, fmt.Errorf("multiple ESN selections")
			}
			out.hasESN = true
			if token == "esn" {
				out.esn = 1
			}
			continue
		}
		return configuredProposal{}, fmt.Errorf("unknown or unsupported algorithm token %q", token)
	}

	if !hasEncryption {
		return configuredProposal{}, fmt.Errorf("missing encryption algorithm")
	}
	if out.aead && hasIntegrity {
		return configuredProposal{}, fmt.Errorf("AEAD encryption must not include a separate integrity algorithm")
	}
	if !out.aead && !hasIntegrity {
		return configuredProposal{}, fmt.Errorf("non-AEAD encryption requires an integrity algorithm")
	}

	if protocol == ProtoIKE {
		if !hasDH {
			return configuredProposal{}, fmt.Errorf("IKE proposal requires a DH group")
		}
		if !hasPRF {
			if !hasIntegrity {
				return configuredProposal{}, fmt.Errorf("AEAD IKE proposal requires an explicit PRF")
			}
			out.prf = prfForIntegrity(out.integrity)
			if out.prf == 0 {
				return configuredProposal{}, fmt.Errorf("no supported implicit PRF for integrity algorithm")
			}
		}
	}

	return out, nil
}

func parseEncryptionToken(token string) (id AlgorithmType, keyLen int, aead bool, ok bool) {
	switch token {
	case "aes128":
		return ENCR_AES_CBC, 128, false, true
	case "aes192":
		return ENCR_AES_CBC, 192, false, true
	case "aes256":
		return ENCR_AES_CBC, 256, false, true
	}

	for _, item := range []struct {
		name string
		id   AlgorithmType
	}{
		{name: "gcm8", id: ENCR_AES_GCM_8},
		{name: "gcm12", id: ENCR_AES_GCM_12},
		{name: "gcm16", id: ENCR_AES_GCM_16},
	} {
		for _, bits := range []int{128, 192, 256} {
			if token == fmt.Sprintf("aes%d%s", bits, item.name) {
				return item.id, bits, true, true
			}
		}
	}
	return 0, 0, false, false
}

func parseIntegrityToken(token string) (AlgorithmType, bool) {
	switch token {
	case "sha1":
		return AUTH_HMAC_SHA1_96, true
	case "sha256", "sha2_256":
		return AUTH_HMAC_SHA2_256_128, true
	case "sha384", "sha2_384":
		return AUTH_HMAC_SHA2_384_192, true
	case "sha512", "sha2_512":
		return AUTH_HMAC_SHA2_512_256, true
	default:
		return 0, false
	}
}

func parsePRFToken(token string) (AlgorithmType, bool) {
	switch token {
	case "prfmd5":
		return PRF_HMAC_MD5, true
	case "prfsha1":
		return PRF_HMAC_SHA1, true
	case "prfsha256", "prfsha2_256":
		return PRF_HMAC_SHA2_256, true
	case "prfsha384", "prfsha2_384":
		return PRF_HMAC_SHA2_384, true
	case "prfsha512", "prfsha2_512":
		return PRF_HMAC_SHA2_512, true
	default:
		return 0, false
	}
}

func parseDHToken(token string) (id AlgorithmType, known bool, supported bool) {
	switch token {
	case "modp768":
		return MODP_768_bit, true, false
	case "modp1024":
		return MODP_1024_bit, true, false
	case "modp1536":
		return MODP_1536_bit, true, false
	case "modp2048":
		return MODP_2048_bit, true, true
	case "modp3072":
		return MODP_3072_bit, true, false
	case "modp4096":
		return MODP_4096_bit, true, false
	case "modp6144":
		return MODP_6144_bit, true, false
	case "modp8192":
		return MODP_8192_bit, true, false
	default:
		return 0, false, false
	}
}

func prfForIntegrity(integrity AlgorithmType) AlgorithmType {
	switch integrity {
	case AUTH_HMAC_SHA1_96:
		return PRF_HMAC_SHA1
	case AUTH_HMAC_SHA2_256_128:
		return PRF_HMAC_SHA2_256
	case AUTH_HMAC_SHA2_384_192:
		return PRF_HMAC_SHA2_384
	case AUTH_HMAC_SHA2_512_256:
		return PRF_HMAC_SHA2_512
	default:
		return 0
	}
}
