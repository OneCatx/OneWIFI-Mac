//! AES-CBC/HMAC-SHA1-96 IMS transport ESP. Verify ICV before decrypting.
use aes::cipher::{BlockModeDecrypt, BlockModeEncrypt, KeyIvInit, block_padding::NoPadding};
use hmac::{Hmac, Mac};
use sha1::Sha1;
use zeroize::Zeroizing;

#[derive(Debug, PartialEq, Eq)]
pub enum EspError {
    InvalidPacket,
    InvalidSpi,
    Authentication,
    Replay,
    Exhausted,
    Entropy,
}

pub const ESP_HEADER_IV_ICV_BYTES: usize = 36;

pub fn sealed_len(tcp_len: usize) -> Option<usize> {
    let padded = tcp_len.checked_add(2)?.checked_add(15)? / 16 * 16;
    ESP_HEADER_IV_ICV_BYTES.checked_add(padded)
}
pub struct Sender {
    spi: u32,
    sequence: u32,
    encryption: Zeroizing<[u8; 16]>,
    integrity: Zeroizing<[u8; 20]>,
}
pub struct Receiver {
    spi: u32,
    highest: u32,
    window: u64,
    encryption: Zeroizing<[u8; 16]>,
    integrity: Zeroizing<[u8; 20]>,
}
impl Sender {
    pub fn new(
        spi: u32,
        encryption: Zeroizing<[u8; 16]>,
        integrity: Zeroizing<[u8; 20]>,
    ) -> Result<Self, EspError> {
        if spi == 0 {
            return Err(EspError::InvalidSpi);
        }
        Ok(Self {
            spi,
            sequence: 0,
            encryption,
            integrity,
        })
    }
    pub fn seal(&mut self, tcp: &[u8]) -> Result<Vec<u8>, EspError> {
        let mut iv = [0; 16];
        getrandom::fill(&mut iv).map_err(|_| EspError::Entropy)?;
        self.seal_with_iv(tcp, iv)
    }
    fn seal_with_iv(&mut self, tcp: &[u8], iv: [u8; 16]) -> Result<Vec<u8>, EspError> {
        if tcp.len() < 20 || tcp.len() > 65_000 {
            return Err(EspError::InvalidPacket);
        }
        self.sequence = self.sequence.checked_add(1).ok_or(EspError::Exhausted)?;
        let pad = (16 - (tcp.len() + 2) % 16) % 16;
        let mut plain = Zeroizing::new(tcp.to_vec());
        for byte in 1..=pad {
            plain.push(byte as u8);
        }
        plain.extend_from_slice(&[pad as u8, 6]);
        let len = plain.len();
        cbc::Encryptor::<aes::Aes128>::new_from_slices(self.encryption.as_ref(), &iv)
            .map_err(|_| EspError::InvalidPacket)?
            .encrypt_padded::<NoPadding>(&mut plain, len)
            .map_err(|_| EspError::InvalidPacket)?;
        let mut packet = Vec::with_capacity(8 + 16 + len + 12);
        packet.extend_from_slice(&self.spi.to_be_bytes());
        packet.extend_from_slice(&self.sequence.to_be_bytes());
        packet.extend_from_slice(&iv);
        packet.extend_from_slice(&plain);
        let mut mac = Hmac::<Sha1>::new_from_slice(self.integrity.as_ref())
            .map_err(|_| EspError::Authentication)?;
        mac.update(&packet);
        packet.extend_from_slice(&mac.finalize().into_bytes()[..12]);
        Ok(packet)
    }
}
impl Receiver {
    pub fn new(
        spi: u32,
        encryption: Zeroizing<[u8; 16]>,
        integrity: Zeroizing<[u8; 20]>,
    ) -> Result<Self, EspError> {
        if spi == 0 {
            return Err(EspError::InvalidSpi);
        }
        Ok(Self {
            spi,
            highest: 0,
            window: 0,
            encryption,
            integrity,
        })
    }
    pub fn open(&mut self, packet: &[u8]) -> Result<Zeroizing<Vec<u8>>, EspError> {
        if packet.len() < 68 || packet.len() > 65_068 || !(packet.len() - 36).is_multiple_of(16) {
            return Err(EspError::InvalidPacket);
        }
        if u32::from_be_bytes(packet[..4].try_into().unwrap()) != self.spi {
            return Err(EspError::InvalidSpi);
        }
        let seq = u32::from_be_bytes(packet[4..8].try_into().unwrap());
        if seq == 0
            || (seq <= self.highest
                && (self.highest - seq >= 64 || self.window & (1u64 << (self.highest - seq)) != 0))
        {
            return Err(EspError::Replay);
        }
        let authenticated = packet.len() - 12;
        let mut mac = Hmac::<Sha1>::new_from_slice(self.integrity.as_ref())
            .map_err(|_| EspError::Authentication)?;
        mac.update(&packet[..authenticated]);
        mac.verify_truncated_left(&packet[authenticated..])
            .map_err(|_| EspError::Authentication)?;
        let mut plain = Zeroizing::new(packet[24..authenticated].to_vec());
        cbc::Decryptor::<aes::Aes128>::new_from_slices(self.encryption.as_ref(), &packet[8..24])
            .map_err(|_| EspError::InvalidPacket)?
            .decrypt_padded::<NoPadding>(&mut plain)
            .map_err(|_| EspError::InvalidPacket)?;
        let pad = usize::from(plain[plain.len() - 2]);
        let end = plain
            .len()
            .checked_sub(pad + 2)
            .ok_or(EspError::InvalidPacket)?;
        if end < 20
            || plain[plain.len() - 1] != 6
            || plain[end..end + pad]
                .iter()
                .enumerate()
                .any(|(index, byte)| usize::from(*byte) != index + 1)
        {
            return Err(EspError::InvalidPacket);
        }
        plain.truncate(end);
        if seq > self.highest {
            let shift = seq - self.highest;
            self.window = if shift >= 64 {
                1
            } else {
                (self.window << shift) | 1
            };
            self.highest = seq;
        } else {
            self.window |= 1u64 << (self.highest - seq);
        }
        Ok(plain)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn tamper_replay_reordering_and_exhaustion() {
        let mut sender =
            Sender::new(42, Zeroizing::new([0x11; 16]), Zeroizing::new([0x22; 20])).unwrap();
        let mut receiver =
            Receiver::new(42, Zeroizing::new([0x11; 16]), Zeroizing::new([0x22; 20])).unwrap();
        let payload = [0x33; 20];
        let first = sender.seal_with_iv(&payload, [0x44; 16]).unwrap();
        let hex: String = first.iter().map(|b| format!("{b:02x}")).collect();
        assert_eq!(
            hex,
            "0000002a0000000144444444444444444444444444444444dc44b424bcb85288ce3be42430864e8b29687bec19b041a7bd700c5353ba8ac3c8dece0d2b98e9920ce61645"
        );
        let second = sender.seal(&payload).unwrap();
        let mut bad = second.clone();
        bad[7] = 200;
        assert_eq!(receiver.open(&bad), Err(EspError::Authentication));
        assert_eq!(receiver.highest, 0);
        assert_eq!(&*receiver.open(&second).unwrap(), &payload);
        assert_eq!(&*receiver.open(&first).unwrap(), &payload);
        assert_eq!(receiver.open(&first), Err(EspError::Replay));
        sender.sequence = u32::MAX;
        assert_eq!(sender.seal(&payload), Err(EspError::Exhausted));
    }

    #[test]
    fn wire_length_boundaries_are_explicit() {
        for (tcp, wire) in [
            (20, 68),
            (1214, 1252),
            (1215, 1268),
            (1198, 1236),
            (1199, 1252),
        ] {
            assert_eq!(sealed_len(tcp), Some(wire));
        }
        assert_eq!(sealed_len(usize::MAX), None);
    }
}
