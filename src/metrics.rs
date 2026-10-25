//! In-process counters reported by the admin console.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

const RECENT_DISCONNECT_WINDOW: Duration = Duration::from_secs(10 * 60);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CleanupSnapshot {
    pub active: bool,
    pub runs_total: u64,
    pub errors_total: u64,
    pub rows_cleared_total: u64,
    pub batches_total: u64,
    pub current_rows_cleared: u64,
    pub current_batches: u64,
    pub last_started_unix_seconds: Option<i64>,
    pub last_finished_unix_seconds: Option<i64>,
    pub last_duration_ms: Option<u64>,
    pub last_rows_cleared: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Snapshot {
    pub active_sse_streams: u64,
    pub sse_completed_total: u64,
    pub sse_client_disconnects_total: u64,
    pub sse_recent_disconnects_10m: u64,
    pub sse_upstream_errors_total: u64,
    pub log_queue_depth: u64,
    pub log_written_total: u64,
    pub log_write_batches_total: u64,
    pub log_dropped_total: u64,
    pub log_write_failures_total: u64,
    pub slow_db_operations_total: u64,
    pub cleanup: CleanupSnapshot,
}

#[derive(Default)]
struct CleanupState {
    active: AtomicBool,
    runs_total: AtomicU64,
    errors_total: AtomicU64,
    rows_cleared_total: AtomicU64,
    batches_total: AtomicU64,
    current_rows_cleared: AtomicU64,
    current_batches: AtomicU64,
    last_started_unix_seconds: AtomicI64,
    last_finished_unix_seconds: AtomicI64,
    last_duration_ms: AtomicU64,
    last_rows_cleared: AtomicU64,
}

const UNSET: i64 = i64::MIN;

impl Default for Runtime {
    fn default() -> Self {
        Self::new()
    }
}

pub struct Runtime {
    active_sse_streams: AtomicI64,
    sse_completed_total: AtomicU64,
    sse_client_disconnects_total: AtomicU64,
    sse_upstream_errors_total: AtomicU64,
    recent_disconnects: Mutex<VecDeque<Instant>>,
    log_queue_depth: AtomicU64,
    log_written_total: AtomicU64,
    log_write_batches_total: AtomicU64,
    log_dropped_total: AtomicU64,
    log_write_failures_total: AtomicU64,
    slow_db_operations_total: AtomicU64,
    cleanup: CleanupState,
}

impl Runtime {
    pub fn new() -> Self {
        Self {
            active_sse_streams: AtomicI64::new(0),
            sse_completed_total: AtomicU64::new(0),
            sse_client_disconnects_total: AtomicU64::new(0),
            sse_upstream_errors_total: AtomicU64::new(0),
            recent_disconnects: Mutex::new(VecDeque::new()),
            log_queue_depth: AtomicU64::new(0),
            log_written_total: AtomicU64::new(0),
            log_write_batches_total: AtomicU64::new(0),
            log_dropped_total: AtomicU64::new(0),
            log_write_failures_total: AtomicU64::new(0),
            slow_db_operations_total: AtomicU64::new(0),
            cleanup: CleanupState {
                last_started_unix_seconds: AtomicI64::new(UNSET),
                last_finished_unix_seconds: AtomicI64::new(UNSET),
                ..CleanupState::default()
            },
        }
    }

    pub fn snapshot(&self) -> Snapshot {
        let recent = self.recent_disconnects_10m();
        let started = self
            .cleanup
            .last_started_unix_seconds
            .load(Ordering::Relaxed);
        let finished = self
            .cleanup
            .last_finished_unix_seconds
            .load(Ordering::Relaxed);
        let duration = self.cleanup.last_duration_ms.load(Ordering::Relaxed);

        Snapshot {
            active_sse_streams: self.active_sse_streams.load(Ordering::Relaxed).max(0) as u64,
            sse_completed_total: self.sse_completed_total.load(Ordering::Relaxed),
            sse_client_disconnects_total: self.sse_client_disconnects_total.load(Ordering::Relaxed),
            sse_recent_disconnects_10m: recent,
            sse_upstream_errors_total: self.sse_upstream_errors_total.load(Ordering::Relaxed),
            log_queue_depth: self.log_queue_depth.load(Ordering::Relaxed),
            log_written_total: self.log_written_total.load(Ordering::Relaxed),
            log_write_batches_total: self.log_write_batches_total.load(Ordering::Relaxed),
            log_dropped_total: self.log_dropped_total.load(Ordering::Relaxed),
            log_write_failures_total: self.log_write_failures_total.load(Ordering::Relaxed),
            slow_db_operations_total: self.slow_db_operations_total.load(Ordering::Relaxed),
            cleanup: CleanupSnapshot {
                active: self.cleanup.active.load(Ordering::Relaxed),
                runs_total: self.cleanup.runs_total.load(Ordering::Relaxed),
                errors_total: self.cleanup.errors_total.load(Ordering::Relaxed),
                rows_cleared_total: self.cleanup.rows_cleared_total.load(Ordering::Relaxed),
                batches_total: self.cleanup.batches_total.load(Ordering::Relaxed),
                current_rows_cleared: self.cleanup.current_rows_cleared.load(Ordering::Relaxed),
                current_batches: self.cleanup.current_batches.load(Ordering::Relaxed),
                last_started_unix_seconds: (started != UNSET).then_some(started),
                last_finished_unix_seconds: (finished != UNSET).then_some(finished),
                last_duration_ms: (duration != 0).then_some(duration),
                last_rows_cleared: self.cleanup.last_rows_cleared.load(Ordering::Relaxed),
            },
        }
    }

    pub fn sse_stream_started(&self) {
        self.active_sse_streams.fetch_add(1, Ordering::Relaxed);
    }

    pub fn sse_stream_finished(&self, completed: bool) {
        self.active_sse_streams.fetch_sub(1, Ordering::Relaxed);
        if completed {
            self.sse_completed_total.fetch_add(1, Ordering::Relaxed);
        }
    }

    pub fn sse_client_disconnected(&self) {
        self.sse_client_disconnects_total
            .fetch_add(1, Ordering::Relaxed);
        let mut recent = self
            .recent_disconnects
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        recent.push_back(Instant::now());
    }

    pub fn sse_upstream_error(&self) {
        self.sse_upstream_errors_total
            .fetch_add(1, Ordering::Relaxed);
    }

    pub fn set_log_queue_depth(&self, depth: u64) {
        self.log_queue_depth.store(depth, Ordering::Relaxed);
    }

    pub fn record_log_written(&self, count: u64) {
        self.log_written_total.fetch_add(count, Ordering::Relaxed);
    }

    pub fn record_log_batch(&self) {
        self.log_write_batches_total.fetch_add(1, Ordering::Relaxed);
        let _ = self
            .log_queue_depth
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |depth| {
                Some(depth.saturating_sub(1))
            });
    }

    pub fn record_log_dropped(&self) {
        self.log_dropped_total.fetch_add(1, Ordering::Relaxed);
    }

    pub fn record_log_write_failure(&self, count: u64) {
        self.log_write_failures_total
            .fetch_add(count, Ordering::Relaxed);
    }

    pub fn record_slow_db_operation(&self) {
        self.slow_db_operations_total
            .fetch_add(1, Ordering::Relaxed);
    }

    pub fn begin_cleanup(&self, unix_seconds: i64) {
        self.cleanup.active.store(true, Ordering::Relaxed);
        self.cleanup
            .last_started_unix_seconds
            .store(unix_seconds, Ordering::Relaxed);
        self.cleanup
            .current_rows_cleared
            .store(0, Ordering::Relaxed);
        self.cleanup.current_batches.store(0, Ordering::Relaxed);
    }

    pub fn record_cleanup_batch(&self, rows_cleared: u64) {
        self.cleanup
            .current_rows_cleared
            .fetch_add(rows_cleared, Ordering::Relaxed);
        self.cleanup.current_batches.fetch_add(1, Ordering::Relaxed);
    }

    pub fn finish_cleanup(&self, success: bool, unix_seconds: i64, duration_ms: u64) {
        let rows = self.cleanup.current_rows_cleared.load(Ordering::Relaxed);
        self.cleanup.active.store(false, Ordering::Relaxed);
        self.cleanup.runs_total.fetch_add(1, Ordering::Relaxed);
        if !success {
            self.cleanup.errors_total.fetch_add(1, Ordering::Relaxed);
        }
        self.cleanup
            .rows_cleared_total
            .fetch_add(rows, Ordering::Relaxed);
        self.cleanup.batches_total.fetch_add(
            self.cleanup.current_batches.load(Ordering::Relaxed),
            Ordering::Relaxed,
        );
        self.cleanup
            .last_rows_cleared
            .store(rows, Ordering::Relaxed);
        self.cleanup
            .last_finished_unix_seconds
            .store(unix_seconds, Ordering::Relaxed);
        self.cleanup
            .last_duration_ms
            .store(duration_ms, Ordering::Relaxed);
    }

    fn recent_disconnects_10m(&self) -> u64 {
        let cutoff = Instant::now()
            .checked_sub(RECENT_DISCONNECT_WINDOW)
            .unwrap_or_else(Instant::now);
        let mut recent = self
            .recent_disconnects
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        while recent.front().is_some_and(|started| *started < cutoff) {
            recent.pop_front();
        }
        recent.len() as u64
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counters_round_trip() {
        let runtime = Runtime::new();
        runtime.sse_stream_started();
        runtime.sse_stream_started();
        runtime.sse_stream_finished(true);
        runtime.sse_client_disconnected();
        runtime.sse_upstream_error();
        runtime.set_log_queue_depth(3);
        runtime.record_log_written(5);
        runtime.record_log_batch();
        runtime.record_log_dropped();
        runtime.record_log_write_failure(2);
        runtime.record_slow_db_operation();
        runtime.begin_cleanup(1_700_000_000);
        runtime.record_cleanup_batch(10);
        runtime.finish_cleanup(true, 1_700_000_001, 25);

        let snapshot = runtime.snapshot();
        assert_eq!(snapshot.active_sse_streams, 1);
        assert_eq!(snapshot.sse_completed_total, 1);
        assert_eq!(snapshot.sse_client_disconnects_total, 1);
        assert_eq!(snapshot.sse_recent_disconnects_10m, 1);
        assert_eq!(snapshot.sse_upstream_errors_total, 1);
        assert_eq!(snapshot.log_queue_depth, 2);
        assert_eq!(snapshot.log_written_total, 5);
        assert_eq!(snapshot.log_write_batches_total, 1);
        assert_eq!(snapshot.log_dropped_total, 1);
        assert_eq!(snapshot.log_write_failures_total, 2);
        assert_eq!(snapshot.slow_db_operations_total, 1);
        assert_eq!(snapshot.cleanup.runs_total, 1);
        assert_eq!(snapshot.cleanup.rows_cleared_total, 10);
        assert_eq!(snapshot.cleanup.batches_total, 1);
        assert_eq!(snapshot.cleanup.last_rows_cleared, 10);
        assert_eq!(
            snapshot.cleanup.last_started_unix_seconds,
            Some(1_700_000_000)
        );
        assert_eq!(
            snapshot.cleanup.last_finished_unix_seconds,
            Some(1_700_000_001)
        );
        assert_eq!(snapshot.cleanup.last_duration_ms, Some(25));
    }
}
