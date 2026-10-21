#!/usr/bin/env python3
"""Real Rust/Go Unix IPC contract checks, with no card or carrier connection.

Rust supervises a Go test binary using the production store client, inbound
serializer and offline public handlers. Production Go main is deliberately not
called because it probes physical hardware. No fixture endpoint is compiled into
the shipping binary. All data, executables and reports stay under Switch/build.
"""

import argparse
import base64
import http.client
import json
import os
from pathlib import Path
import secrets
import shlex
import signal
import socket
import subprocess
import time


SOURCE = Path(__file__).resolve().parents[1]
ROOT = SOURCE.parent
BUILD = ROOT / "build"
REPORTS = BUILD / "reports"
ARTIFACTS = BUILD / "contract"


def ensure(condition, message):
    if not condition:
        raise AssertionError(message)


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=15)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class Fixture:
    def __init__(self, runtime, env, wrapper, log):
        self.runtime = runtime
        self.socket = runtime / "s.sock"
        self.env = dict(env, SWITCH_DATA_DIR=str(runtime),
                        PROLETARIUS_APP_TOKEN=secrets.token_hex(24),
                        PROLETARIUS_GO_BACKEND_PATH=str(wrapper),
                        SWITCH_INTEGRATION_FIXTURE="1")
        self.log = log
        self.process = None

    def request(self, path, body=None, expected=200, auth=True, headers=None,
                method=None):
        connection = UnixHTTP(self.socket)
        request_headers = {"Content-Type": "application/json"}
        if auth:
            request_headers["X-Proletarius-App-Token"] = self.env["PROLETARIUS_APP_TOKEN"]
        request_headers.update(headers or {})
        payload = None if body is None else json.dumps(body).encode()
        try:
            connection.request(method or ("GET" if body is None else "POST"),
                               path, body=payload, headers=request_headers)
            response = connection.getresponse()
            raw = response.read()
            value = json.loads(raw) if raw else None
            ensure(response.status == expected,
                   f"{method or ('GET' if body is None else 'POST')} {path}: "
                   f"expected {expected}, got {response.status}, response={value}")
            return value
        finally:
            connection.close()

    def rpc(self, path, value=None, expected=200):
        return self.request("/__switch_fixture/rpc", {"path": path, "input": value}, expected)

    def write(self, action, **body):
        body.setdefault("event_id", secrets.token_hex(12))
        return self.rpc("store/" + action, body)

    def inbound(self, event, profile="A"):
        return self.request("/__switch_fixture/inbound", {"profile_id": profile, "event": event})

    def start(self):
        self.process = subprocess.Popen(
            [str(BUILD / "rust-target/debug/switchd-rs"), "-listen", "unix:" + str(self.socket)],
            env=self.env, cwd=SOURCE, stdout=self.log, stderr=subprocess.STDOUT,
            start_new_session=True)
        return self.wait_ready()

    def wait_ready(self, previous_boot=None):
        last = None
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            ensure(self.process.poll() is None, "Rust process exited; inspect process log")
            try:
                info = self.request("/__switch_fixture/info")
                if info["boot_id"] != previous_boot:
                    return info
            except (AssertionError, OSError, http.client.HTTPException) as error:
                last = error
            time.sleep(0.05)
        raise AssertionError(f"fixture did not become ready: {last}")

    def crash_go(self):
        info = self.request("/__switch_fixture/info")
        os.kill(info["pid"], signal.SIGKILL)
        return self.wait_ready(info["boot_id"])

    def stop(self):
        if self.process is None:
            return
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait(timeout=5)
                raise AssertionError("fixture did not shut down; its isolated process group was killed")
        ensure(self.process.returncode == 0, f"Rust exited {self.process.returncode}")
        self.process = None

    def messages(self, profile="A"):
        result, cursor = [], 0
        for _ in range(20):
            page = self.request(f"/api/multistandby/messages?profile_id={profile}&cursor={cursor}")
            result.extend(page["messages"])
            cursor = page["next_cursor"]
            if len(page["messages"]) < 100:
                return result
        raise AssertionError("message pagination did not finish")

    def changes(self):
        result, cursor = [], 0
        for _ in range(20):
            page = self.request(f"/api/multistandby/changes?cursor={cursor}")
            result.extend(page["changes"])
            cursor = page["next_cursor"]
            if len(page["changes"]) < 100:
                return result
        raise AssertionError("change pagination did not finish")

    def snapshot(self):
        # Read-only inspection proves raw opaque events really reached disk;
        # the Go production client never accesses this file.
        return json.loads((self.runtime / "multistandby/store-v1.json").read_text())[0]


def inbound_event(number, content="inbound fixture", **extra):
    raw = b"\x01\x02" + number.to_bytes(4, "big")
    value = {"RPDU": base64.b64encode(raw).decode(),
             "TPDU": base64.b64encode(raw[1:]).decode(),
             "Sender": "+12025550111", "Content": content,
             "SIPCallID": f"fixture-{number}", "SIPCSeq": 1,
             "Decoded": True, "HasTimestamp": True,
             "Timestamp": "2026-09-30T00:00:00Z"}
    value.update(extra)
    return value


def checks(f, record):
    internal = "/api/internal/multistandby/store/catalog"
    f.request("/api/multistandby/profiles", expected=401, auth=False)
    f.request(internal, expected=401)
    f.request(internal, expected=401, headers={"X-Switch-Internal-Token": "wrong"})
    f.request(internal, expected=401,
              headers={"X-Switch-Internal-Token": f.env["PROLETARIUS_APP_TOKEN"]})
    f.request("/api/internal/multistandby/reader-reservation/acquire", {}, expected=401)
    record("public app authentication and private token isolation")

    f.write("catalog", profiles=[{"profile_id": p, "card_id": "fixture-card",
                                 "eid": "fixture-eid", "iccid": f"fixture-iccid-{p}",
                                 "display_name": f"Fixture {p}"} for p in ["A", "B"]])
    desired = f.write("desired", profile_id="A", desired_state="keep_online")
    profiles = f.request("/api/multistandby/profiles")
    ensure(len(profiles["profiles"]) == 2, "offline profiles omitted")
    ensure(all(p["health"] == "offline" and not p["sms_ready"] for p in profiles["profiles"]),
           "offline profiles incorrectly claim readiness")
    ensure(next(p for p in profiles["profiles"] if p["profile_id"] == "A")["desired_state"] == "keep_online",
           "offline desired intention lost")
    f.request("/api/multistandby/messages/send", {"profile_id": "A", "phone": "+12025550123",
              "body": "must not send", "client_request_id": "offline-send"}, expected=409)
    ensure(not f.rpc("store/pending")["messages"], "offline send queued despite no ready session")
    (REPORTS / "multistandby-profiles-fixture.json").write_text(json.dumps(profiles, indent=2))
    record("Go offline catalog/desired state and offline send rejection")

    queue = {"event_id": "queue-1", "profile_id": "A", "client_request_id": "client-1",
             "phone": "+12025550123", "body": "outbound fixture"}
    queued = f.rpc("store/queue", queue)
    retry = dict(queue, event_id="queue-2")
    ensure(f.rpc("store/queue", retry)["message_id"] == queued["message_id"], "queue retry duplicated")
    f.rpc("store/queue", dict(queue, event_id="queue-conflict", body="different"), expected=409)
    outgoing = f.messages()[0]
    ensure(outgoing["direction"] == "outbound" and outgoing["state"] == "queued", "outbound direction/state lost")
    ensure(not f.messages("B"), "profile B sees profile A message")
    f.request(f"/api/multistandby/messages/{queued['message_id']}?profile_id=B", method="DELETE", expected=404)
    record("outbox idempotence, per-profile isolation and message direction")

    normal = inbound_event(1)
    f.inbound(normal)
    prior_changes = len(f.changes())
    retry = dict(normal, SIPCallID="network-retry", SIPCSeq=42, Timestamp="2026-09-30T00:00:02Z")
    f.inbound(retry)
    ensure(len(f.changes()) == prior_changes, "network retry created another notification")
    normal_message = next(m for m in f.messages() if m["body"] == normal["Content"])
    ensure(normal_message["direction"] == "inbound", "inbound direction lost")
    f.inbound(normal, "B")
    ensure(len(f.messages("B")) == 1, "network dedup crossed profile boundary")
    f.inbound(inbound_event(2, "", StatusReport=True, MessageReference=7, DeliveryStatus="delivered"))
    f.inbound(inbound_event(3, "", Decoded=False, DecodeError="unsupported fixture DCS"))
    ensure(len(f.changes()) == prior_changes + 1, "status report/undecoded notified as ordinary message")
    stored = list(f.snapshot()["messages"].values())
    ensure(any(m.get("kind") == "status_report" and m["state"] == "unmatched" and m["raw_tpdu_hex"] for m in stored),
           "raw status report was not durably retained")
    ensure(any(m.get("kind") == "undecoded" and m["state"] == "undecoded" and m["raw_rpdu_hex"] for m in stored),
           "undecoded raw event was not durably retained")
    ensure(all(m["state"] not in ["unmatched", "undecoded"] for m in f.messages()), "opaque event leaked as empty SMS")
    record("Go inbound serialization, network retry dedup, raw report/undecoded durability")

    part = {"Reference": 23, "RefBits": 8, "Total": 2, "Sequence": 1}
    f.inbound(inbound_event(10, "hello ", Concat=part))
    ensure(not any(m["body"] == "hello " for m in f.messages()), "partial fragment appeared as complete")
    before_parts = len(f.changes())
    f.crash_go()
    f.inbound(inbound_event(11, "world", Timestamp="2026-09-30T00:00:01Z", Concat=dict(part, Sequence=2)))
    ensure(len([m for m in f.messages() if m["body"] == "hello world"]) == 1, "multipart failed across Go restart")
    ensure(len(f.changes()) == before_parts + 1, "multipart completion did not notify exactly once")
    record("multipart persistence across real Go child crash/restart")

    for i in range(102):
        f.write("inbound", profile_id="A", message_id=f"page-{i}", dedup_key=f"wire-page-{i}",
                sender="fixture", body=f"page fixture {i}", received_at_ms=1700000000000 + i,
                raw_rpdu_hex="0102", raw_tpdu_hex="02")
    page = f.request("/api/multistandby/messages?profile_id=A&cursor=0")
    ensure(len(page["messages"]) == 100, "first page limit is not 100")
    all_messages = f.messages()
    ensure(len(all_messages) == 105, f"pagination omitted/duplicated rows: {len(all_messages)}")
    ensure(len({m["message_id"] for m in all_messages}) == len(all_messages), "pagination repeated message")
    ensure([m["revision"] for m in all_messages] == sorted(m["revision"] for m in all_messages), "cursor order not monotonic")
    ensure(all("parts" not in m and "attempt_token" not in m for m in all_messages), "private fields exposed publicly")
    changes = f.changes()
    ensure(all(c["direction"] in ["inbound", "outbound"] and c["kind"] == "message" for c in changes),
           "change notification classification missing")
    (REPORTS / "multistandby-messages-fixture.json").write_text(json.dumps(page, indent=2))
    (REPORTS / "multistandby-changes-fixture.json").write_text(json.dumps({"changes": changes[:5]}, indent=2))
    record("public 100-item cursor pagination, direction/kind and private field filtering")

    claim = {"event_id": "claim-1", "profile_id": "A", "message_id": queued["message_id"],
             "attempt_token": "attempt-one", "expected_revision": desired["revision"]}
    f.rpc("store/claim", claim)
    owner = "o" * 48
    f.rpc("reader-reservation/acquire", {"owner_token": owner})
    f.crash_go()
    unknown = next(m for m in f.messages() if m["message_id"] == queued["message_id"])
    ensure(unknown["state"] == "unknown", "Go crash did not fence uncertain send")
    f.rpc("store/claim", claim, expected=409)
    f.rpc("store/claim", dict(claim, event_id="claim-new", attempt_token="attempt-two"), expected=409)
    ensure(not f.rpc("store/pending")["messages"], "uncertain message returned to pending")
    new_owner = "n" * 48
    f.rpc("reader-reservation/acquire", {"owner_token": new_owner})
    f.rpc("reader-reservation/release", {"owner_token": owner}, expected=409)
    f.rpc("reader-reservation/release", {"owner_token": new_owner})
    record("supervised Go crash: submitting→unknown, replay/new claims denied, reservation recovered")

    queued2 = f.write("queue", profile_id="A", client_request_id="client-2", phone="fixture", body="restart unknown")
    f.write("desired", profile_id="A", desired_state="stopped")
    claim2 = {"event_id": "claim-stop", "profile_id": "A", "message_id": queued2["message_id"],
              "attempt_token": "attempt-three", "expected_revision": desired["revision"]}
    f.rpc("store/claim", claim2, expected=409)
    resumed = f.write("desired", profile_id="A", desired_state="keep_online")
    f.rpc("store/claim", dict(claim2, event_id="claim-stale"), expected=409)
    f.rpc("store/claim", dict(claim2, event_id="claim-resume", expected_revision=resumed["revision"]))
    f.write("desired", profile_id="B", desired_state="stopped")
    prior_boot = f.request("/api/multistandby/profiles")["boot_id"]
    f.stop()
    f.start()
    profiles = f.request("/api/multistandby/profiles")
    ensure(profiles["boot_id"] != prior_boot, "new Go boot ID not exposed")
    states = {p["profile_id"]: p["desired_state"] for p in profiles["profiles"]}
    ensure(states == {"A": "keep_online", "B": "stopped"}, "desired intent lost at full restart")
    ensure(next(m for m in f.messages() if m["message_id"] == queued2["message_id"])["state"] == "unknown",
           "Rust startup did not fence uncertain send")
    f.rpc("store/claim", dict(claim2, event_id="claim-after-restart", expected_revision=resumed["revision"]), expected=409)
    ensure(len(f.messages()) == 106, "offline history not retained at full restart")
    record("desired revision cancellation fence, whole-process restart and offline history")

    f.request(f"/api/multistandby/messages/{normal_message['message_id']}?profile_id=A", method="DELETE")
    f.inbound(normal)
    ensure(not any(m["message_id"] == normal_message["message_id"] for m in f.messages()), "retry resurrected deleted message")
    ensure(any(c["deleted"] and c["message_id"] == normal_message["message_id"] for c in f.changes()), "delete change absent")
    ensure(len(f.messages("B")) == 1, "delete crossed profile boundary")
    record("public delete tombstone, no network replay resurrection and cross-profile retention")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--skip-build", action="store_true", help="reuse binaries built by this script")
    args = parser.parse_args()
    ensure(ROOT == Path("/Users/proletarius/Desktop/Switch"), "this fixture must stay inside the isolated Switch workspace")
    for path in [REPORTS, ARTIFACTS, BUILD / "tmp", BUILD / "runtime"]:
        path.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, CARGO_TARGET_DIR=str(BUILD / "rust-target"), TMPDIR=str(BUILD / "tmp"),
               SWITCH_DATA_DIR=str(BUILD / "runtime"), GOPATH=str(BUILD / "go"),
               GOMODCACHE=str(BUILD / "go-mod"), GOCACHE=str(BUILD / "go-cache"))
    fixture_binary = ARTIFACTS / "go-fixture"
    if not args.skip_build:
        commands = [(["cargo", "+1.98.0", "build", "-p", "proletariusd"], SOURCE / "rust"),
                    (["go", "test", "-c", "-o", str(fixture_binary), "./cmd/proletarius-macos"], SOURCE / "backend")]
        with (REPORTS / "multistandby-contract-build.log").open("w") as log:
            for command, cwd in commands:
                print("Building", " ".join(command), flush=True)
                subprocess.run(command, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
    ensure(fixture_binary.is_file(), "Go fixture binary missing; run without --skip-build")
    wrapper = ARTIFACTS / "go-fixture-wrapper.sh"
    wrapper.write_text("#!/bin/sh\nexec " + shlex.quote(str(fixture_binary)) +
                       " -test.run '^TestMultiStandbyProcessFixture$' -test.timeout 0 \"$@\"\n")
    wrapper.chmod(0o700)
    runtime = BUILD / "runtime" / ("ct-" + secrets.token_hex(3))
    runtime.mkdir(mode=0o700)
    results = []

    def record(name):
        results.append({"name": name, "passed": True})
        print("PASS", name, flush=True)

    report = {"fixture": "real Rust supervisor + Go production IPC/handlers in hardware-free test process",
              "scope": "No physical card, IMS registration or carrier SMS is exercised",
              "runtime": str(runtime), "checks": results}
    with (REPORTS / "multistandby-contract-process.log").open("w") as log:
        fixture = Fixture(runtime, env, wrapper, log)
        try:
            fixture.start()
            checks(fixture, record)
            report["passed"] = True
        except Exception as error:
            report["passed"] = False
            report["error"] = str(error)
            raise
        finally:
            try:
                fixture.stop()
            finally:
                (REPORTS / "multistandby-contract-results.json").write_text(json.dumps(report, indent=2))
    print(f"Passed {len(results)} cross-language checks; reports: {REPORTS}", flush=True)


if __name__ == "__main__":
    main()
