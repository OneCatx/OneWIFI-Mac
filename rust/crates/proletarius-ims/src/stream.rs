//! Cancellation-safe bounded framing for SIP over a reliable byte stream.
//! Framing is independent of requests/responses and does not mark IMS online.
use crate::{
    MAX_HEADER_COUNT, MAX_HEADER_LINE_LENGTH, MAX_SIP_MESSAGE_LENGTH, SipError, is_header_name_byte,
};
use std::io;
use tokio::io::{AsyncRead, AsyncReadExt};
use zeroize::Zeroizing;

pub enum SipStreamItem {
    Message(Zeroizing<Vec<u8>>),
    /// One leading CRLF. The session owner correlates ping/pong sequences;
    /// this event alone is not an authenticated health or registration proof.
    KeepAliveLine,
}

pub struct SipStreamReader<R> {
    reader: R,
    buffer: Zeroizing<Vec<u8>>,
    scan: usize,
    frame_len: Option<usize>,
    failed: bool,
}

impl<R: AsyncRead + Unpin> SipStreamReader<R> {
    pub fn new(reader: R) -> Self {
        Self {
            reader,
            buffer: Zeroizing::new(Vec::new()),
            scan: 0,
            frame_len: None,
            failed: false,
        }
    }

    /// Recover the underlying reader when ownership of a reliable transport
    /// must move to a different protocol phase. Any already-buffered bytes are
    /// deliberately discarded and zeroized with this value; callers must only
    /// use this at a protocol boundary where traffic from the old transport is
    /// no longer valid (for example before installing IMS sec-agree).
    pub fn into_inner(self) -> R {
        self.reader
    }

    /// Safe to cancel while waiting: bytes already read remain owned here.
    /// A framing error is terminal; never search later bytes for another SIP
    /// start line, which could reinterpret a malicious/truncated message body.
    pub async fn next(&mut self) -> io::Result<Option<SipStreamItem>> {
        if self.failed {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "SIP stream framing previously failed",
            ));
        }
        loop {
            if self.frame_len.is_none() && self.buffer.starts_with(b"\r\n") {
                self.take(2);
                return Ok(Some(SipStreamItem::KeepAliveLine));
            }
            if self.frame_len.is_none() {
                if let Some(offset) = self.buffer[self.scan..]
                    .windows(4)
                    .position(|w| w == b"\r\n\r\n")
                {
                    let boundary = self.scan + offset;
                    match framed_length(&self.buffer[..boundary]) {
                        Ok(length) => self.frame_len = Some(length),
                        Err(error) => {
                            self.failed = true;
                            self.buffer = Zeroizing::new(Vec::new());
                            return Err(io::Error::new(io::ErrorKind::InvalidData, error));
                        }
                    }
                } else {
                    self.scan = self.buffer.len().saturating_sub(3);
                }
            }
            if let Some(length) = self.frame_len
                && self.buffer.len() >= length
            {
                return Ok(Some(SipStreamItem::Message(self.take(length))));
            }
            let remaining = MAX_SIP_MESSAGE_LENGTH.saturating_sub(self.buffer.len());
            if remaining == 0 {
                self.failed = true;
                self.buffer = Zeroizing::new(Vec::new());
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    SipError::MessageTooLarge,
                ));
            }
            let mut scratch = Zeroizing::new([0u8; 4096]);
            let capacity = remaining.min(scratch.len());
            let count = match self.reader.read(&mut scratch[..capacity]).await {
                Ok(count) => count,
                Err(error) => {
                    self.failed = true;
                    return Err(error);
                }
            };
            if count == 0 {
                if self.buffer.is_empty() {
                    return Ok(None);
                }
                self.failed = true;
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "truncated SIP stream message",
                ));
            }
            self.buffer.extend_from_slice(&scratch[..count]);
        }
    }

    fn take(&mut self, length: usize) -> Zeroizing<Vec<u8>> {
        let frame = Zeroizing::new(self.buffer[..length].to_vec());
        self.buffer = Zeroizing::new(self.buffer[length..].to_vec());
        self.scan = 0;
        self.frame_len = None;
        frame
    }
}

pub(crate) fn framed_length(head: &[u8]) -> Result<usize, SipError> {
    let head = std::str::from_utf8(head).map_err(|_| SipError::InvalidHeader)?;
    let mut lines = head.split("\r\n");
    let first = lines.next().ok_or(SipError::InvalidHeader)?;
    if first.is_empty()
        || first.len() > MAX_HEADER_LINE_LENGTH
        || first.bytes().any(|b| b.is_ascii_control())
    {
        return Err(SipError::InvalidHeader);
    }
    let mut length = None;
    for (count, line) in lines.enumerate() {
        if count >= MAX_HEADER_COUNT {
            return Err(SipError::TooManyHeaders);
        }
        if line.len() > MAX_HEADER_LINE_LENGTH {
            return Err(SipError::HeaderLineTooLong);
        }
        if line.starts_with([' ', '\t']) {
            return Err(SipError::InvalidHeader);
        }
        let (name, value) = line.split_once(':').ok_or(SipError::InvalidHeader)?;
        let name = name.trim();
        let value = value.trim();
        if name.is_empty()
            || !name.bytes().all(is_header_name_byte)
            || value.bytes().any(|b| b.is_ascii_control())
        {
            return Err(SipError::InvalidHeader);
        }
        if name.eq_ignore_ascii_case("content-length") || name.eq_ignore_ascii_case("l") {
            if length.is_some() {
                return Err(SipError::DuplicateContentLength);
            }
            if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
                return Err(SipError::InvalidContentLength);
            }
            length = Some(
                value
                    .parse::<usize>()
                    .map_err(|_| SipError::InvalidContentLength)?,
            );
        }
    }
    // RFC 3261 reliable transports require Content-Length for framing.
    let total = head
        .len()
        .checked_add(4)
        .and_then(|n| n.checked_add(length?))
        .ok_or(SipError::InvalidContentLength)?;
    if total > MAX_SIP_MESSAGE_LENGTH {
        return Err(SipError::MessageTooLarge);
    }
    Ok(total)
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::AsyncWriteExt;

    #[tokio::test]
    async fn fragments_pipeline_and_binary_body_preserve_boundaries() {
        let (mut writer, reader) = tokio::io::duplex(16);
        let first = b"MESSAGE sip:a@ims SIP/2.0\r\nl: 6\r\n\r\n\0\xff\r\n\r\n";
        let second = b"SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n";
        let send = async {
            for byte in b"\r\n\r\n".iter().chain(first).chain(second) {
                writer.write_all(&[*byte]).await.unwrap();
            }
            writer.shutdown().await.unwrap();
        };
        let receive = async {
            let mut reader = SipStreamReader::new(reader);
            for _ in 0..2 {
                assert!(matches!(
                    reader.next().await.unwrap(),
                    Some(SipStreamItem::KeepAliveLine)
                ));
            }
            for expected in [first.as_slice(), second.as_slice()] {
                let Some(SipStreamItem::Message(frame)) = reader.next().await.unwrap() else {
                    panic!("missing message")
                };
                assert_eq!(&*frame, expected);
            }
            assert!(reader.next().await.unwrap().is_none());
        };
        tokio::join!(send, receive);
    }

    #[tokio::test]
    async fn malformed_framing_and_truncated_eof_fail_closed() {
        for packet in [
            b"SIP/2.0 200 OK\r\n\r\n".as_slice(),
            b"SIP/2.0 200 OK\r\nContent-Length: 0\r\nl: 0\r\n\r\n",
            b"SIP/2.0 200 OK\r\nContent-Length: 999999\r\n\r\n",
            b"SIP/2.0 200 OK\r\nContent-Length: 2\r\n\r\nx",
        ] {
            let mut reader = SipStreamReader::new(packet);
            assert!(reader.next().await.is_err());
            assert!(reader.next().await.is_err());
        }
    }

    #[tokio::test]
    async fn cancelling_partial_read_preserves_received_prefix() {
        let (mut writer, input) = tokio::io::duplex(128);
        writer
            .write_all(b"SIP/2.0 200 OK\r\nContent-Len")
            .await
            .unwrap();
        let mut reader = SipStreamReader::new(input);
        assert!(
            tokio::time::timeout(std::time::Duration::from_millis(10), reader.next())
                .await
                .is_err()
        );
        writer.write_all(b"gth: 0\r\n\r\n").await.unwrap();
        let Some(SipStreamItem::Message(frame)) = reader.next().await.unwrap() else {
            panic!("missing frame")
        };
        assert_eq!(&*frame, b"SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n");
    }
}
