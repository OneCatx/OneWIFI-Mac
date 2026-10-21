use std::time::Duration;

const MAX_CONSECUTIVE_RESTARTS: usize = 3;
const STABLE_WINDOW: Duration = Duration::from_secs(30);
const RESTART_DELAYS: [Duration; MAX_CONSECUTIVE_RESTARTS] = [
    Duration::from_millis(250),
    Duration::from_secs(1),
    Duration::from_secs(3),
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RestartDecision {
    RetryAfter { attempt: usize, delay: Duration },
    Exhausted,
}

#[derive(Default)]
pub(crate) struct RestartTracker {
    consecutive_failures: usize,
}

impl RestartTracker {
    pub(crate) fn record_failure(&mut self, previous_uptime: Duration) -> RestartDecision {
        if previous_uptime >= STABLE_WINDOW {
            self.consecutive_failures = 0;
        }
        if self.consecutive_failures >= MAX_CONSECUTIVE_RESTARTS {
            return RestartDecision::Exhausted;
        }
        let delay = RESTART_DELAYS[self.consecutive_failures];
        self.consecutive_failures += 1;
        RestartDecision::RetryAfter {
            attempt: self.consecutive_failures,
            delay,
        }
    }

    pub(crate) fn observe_healthy(&mut self, uptime: Duration) -> bool {
        if uptime >= STABLE_WINDOW && self.consecutive_failures != 0 {
            self.consecutive_failures = 0;
            return true;
        }
        false
    }

    pub(crate) const fn attempts(&self) -> usize {
        self.consecutive_failures
    }
}

#[cfg(test)]
mod tests {
    use super::{RestartDecision, RestartTracker};
    use std::time::Duration;

    #[test]
    fn rapid_failures_back_off_then_exhaust() {
        let mut tracker = RestartTracker::default();
        assert_eq!(
            tracker.record_failure(Duration::from_secs(1)),
            RestartDecision::RetryAfter {
                attempt: 1,
                delay: Duration::from_millis(250),
            }
        );
        assert_eq!(
            tracker.record_failure(Duration::from_secs(1)),
            RestartDecision::RetryAfter {
                attempt: 2,
                delay: Duration::from_secs(1),
            }
        );
        assert_eq!(
            tracker.record_failure(Duration::from_secs(1)),
            RestartDecision::RetryAfter {
                attempt: 3,
                delay: Duration::from_secs(3),
            }
        );
        assert_eq!(
            tracker.record_failure(Duration::from_secs(1)),
            RestartDecision::Exhausted
        );
    }

    #[test]
    fn stable_window_resets_failure_budget() {
        let mut tracker = RestartTracker::default();
        let _ = tracker.record_failure(Duration::ZERO);
        assert_eq!(tracker.attempts(), 1);
        assert!(!tracker.observe_healthy(Duration::from_secs(29)));
        assert_eq!(tracker.attempts(), 1);
        assert!(tracker.observe_healthy(Duration::from_secs(30)));
        assert_eq!(tracker.attempts(), 0);
        assert_eq!(
            tracker.record_failure(Duration::from_secs(31)),
            RestartDecision::RetryAfter {
                attempt: 1,
                delay: Duration::from_millis(250),
            }
        );
    }
}
