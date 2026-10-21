import Foundation

@main
private enum BLEPhaseDeadlineTests {
    static func main() {
        let queue = DispatchQueue(label: "ble-phase-deadline-tests")
        let deadline = BLEPhaseDeadline(queue: queue)
        let expired = DispatchSemaphore(value: 0)

        deadline.arm(after: 0.02) { expired.signal() }
        precondition(expired.wait(timeout: .now() + 1) == .success, "armed deadline did not fire")

        let cancelled = DispatchSemaphore(value: 0)
        deadline.arm(after: 0.03) { cancelled.signal() }
        deadline.cancel()
        precondition(cancelled.wait(timeout: .now() + 0.08) == .timedOut, "cancelled deadline fired")

        let replacement = DispatchSemaphore(value: 0)
        var count = 0
        deadline.arm(after: 0.03) { count += 10; replacement.signal() }
        deadline.arm(after: 0.08) { count += 1; replacement.signal() }
        precondition(replacement.wait(timeout: .now() + 1) == .success, "replacement deadline did not fire")
        precondition(count == 1, "superseded deadline fired after replacement")

        // Repeated phase changes must leave only the final deadline armed.
        for _ in 0..<100 {
            deadline.arm(after: 0.01) { count += 100 }
            deadline.cancel()
        }
        let settled = DispatchSemaphore(value: 0)
        queue.asyncAfter(deadline: .now() + 0.04) { settled.signal() }
        precondition(settled.wait(timeout: .now() + 1) == .success, "deadline stress wait timed out")
        precondition(count == 1, "cancel stress leaked a deadline callback")
        print("BLE phase deadline: expiry, cancel, replace, and 100-cycle cancel stress passed")
    }
}
