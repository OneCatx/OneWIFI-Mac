//! Deterministic IMS audio SDP negotiation.
//!
//! PCMA is preferred whenever the peer offers it. AMR-NB mode 7 is retained
//! as a carrier fallback because O2/giffgaff incoming calls have been observed
//! to offer only bandwidth-efficient AMR.
//!
//! ## Staged Go compatibility contract
//!
//! Until this parser becomes the live owner, its lexical behavior is kept
//! deliberately compatible with Go `callRuntime.applyRemoteSDP`: canonical
//! `m=audio` / `RTP/AVP` tokens, exact `c=IN IP4|IP6 ` prefixes with exactly
//! three `Fields` tokens, raw payload-token map keys, Unicode whitespace, and
//! `strconv.Atoi`-equivalent port/payload/AMR mode-set matching.  The stateful bodyless
//! final-response / early-media rule remains outside this stateless parser in
//! the Go call runtime.

use std::{collections::HashMap, fmt, net::IpAddr};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AudioCodec {
    Pcma,
    AmrNbMode7 { octet_aligned: bool },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AudioNegotiation {
    pub remote_ip: IpAddr,
    pub remote_port: u16,
    pub payload_type: u8,
    pub codec: AudioCodec,
    pub telephone_event_payload: Option<u8>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SdpError {
    MissingAudio,
    InvalidAudioEndpoint,
    UnsupportedAudioCodec,
}

impl fmt::Display for SdpError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingAudio => formatter.write_str("remote SDP has no usable audio section"),
            Self::InvalidAudioEndpoint => {
                formatter.write_str("remote SDP has an invalid numeric audio endpoint")
            }
            Self::UnsupportedAudioCodec => {
                formatter.write_str("remote SDP offers neither PCMA nor AMR-NB mode 7")
            }
        }
    }
}

impl std::error::Error for SdpError {}

pub fn negotiate_audio(body: &str, answer: bool) -> Result<AudioNegotiation, SdpError> {
    let mut session_ip = None;
    let mut media_ip = None;
    let mut audio_port = None;
    // Keep payload tokens raw as Go does.  For example, `08` and `8` both
    // parse as payload type 8 but deliberately have distinct rtpmap/fmtp map
    // keys until the selected payload is converted to a numeric value.
    let mut offered = Vec::<String>::new();
    let mut rtpmap = HashMap::<String, String>::new();
    let mut fmtp = HashMap::<String, String>::new();
    let mut media_section = false;
    let mut audio_section = false;

    for raw in body.lines() {
        let line = raw.trim_end_matches('\r').trim();
        if line.starts_with("m=") {
            media_section = true;
            audio_section = false;
            let fields = line.split_whitespace().collect::<Vec<_>>();
            if audio_port.is_none()
                && fields.len() > 3
                // Keep the staged parser byte-for-byte compatible with the
                // production Go call path.  In particular, Go accepts only
                // the canonical SDP media token and transport spelling here;
                // accepting a relaxed form in Rust would change live call
                // admission when this parser becomes the owner.
                && fields[0] == "m=audio"
                && fields[2] == "RTP/AVP"
                && let Some(port) = parse_go_port(fields[1])
            {
                audio_port = Some(port);
                audio_section = true;
                offered = fields[3..].iter().map(ToString::to_string).collect();
            }
            continue;
        }
        if line.starts_with("c=IN IP4 ") || line.starts_with("c=IN IP6 ") {
            let fields = line.split_whitespace().collect::<Vec<_>>();
            if fields.len() == 3 {
                let parsed = fields[2].parse::<IpAddr>().ok();
                if media_section && audio_section {
                    if parsed.is_none() {
                        return Err(SdpError::InvalidAudioEndpoint);
                    }
                    media_ip = parsed;
                } else if !media_section {
                    session_ip = parsed;
                }
            }
        }
        if !audio_section {
            continue;
        }
        if let Some(value) = strip_prefix_ascii_case(line, "a=rtpmap:") {
            let mut fields = value.split_whitespace();
            if let (Some(payload), Some(mapping)) = (fields.next(), fields.next()) {
                rtpmap.insert(payload.to_owned(), mapping.to_ascii_uppercase());
            }
        } else if let Some(value) = strip_prefix_ascii_case(line, "a=fmtp:") {
            // Go first applies strings.TrimSpace to the whole suffix, then
            // splits at the first literal ASCII space.  Do not trim either
            // split half afterwards: the payload token remains a lexical map
            // key, including any non-space whitespace immediately before the
            // delimiter.
            let value = value.trim();
            if let Some((payload, parameters)) = value.split_once(' ') {
                fmtp.insert(payload.to_owned(), parameters.to_ascii_lowercase());
            }
        }
    }

    let remote_port = audio_port.ok_or(SdpError::MissingAudio)?;
    let remote_ip = media_ip
        .or(session_ip)
        .ok_or(SdpError::InvalidAudioEndpoint)?;

    let selected = offered.iter().find_map(|payload| {
        let parsed = parse_go_payload(payload)?;
        let mapping = rtpmap.get(payload).map(String::as_str).unwrap_or("");
        let pcma = matches!(mapping, "PCMA/8000" | "PCMA/8000/1");
        ((parsed == 8 && (mapping.is_empty() || pcma)) || (!answer && parsed >= 96 && pcma))
            .then_some((parsed, AudioCodec::Pcma))
    });
    let selected = selected.or_else(|| {
        offered.iter().find_map(|payload| {
            let parsed = parse_go_payload(payload)?;
            let mapping = rtpmap.get(payload).map(String::as_str).unwrap_or("");
            let parameters = fmtp.get(payload).map(String::as_str).unwrap_or("");
            (parsed >= 96
                && mapping == "AMR/8000"
                && parameter_list_contains_integer(parameters, "mode-set", 7))
            .then_some((
                parsed,
                AudioCodec::AmrNbMode7 {
                    octet_aligned: parameter_equals(parameters, "octet-align", "1"),
                },
            ))
        })
    });
    let (payload_type, codec) = selected.ok_or(SdpError::UnsupportedAudioCodec)?;

    let telephone_event_payload = offered.iter().find_map(|payload| {
        let parsed = parse_go_payload(payload)?;
        let mapping = rtpmap.get(payload).map(String::as_str).unwrap_or("");
        (parsed >= 96
            && parsed != payload_type
            && matches!(mapping, "TELEPHONE-EVENT/8000" | "TELEPHONE-EVENT/8000/1")
            && (!answer || parsed == 101))
            .then_some(parsed)
    });

    Ok(AudioNegotiation {
        remote_ip,
        remote_port,
        payload_type,
        codec,
        telephone_event_payload,
    })
}

/// Parses an offered payload exactly as Go's `strconv.Atoi` followed by the
/// `0..=127` RTP payload range check.  The raw lexical token intentionally
/// remains the map key above.
fn parse_go_payload(value: &str) -> Option<u8> {
    value
        .parse::<i32>()
        .ok()
        .filter(|value| (0..=127).contains(value))
        .map(|value| value as u8)
}

/// Parses an SDP media port with Go's `strconv.Atoi` semantics, then applies
/// the same valid RTP port range as the live Go route.
fn parse_go_port(value: &str) -> Option<u16> {
    value
        .parse::<i32>()
        .ok()
        .filter(|value| (1..=65_535).contains(value))
        .map(|value| value as u16)
}

fn strip_prefix_ascii_case<'a>(value: &'a str, prefix: &str) -> Option<&'a str> {
    value
        .get(..prefix.len())
        .filter(|candidate| candidate.eq_ignore_ascii_case(prefix))
        .and_then(|_| value.get(prefix.len()..))
}

fn parameter_equals(parameters: &str, name: &str, expected: &str) -> bool {
    parameters.split(';').any(|field| {
        field.split_once('=').is_some_and(|(key, value)| {
            key.trim().eq_ignore_ascii_case(name) && value.trim() == expected
        })
    })
}

/// Matches Go's `strconv.Atoi(strings.TrimSpace(mode)) == wanted` behavior
/// for AMR `mode-set` values.  This intentionally accepts canonical integer
/// spellings such as `7`, `07`, and `+7`, while rejecting non-integers.
fn parameter_list_contains_integer(parameters: &str, name: &str, wanted: i32) -> bool {
    parameters.split(';').any(|field| {
        field.split_once('=').is_some_and(|(key, value)| {
            key.trim().eq_ignore_ascii_case(name)
                && value
                    .split(',')
                    .any(|item| item.trim().parse::<i32>() == Ok(wanted))
        })
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prefers_pcma_and_keeps_dynamic_dtmf() {
        let offer = "v=0\r\nc=IN IP4 192.0.2.20\r\nm=audio 49170 RTP/AVP 97 8 110\r\na=rtpmap:97 AMR/8000\r\na=fmtp:97 mode-set=7\r\na=rtpmap:110 telephone-event/8000\r\na=sendrecv\r\n";
        assert_eq!(
            negotiate_audio(offer, false).unwrap(),
            AudioNegotiation {
                remote_ip: "192.0.2.20".parse().unwrap(),
                remote_port: 49170,
                payload_type: 8,
                codec: AudioCodec::Pcma,
                telephone_event_payload: Some(110),
            }
        );
    }

    #[test]
    fn accepts_real_giffgaff_bandwidth_efficient_amr_shape() {
        let offer = "v=0\r\no=- 1 1 IN IP6 2001:db8::10\r\ns=-\r\nc=IN IP6 2001:db8::20\r\nt=0 0\r\nm=audio 40000 RTP/AVP 96 101\r\na=rtpmap:96 AMR/8000\r\na=fmtp:96 mode-set=0,2,4,7\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-15\r\na=sendrecv\r\n";
        let result = negotiate_audio(offer, false).unwrap();
        assert_eq!(result.remote_ip, "2001:db8::20".parse::<IpAddr>().unwrap());
        assert_eq!(result.payload_type, 96);
        assert_eq!(
            result.codec,
            AudioCodec::AmrNbMode7 {
                octet_aligned: false
            }
        );
        assert_eq!(result.telephone_event_payload, Some(101));
    }

    #[test]
    fn rejects_amr_without_mode_7_and_hostnames() {
        let unsupported = "v=0\r\nc=IN IP4 example.invalid\r\nm=audio 40000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\na=fmtp:96 mode-set=0,2,4\r\n";
        assert_eq!(
            negotiate_audio(unsupported, false),
            Err(SdpError::InvalidAudioEndpoint)
        );
        let unsupported = "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 40000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\na=fmtp:96 mode-set=0,2,4\r\n";
        assert_eq!(
            negotiate_audio(unsupported, false),
            Err(SdpError::UnsupportedAudioCodec)
        );
        let invalid_audio_override = "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 40000 RTP/AVP 8\r\nc=IN IP4 media.example.invalid\r\n";
        assert_eq!(
            negotiate_audio(invalid_audio_override, false),
            Err(SdpError::InvalidAudioEndpoint)
        );
    }

    #[test]
    fn matches_go_case_sensitive_audio_media_and_transport_tokens() {
        for malformed_media in [
            "m=Audio 40000 RTP/AVP 8",
            "m=audio 40000 rtp/avp 8",
            "m=AUDIO 40000 RTP/AVP 8",
            "m= audio 40000 RTP/AVP 8",
        ] {
            let sdp = format!("v=0\r\nc=IN IP4 192.0.2.30\r\n{malformed_media}\r\n");
            assert_eq!(
                negotiate_audio(&sdp, false),
                Err(SdpError::MissingAudio),
                "{malformed_media} must retain Go's exact-token behavior"
            );
        }
    }

    #[test]
    fn matches_go_atoi_mode_set_spellings() {
        for modes in ["7", "07", "+7", "  +7 ,  07 "] {
            let sdp = format!(
                "v=0\r\nc=IN IP4 192.0.2.31\r\nm=audio 40000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\na=fmtp:96 mode-set={modes}\r\n"
            );
            assert_eq!(
                negotiate_audio(&sdp, false).unwrap().codec,
                AudioCodec::AmrNbMode7 {
                    octet_aligned: false
                },
                "mode-set={modes} must match Go strconv.Atoi semantics"
            );
        }

        let non_integer = "v=0\r\nc=IN IP4 192.0.2.31\r\nm=audio 40000 RTP/AVP 96\r\na=rtpmap:96 AMR/8000\r\na=fmtp:96 mode-set=7.0\r\n";
        assert_eq!(
            negotiate_audio(non_integer, false),
            Err(SdpError::UnsupportedAudioCodec)
        );
    }

    #[test]
    fn matches_go_connection_field_shape_and_unicode_fields() {
        let extra_media_fields = "v=0\r\nc=IN IP4 192.0.2.40\r\nm=audio 40000 RTP/AVP 8\r\nc=IN IP4 192.0.2.41 ignored\r\n";
        assert_eq!(
            negotiate_audio(extra_media_fields, false)
                .unwrap()
                .remote_ip,
            "192.0.2.40".parse::<IpAddr>().unwrap(),
            "a c= line with more than three Fields must be ignored like Go"
        );

        let noncanonical_connection_spacing =
            "v=0\r\nc=IN  IP4 192.0.2.40\r\nm=audio 40000 RTP/AVP 8\r\n";
        assert_eq!(
            negotiate_audio(noncanonical_connection_spacing, false),
            Err(SdpError::InvalidAudioEndpoint),
            "Go only recognizes the exact c=IN IP4|IP6 prefix"
        );

        let unicode_fields =
            "v=0\r\nc=IN IP4 192.0.2.42\r\nm=audio\u{00a0}40000\u{00a0}RTP/AVP\u{00a0}8\r\n";
        assert_eq!(
            negotiate_audio(unicode_fields, false).unwrap().payload_type,
            8,
            "Rust must use Go strings.Fields-compatible Unicode whitespace"
        );

        let plus_prefixed_port = "v=0\r\nc=IN IP4 192.0.2.43\r\nm=audio +40000 RTP/AVP 8\r\n";
        assert_eq!(
            negotiate_audio(plus_prefixed_port, false)
                .unwrap()
                .remote_port,
            40000,
            "Go strconv.Atoi accepts a leading plus on the RTP port"
        );
    }

    #[test]
    fn keeps_payload_attribute_map_keys_lexically_raw() {
        let dynamic_pcma_key_mismatch =
            "v=0\r\nc=IN IP4 192.0.2.50\r\nm=audio 40000 RTP/AVP 097\r\na=rtpmap:97 PCMA/8000\r\n";
        assert_eq!(
            negotiate_audio(dynamic_pcma_key_mismatch, false),
            Err(SdpError::UnsupportedAudioCodec),
            "Go does not equate m= payload 097 with rtpmap key 97"
        );

        let dynamic_pcma_raw_match = "v=0\r\nc=IN IP4 192.0.2.50\r\nm=audio 40000 RTP/AVP +097\r\na=rtpmap:+097 PCMA/8000\r\n";
        let pcma = negotiate_audio(dynamic_pcma_raw_match, false).unwrap();
        assert_eq!(pcma.payload_type, 97);
        assert_eq!(pcma.codec, AudioCodec::Pcma);

        let amr_fmtp_key_mismatch = "v=0\r\nc=IN IP4 192.0.2.51\r\nm=audio 40000 RTP/AVP 097\r\na=rtpmap:097 AMR/8000\r\na=fmtp:97 mode-set=7\r\n";
        assert_eq!(
            negotiate_audio(amr_fmtp_key_mismatch, false),
            Err(SdpError::UnsupportedAudioCodec),
            "Go does not normalize fmtp map keys either"
        );

        let amr_fmtp_raw_match = "v=0\r\nc=IN IP4 192.0.2.51\r\nm=audio 40000 RTP/AVP 097\r\na=rtpmap:097 AMR/8000\r\na=fmtp:097 mode-set=07\r\n";
        assert_eq!(
            negotiate_audio(amr_fmtp_raw_match, false).unwrap().codec,
            AudioCodec::AmrNbMode7 {
                octet_aligned: false
            }
        );

        let fmtp_leading_whitespace = "v=0\r\nc=IN IP4 192.0.2.52\r\nm=audio 40000 RTP/AVP 097\r\na=rtpmap:097 AMR/8000\r\na=fmtp:\t 097 mode-set=7\r\n";
        assert_eq!(
            negotiate_audio(fmtp_leading_whitespace, false)
                .unwrap()
                .codec,
            AudioCodec::AmrNbMode7 {
                octet_aligned: false
            },
            "outer whitespace after a=fmtp: follows Go TrimSpace behavior"
        );

        let fmtp_tab_in_key = "v=0\r\nc=IN IP4 192.0.2.52\r\nm=audio 40000 RTP/AVP 097\r\na=rtpmap:097 AMR/8000\r\na=fmtp:097\t mode-set=7\r\n";
        assert_eq!(
            negotiate_audio(fmtp_tab_in_key, false),
            Err(SdpError::UnsupportedAudioCodec),
            "Go retains a tab before the literal fmtp delimiter in the map key"
        );
    }
}
