//! Offline cross-language parameter replay; no hardware or network access.
use proletarius_vowifi::carrier::{Registry, SimIdentity};
use proletarius_vowifi::carrier_runtime_profile::RuntimeCarrierProfile;
use serde_json::{Value, json};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let output = std::env::args().nth(1).ok_or("output path is required")?;
    let cases: Vec<Value> = serde_json::from_str(include_str!("../../../carrier-cases.json"))?;
    let registry = Registry::embedded()?;
    let mut results = Vec::with_capacity(cases.len());
    for case in cases {
        let sim: SimIdentity = serde_json::from_value(case["sim"].clone())?;
        let selected = match registry.resolve(&sim) {
            Ok(selected) => selected,
            Err(_) => {
                results.push(json!({"sim":case["sim"],"rejected":true}));
                continue;
            }
        };
        let runtime_profile = match selected.as_ref() {
            Some(profile) => RuntimeCarrierProfile::expand(profile, &sim, &registry.revision),
            None => Ok(RuntimeCarrierProfile::standard(&sim, &registry.revision)),
        };
        match runtime_profile {
            Ok(runtime_profile) => results.push(json!({
                "sim":case["sim"],
                "matched":selected.is_some(),
                "runtime_profile":runtime_profile,
                "rejected":false
            })),
            Err(_) => results.push(json!({"sim":case["sim"],"rejected":true})),
        }
    }
    std::fs::write(output, serde_json::to_vec(&results)?)?;
    println!("replayed_cases={}", results.len());
    Ok(())
}
