//! Ordered REGISTER serialization. Carrier policy supplies header values and
//! order; this layer owns framing and rejects injection/duplicate core fields.
use crate::{SipError, aka_authorization::AuthorizationHeader};
use zeroize::Zeroizing;

pub struct RegisterRequest<'a> {
    pub uri: &'a str,
    pub headers: &'a [(&'a str, &'a str)],
    pub body: &'a [u8],
}

impl RegisterRequest<'_> {
    pub fn encode(
        &self,
        authorization: Option<&AuthorizationHeader>,
    ) -> Result<Zeroizing<Vec<u8>>, SipError> {
        if !(self.uri.starts_with("sip:") || self.uri.starts_with("sips:"))
            || self.uri.len() > 4096
            || self
                .uri
                .bytes()
                .any(|b| b.is_ascii_whitespace() || b.is_ascii_control())
        {
            return Err(SipError::InvalidHeader);
        }
        if self.headers.len() > 120 || self.body.len() > 65_536 {
            return Err(SipError::MessageTooLarge);
        }
        let mut out = Zeroizing::new(format!("REGISTER {} SIP/2.0\r\n", self.uri).into_bytes());
        let mut core = [false; 7];
        for &(name, value) in self.headers {
            validate(name, value)?;
            let key = name.to_ascii_lowercase();
            if matches!(key.as_str(), "content-length" | "l") {
                return Err(SipError::DuplicateContentLength);
            }
            if authorization.is_some_and(|auth| name.eq_ignore_ascii_case(auth.name())) {
                return Err(SipError::InvalidHeader);
            }
            let index = match key.as_str() {
                "via" | "v" => Some(0),
                "from" | "f" => Some(1),
                "to" | "t" => Some(2),
                "call-id" | "i" => Some(3),
                "cseq" => Some(4),
                "max-forwards" => Some(5),
                "contact" | "m" => Some(6),
                _ => None,
            };
            if let Some(index) = index {
                if core[index] {
                    return Err(SipError::InvalidHeader);
                }
                core[index] = true;
            }
            append(&mut out, name, value);
            if out.len() > 65_536 {
                return Err(SipError::MessageTooLarge);
            }
        }
        if core.iter().any(|present| !present) {
            return Err(SipError::InvalidHeader);
        }
        if let Some(auth) = authorization {
            if !matches!(auth.name(), "Authorization" | "Proxy-Authorization") {
                return Err(SipError::InvalidHeader);
            }
            validate(auth.name(), auth.value())?;
            append(&mut out, auth.name(), auth.value());
        }
        append(&mut out, "Content-Length", &self.body.len().to_string());
        out.extend_from_slice(b"\r\n");
        out.extend_from_slice(self.body);
        // Use the same transaction validator as the actual sending path.
        crate::registration_connection::request_transaction(&out)?;
        Ok(out)
    }
}

fn validate(name: &str, value: &str) -> Result<(), SipError> {
    if name.is_empty()
        || !name.bytes().all(crate::is_header_name_byte)
        || value.trim().is_empty()
        || value.bytes().any(|b| b.is_ascii_control())
    {
        return Err(SipError::InvalidHeader);
    }
    if name.len() + value.len() + 2 > 8192 {
        return Err(SipError::HeaderLineTooLong);
    }
    Ok(())
}
fn append(out: &mut Vec<u8>, name: &str, value: &str) {
    out.extend_from_slice(name.as_bytes());
    out.extend_from_slice(b": ");
    out.extend_from_slice(value.as_bytes());
    out.extend_from_slice(b"\r\n");
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn preserves_template_order_and_frames_binary_body() {
        let mut headers = vec![
            ("Via", "SIP/2.0/TCP 10.0.0.2:5060;branch=z9hG4bK-one"),
            ("From", "<sip:u@ims.test>;tag=one"),
            ("To", "<sip:u@ims.test>"),
            ("Call-ID", "reg-1"),
            ("CSeq", "1 REGISTER"),
            ("Max-Forwards", "70"),
            ("Contact", "<sip:u@10.0.0.2:5060;transport=tcp>"),
            ("Supported", "path,sec-agree,gruu"),
        ];
        let auth = AuthorizationHeader::from_parts("Authorization", "Digest username=\"u\"");
        let wire = RegisterRequest {
            uri: "sip:ims.test",
            headers: &headers,
            body: b"a\0b",
        }
        .encode(Some(&auth))
        .unwrap();
        assert!(wire.ends_with(b"Supported: path,sec-agree,gruu\r\nAuthorization: Digest username=\"u\"\r\nContent-Length: 3\r\n\r\na\0b"));
        for extra in [
            ("l", "3"),
            ("i", "other"),
            ("X-Test", "ok\r\nInjected: yes"),
            ("Authorization", "Digest nonce=x"),
        ] {
            headers.push(extra);
            assert!(
                RegisterRequest {
                    uri: "sip:ims.test",
                    headers: &headers,
                    body: b""
                }
                .encode(Some(&auth))
                .is_err()
            );
            headers.pop();
        }
    }
}
