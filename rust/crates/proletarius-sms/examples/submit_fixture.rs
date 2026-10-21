//! Offline synthetic-fixture helper; never sends messages.
fn main() {
    let args: Vec<String> = std::env::args().collect();
    assert_eq!(args.len(), 3);
    let parts = proletarius_sms::submit::prepare(&args[1], &args[2], 42).expect("fixture encode");
    for part in parts {
        for byte in part {
            print!("{byte:02x}");
        }
        println!();
    }
}
