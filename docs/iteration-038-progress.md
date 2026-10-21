> 【历史】以下内容来自 Proletarius／Switch 时代，含当时功能、路径、发布与验证结果，仅供追溯；**不是 OneWIFI 的当前指令或验收结论**。传真能力已移除；现行产物为 `OneWIFI.app`，构建与范围以根 README 与 `docs/客户端-Mac-身份冻结-20261021.md` 为准。

# Iteration 038 — Reader lease renewal

## Scope

The Rust native reader session lease was previously protected by a fixed
two-minute Go watchdog, but the private Rust client had no renewal operation.
That was insufficient for any future session actor that owns a reader across
long-lived IKE/IMS registration.

## Changes

- Added a private authenticated `session-lease/renew` endpoint and Rust client
  method. Renewal extends only the matching live lease; an expired or stale
  token cannot reacquire reader ownership.
- Stored the lease deadline and made the expiry callback re-check it while
  holding the same mutex used by renewal, AKA handoff, and release. A callback
  already queued when renewal arrives now reschedules instead of releasing the
  reader underneath the session.
- Added Go lifecycle/handler tests and Rust wire-contract coverage for renewal.
- Incremented the app version from 2.16 to 2.17 (build 217).

## Verification and limits

The focused Go lease tests pass. Renewal is still a gated building block: the
native Rust VoWiFi actor does not yet consume it, and the production ownership
gates remain closed. This iteration therefore does not claim Rust-owned IMS,
SMS, incoming calls, or an attached-reader live acceptance test. The DMG is a
macOS 27 build of the current integrated app and does not change that gate.
