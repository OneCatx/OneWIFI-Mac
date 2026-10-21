# Switch Rust runtime

This workspace is the staged replacement for the Go backend. Rust owns the
public gateway, reader inventory and read-only identity/AKA PC/SC operations,
call archive, and local SMS archive. eSIM mutation, USB/QMI devices, live
VoWiFi/IMS transport, SMS production, and macOS audio remain outside Rust's
runtime ownership. Go stays supervised until each remaining cutover gate passes.

The gateway binary provides:

- the existing `-listen unix:<path>` launch contract;
- `GET /api/v2/runtime` for migration diagnostics;
- the existing per-launch application-token check;
- signal-driven shutdown and safe Unix-socket cleanup.
- supervised launch and graceful termination of the bundled Go backend;
- transparent forwarding of all existing application APIs to Go.
- `GET /api/v2/device-snapshot`, a schema-versioned typed shadow read of
  module, reader inventory, selected reader, card identity, and reader VoWiFi
  state from one snapshot generation;
- partial-failure reporting so one absent device does not erase the other.
- a hidden Swift read-path gate that consumes the snapshot with compatible-endpoint fallback while Go remains the hardware owner.
- sanitized Go fixture replay plus Unix-socket timeout, size-limit, malformed-payload, and status-code failure injection before any default cutover.
- bounded Go child restart with 250 ms / 1 s / 3 s backoff, a rapid-failure cap, stable-window reset, and public-socket continuity.
- bounded read recovery across child startup/restart, explicit child generation diagnostics, and fail-fast non-replay of mutating requests.
- native macOS PC/SC inventory and card-state observation with revisioned
  hot-plug reconciliation; this observer never opens the card or sends APDUs.
- a strict Rust SIM protocol core for complete-AID selection, bounded APDU
  response chaining, and 3G AKA success/AUTS/reject parsing without secret
  payload logging; USIM/ISIM AKA now runs in a serialized native PC/SC
  transaction, while eSIM mutation and other live card sessions remain on Go
  until their ownership gates pass.
- strict IKEv2 header/NAT-T framing and a SPI/message-ID session window that
  keeps multi-round EAP-AKA authentication separate from final establishment.
- strict SIP response framing and a REGISTER transaction state machine that
  validates Call-ID/CSeq, bounded AKA challenge rounds, and sec-agree offers.
- strict IMS SMS RP-DATA/RP-ACK/RP-ERROR framing with bounded, single-use
  delivery-reference correlation.
- deterministic INVITE/CANCEL/final-response state, strict RTP framing,
  bounded loss classification, and RFC4733 DTMF payloads.
- `GET /api/v2/migration-readiness`, a schema-versioned component ownership
  manifest that keeps Go removal fail-closed until every blocking live runtime
  has an accepted Rust owner.
- a locked, atomic Rust call archive that natively serves list, clear, and
  exact-ID delete while preserving the existing Go JSON and Swift API shapes.
- a locked, atomic local SMS archive API at `GET /api/v2/sms-archive` and its
  native clear/exact-ID/exact-sender mutations. Live SMS production, sending,
  SIM/ME storage, and device routing remain explicitly Go-owned.
- a bounded native SOCKS5 handshake and UDP ASSOCIATE capability probe at
  `POST /api/v2/vowifi/proxy-probe`. It never resolves or contacts the ePDG;
  the compatibility runtime consumes it while live-session ownership moves.

Run locally:

```sh
cargo test --workspace
PROLETARIUS_GO_BACKEND_PATH=/path/to/switchd \
cargo run -p proletariusd --bin switchd-rs -- -listen unix:/tmp/switch-rust.sock
```

Before a migration cutover, run the repository-wide gate from the project
root:

```sh
./scripts/test-migration-gates.sh
```

The recorded proxy contract is `v0.16-proxy-preferred-direct`: an explicitly
configured transparent/SOCKS5 path is preferred, while a real public ePDG
address may use the current network directly when proxy transport is absent or
unavailable. Fake-IP addresses are never reclassified as public direct
destinations. The remaining Go ownership is intentionally limited to the
hardware bridge and live-session paths that still need differential and live
tests (DJI USB/QMI, mutating eSIM/APDU sessions, and the reader VoWiFi
transport). Rust already owns the reader identity/AKA protocol core and all
local archives; those compatibility bridges are removed only after their
cutover gates pass.
