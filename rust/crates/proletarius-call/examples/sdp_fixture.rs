use proletarius_call::sdp::{AudioCodec, negotiate_audio};

fn main() -> Result<(), String> {
    let answer = match std::env::args().nth(1).as_deref() {
        Some("answer") => true,
        Some("offer") => false,
        _ => return Err("usage: sdp_fixture <offer|answer> <sdp>".to_owned()),
    };
    let body = std::env::args()
        .nth(2)
        .ok_or_else(|| "missing SDP body".to_owned())?;
    match negotiate_audio(&body, answer) {
        Ok(result) => {
            let (codec, octet_aligned) = match result.codec {
                AudioCodec::Pcma => ("PCMA", false),
                AudioCodec::AmrNbMode7 { octet_aligned } => ("AMR", octet_aligned),
            };
            println!(
                "ok|{}|{}|{}|{}|{}|{}",
                result.remote_ip,
                result.remote_port,
                result.payload_type,
                codec,
                octet_aligned,
                result
                    .telephone_event_payload
                    .map_or_else(|| "-".to_owned(), |value| value.to_string())
            );
        }
        Err(error) => println!("error|{error}"),
    }
    Ok(())
}
