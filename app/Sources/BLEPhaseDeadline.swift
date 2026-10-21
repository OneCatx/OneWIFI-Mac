import Foundation

/// Cancellable inactivity deadline for one CoreBluetooth connection phase.
/// Calls are serialized on the queue that owns the Bluetooth central.
final class BLEPhaseDeadline {
    private let queue: DispatchQueue
    private var generation: UInt64 = 0
    private var work: DispatchWorkItem?

    init(queue: DispatchQueue = .main) {
        self.queue = queue
    }

    func arm(after interval: TimeInterval, onTimeout: @escaping () -> Void) {
        cancel()
        let activeGeneration = generation
        let next = DispatchWorkItem { [weak self] in
            guard let self, self.generation == activeGeneration else { return }
            self.work = nil
            onTimeout()
        }
        work = next
        queue.asyncAfter(deadline: .now() + interval, execute: next)
    }

    func cancel() {
        generation &+= 1
        work?.cancel()
        work = nil
    }
}
