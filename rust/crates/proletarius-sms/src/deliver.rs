//! SMS-DELIVER framing. Timestamp bytes are retained for explicit calendar conversion.
use crate::{address, udh, user_data};

#[derive(Debug, PartialEq, Eq)]
pub struct Message {
    pub sender: String,
    pub timestamp: [u8; 7],
    pub service_center_time: Option<crate::timestamp::Timestamp>,
    pub protocol_id: u8,
    pub dcs: u8,
    pub concatenation: Option<udh::Concatenation>,
    pub content: user_data::Content,
}

#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    Framing,
    Address,
    Content(user_data::DecodeError),
}

pub fn parse(tpdu: &[u8]) -> Result<Message, Error> {
    if tpdu.len() < 3 || tpdu.len() > 232 || tpdu[0] & 3 != 0 {
        return Err(Error::Framing);
    }
    let (sender, offset) = address::parse(tpdu, 1).map_err(|error| match error {
        address::Error::Framing => Error::Framing,
        address::Error::Invalid => Error::Address,
    })?;
    let fixed = tpdu.get(offset..offset + 10).ok_or(Error::Framing)?;
    let timestamp = fixed[2..9].try_into().map_err(|_| Error::Framing)?;
    let service_center_time = crate::timestamp::parse(&fixed[2..9]).ok();
    let data = &tpdu[offset + 10..];
    let has_header = tpdu[0] & 0x40 != 0;
    let concatenation = if has_header {
        udh::parse(data).map_err(|_| Error::Framing)?.concatenation
    } else {
        None
    };
    let content =
        user_data::decode(fixed[1], fixed[9], has_header, data).map_err(Error::Content)?;
    Ok(Message {
        sender,
        timestamp,
        service_center_time,
        protocol_id: fixed[0],
        dcs: fixed[1],
        concatenation,
        content,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    const FIXTURE: &[u8] = &[
        0, 3, 0x81, 0x88, 0xf8, 0, 8, 0x62, 0x90, 0x60, 0, 0, 0, 0, 2, 0, 0x41,
    ];
    #[test]
    fn parses_sender_and_body_without_smsc_prefix() {
        let message = parse(FIXTURE).unwrap();
        assert_eq!(message.sender, "888");
        assert_eq!(message.content, user_data::Content::Text("A".into()));
        assert_eq!(message.timestamp, [0x62, 0x90, 0x60, 0, 0, 0, 0]);
        assert_eq!(message.service_center_time.unwrap().year, 2026);
    }

    #[test]
    fn invalid_calendar_does_not_discard_message_content() {
        let mut fixture = FIXTURE.to_vec();
        fixture[8] = 0x31;
        let message = parse(&fixture).unwrap();
        assert!(message.service_center_time.is_none());
        assert_eq!(message.timestamp[1], 0x31);
        assert_eq!(message.content, user_data::Content::Text("A".into()));
    }

    #[test]
    fn alphanumeric_sender_length_uses_semi_octets() {
        let address = crate::septets::pack(b"OKX", 0).unwrap();
        let mut tpdu = vec![0, 6, 0xd0];
        tpdu.extend(address);
        tpdu.extend_from_slice(&FIXTURE[5..]);
        let message = parse(&tpdu).unwrap();
        assert_eq!(message.sender, "OKX");
        assert_eq!(message.content, user_data::Content::Text("A".into()));
    }
    #[test]
    fn rejects_all_truncations_and_invalid_numeric_address() {
        for count in 0..FIXTURE.len() {
            assert!(parse(&FIXTURE[..count]).is_err());
        }
        let mut malformed = FIXTURE.to_vec();
        malformed[3] = 0xaa;
        assert_eq!(parse(&malformed), Err(Error::Address));
        let mut trailing = FIXTURE.to_vec();
        trailing.push(0);
        assert!(parse(&trailing).is_err());
    }
}
