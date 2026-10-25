//! Admission control for admin-token verification attempts.
//!
//! Every miss on the verified-token cache costs an Argon2id verification:
//! tens of milliseconds of CPU and ~19 MiB of memory. Three limits apply,
//! cheapest first: a per-client backoff, a global failure gate, and a bound on
//! how many requests may queue for the verification slot.

use std::collections::HashMap;
use std::net::IpAddr;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tokio::sync::{OwnedSemaphorePermit, Semaphore};

const FREE_ATTEMPTS: u32 = 5;
const BASE_BACKOFF: Duration = Duration::from_secs(1);
const MAX_BACKOFF: Duration = Duration::from_secs(60);
const CLIENT_RESET_AFTER: Duration = Duration::from_secs(15 * 60);
const MAX_TRACKED_CLIENTS: usize = 4096;
const GLOBAL_WINDOW: Duration = Duration::from_secs(60);
const GLOBAL_THRESHOLD: u32 = 100;
const GLOBAL_COOLDOWN: Duration = Duration::from_secs(30);

/// Bounds the requests allowed to queue for the verification slot.
pub const MAX_VERIFICATION_WAITERS: usize = 8;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ClientKind {
    /// A caller on the local machine, which is already trusted.
    Loopback,
    /// A caller with a known non-loopback address.
    Remote,
    /// A caller whose peer address could not be determined.
    Unknown,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Client {
    pub kind: ClientKind,
    pub addr: Option<IpAddr>,
}

impl Client {
    pub fn from_addr(addr: IpAddr) -> Self {
        if addr.is_loopback() {
            Self {
                kind: ClientKind::Loopback,
                addr: Some(addr),
            }
        } else {
            Self {
                kind: ClientKind::Remote,
                addr: Some(addr),
            }
        }
    }

    pub fn unknown() -> Self {
        Self {
            kind: ClientKind::Unknown,
            addr: None,
        }
    }

    fn key(&self) -> Option<IpAddr> {
        if self.kind == ClientKind::Remote {
            self.addr
        } else {
            None
        }
    }
}

#[derive(Debug, Clone)]
struct ClientState {
    failures: u32,
    blocked_until: Option<Instant>,
    last_failure: Instant,
}

#[derive(Debug, Clone)]
struct GlobalState {
    window_start: Instant,
    failures: u32,
    cooldown_until: Option<Instant>,
}

struct Gates {
    remote: GlobalState,
    loopback: GlobalState,
}

type Now = dyn Fn() -> Instant + Send + Sync;

pub struct Throttle {
    clients: Mutex<HashMap<IpAddr, ClientState>>,
    gates: Mutex<Gates>,
    now: Arc<Now>,
}

impl Throttle {
    pub fn new() -> Self {
        Self::with_clock(Instant::now)
    }

    pub fn with_clock(now: impl Fn() -> Instant + Send + Sync + 'static) -> Self {
        let now = Arc::new(now);
        let start = now();
        Self {
            clients: Mutex::new(HashMap::new()),
            gates: Mutex::new(Gates {
                remote: GlobalState {
                    window_start: start,
                    failures: 0,
                    cooldown_until: None,
                },
                loopback: GlobalState {
                    window_start: start,
                    failures: 0,
                    cooldown_until: None,
                },
            }),
            now,
        }
    }

    /// Reports whether the client may pay for a verification right now.
    pub fn admit(&self, client: Client) -> bool {
        let now = (self.now)();

        {
            let mut gates = self.gates.lock().unwrap_or_else(|error| error.into_inner());
            let gate = if client.kind == ClientKind::Loopback {
                &mut gates.loopback
            } else {
                &mut gates.remote
            };
            if let Some(until) = gate.cooldown_until {
                if now < until {
                    return false;
                }
            }
            gate.cooldown_until = None;
        }

        let Some(addr) = client.key() else {
            return true;
        };

        let clients = self
            .clients
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        match clients.get(&addr).and_then(|state| state.blocked_until) {
            Some(until) => now >= until,
            None => true,
        }
    }

    /// Clears a client's failure streak after a real verification succeeded.
    pub fn record_success(&self, client: Client) {
        let Some(addr) = client.key() else {
            return;
        };
        let mut clients = self
            .clients
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        clients.remove(&addr);
    }

    /// Charges a failed verification to the client and the global gate.
    pub fn record_failure(&self, client: Client) {
        let now = (self.now)();

        {
            let mut gates = self.gates.lock().unwrap_or_else(|error| error.into_inner());
            let gate = if client.kind == ClientKind::Loopback {
                &mut gates.loopback
            } else {
                &mut gates.remote
            };
            if now.saturating_duration_since(gate.window_start) >= GLOBAL_WINDOW {
                gate.window_start = now;
                gate.failures = 0;
            }
            gate.failures += 1;
            if gate.failures >= GLOBAL_THRESHOLD {
                gate.cooldown_until = Some(now + GLOBAL_COOLDOWN);
                gate.window_start = now;
                gate.failures = 0;
            }
        }

        let Some(addr) = client.key() else {
            return;
        };

        let mut clients = self
            .clients
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        let has_room = prune(&mut clients, now);
        if !has_room && !clients.contains_key(&addr) {
            // Every slot is held by a client still being penalised.
            return;
        }
        let state = clients.entry(addr).or_insert(ClientState {
            failures: 0,
            blocked_until: None,
            last_failure: now,
        });

        if now.saturating_duration_since(state.last_failure) >= CLIENT_RESET_AFTER {
            state.failures = 0;
        }
        state.failures += 1;
        state.last_failure = now;
        state.blocked_until = backoff(state.failures, now);
    }

    #[cfg(test)]
    fn tracked_clients(&self) -> usize {
        let clients = self
            .clients
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        clients.len()
    }
}

impl Default for Throttle {
    fn default() -> Self {
        Self::new()
    }
}

/// Returns the delay for the failures-th consecutive failure, if penalized.
fn backoff(failures: u32, now: Instant) -> Option<Instant> {
    if failures <= FREE_ATTEMPTS {
        return None;
    }
    let exponent = failures - FREE_ATTEMPTS - 1;
    let base_millis = BASE_BACKOFF.as_millis() as u64;
    let delay = if exponent >= 63 {
        MAX_BACKOFF
    } else {
        base_millis
            .checked_shl(exponent)
            .map(|millis| Duration::from_millis(millis).min(MAX_BACKOFF))
            .unwrap_or(MAX_BACKOFF)
    };
    Some(now + delay)
}

/// Drops entries that have gone quiet and reports whether there is room for
/// another client.
fn prune(clients: &mut HashMap<IpAddr, ClientState>, now: Instant) -> bool {
    if clients.len() < MAX_TRACKED_CLIENTS {
        return true;
    }
    clients.retain(|_, state| {
        let quiet = now.saturating_duration_since(state.last_failure) >= CLIENT_RESET_AFTER;
        let still_blocked = state.blocked_until.is_some_and(|until| now < until);
        !(quiet && !still_blocked)
    });
    clients.len() < MAX_TRACKED_CLIENTS
}

/// Bounds the queue for the single verification slot.
pub struct VerificationQueue {
    waiting: std::sync::atomic::AtomicUsize,
    slot: Arc<Semaphore>,
}

impl VerificationQueue {
    pub fn new() -> Self {
        Self {
            waiting: std::sync::atomic::AtomicUsize::new(0),
            slot: Arc::new(Semaphore::new(1)),
        }
    }

    /// Waits for the verification slot; `None` means too many requests are
    /// already queued for it.
    pub async fn enter(&self) -> Option<OwnedSemaphorePermit> {
        use std::sync::atomic::Ordering;

        let previous = self.waiting.fetch_add(1, Ordering::SeqCst);
        if previous >= MAX_VERIFICATION_WAITERS {
            self.waiting.fetch_sub(1, Ordering::SeqCst);
            return None;
        }
        let permit = self.slot.clone().acquire_owned().await.ok()?;
        self.waiting.fetch_sub(1, Ordering::SeqCst);
        Some(permit)
    }
}

impl Default for VerificationQueue {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::{Ipv4Addr, Ipv6Addr};

    fn fake_clock() -> (
        Arc<Mutex<Instant>>,
        impl Fn() -> Instant + Send + Sync + 'static,
    ) {
        let clock = Arc::new(Mutex::new(Instant::now()));
        let handle = Arc::clone(&clock);
        let now = move || *handle.lock().unwrap_or_else(|error| error.into_inner());
        (clock, now)
    }

    fn advance(clock: &Arc<Mutex<Instant>>, duration: Duration) {
        let mut now = clock.lock().unwrap();
        *now += duration;
    }

    fn remote(third: u8) -> Client {
        Client::from_addr(IpAddr::V4(Ipv4Addr::new(203, 0, 113, third)))
    }

    #[test]
    fn five_failures_are_free_and_the_sixth_backs_off() {
        let (clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);
        let client = remote(1);

        for _ in 0..FREE_ATTEMPTS {
            throttle.record_failure(client);
        }
        assert!(throttle.admit(client));

        throttle.record_failure(client);
        assert!(!throttle.admit(client));

        advance(&clock, Duration::from_secs(1));
        assert!(throttle.admit(client));
    }

    #[test]
    fn backoff_doubles_and_caps() {
        let (clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);
        let client = remote(2);

        for _ in 0..6 {
            throttle.record_failure(client);
        }
        assert!(!throttle.admit(client));
        advance(&clock, Duration::from_secs(1));
        assert!(throttle.admit(client));

        throttle.record_failure(client);
        assert!(!throttle.admit(client));
        advance(&clock, Duration::from_secs(1));
        assert!(!throttle.admit(client));
        advance(&clock, Duration::from_secs(1));
        assert!(throttle.admit(client));
    }

    #[test]
    fn a_quiet_client_forgets_its_streak() {
        let (clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);
        let client = remote(3);

        for _ in 0..6 {
            throttle.record_failure(client);
        }
        assert!(!throttle.admit(client));
        advance(&clock, CLIENT_RESET_AFTER + Duration::from_secs(1));
        throttle.record_failure(client);
        assert!(throttle.admit(client));
    }

    #[test]
    fn success_clears_the_streak() {
        let (_clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);
        let client = remote(4);

        for _ in 0..6 {
            throttle.record_failure(client);
        }
        assert!(!throttle.admit(client));
        throttle.record_success(client);
        assert!(throttle.admit(client));
    }

    #[test]
    fn the_global_gate_cools_down_and_keeps_loopback_separate() {
        let (clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);

        for index in 0..GLOBAL_THRESHOLD {
            throttle.record_failure(remote((index % 250) as u8 + 1));
        }
        assert!(!throttle.admit(remote(200)));

        // Loopback has its own counter, so the remote cooldown does not lock
        // the operator out.
        let operator = Client::from_addr(IpAddr::V6(Ipv6Addr::LOCALHOST));
        assert!(throttle.admit(operator));

        advance(&clock, GLOBAL_COOLDOWN + Duration::from_secs(1));
        assert!(throttle.admit(remote(200)));
    }

    #[test]
    fn loopback_is_never_tracked_individually() {
        let (_clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);
        let operator = Client::from_addr(IpAddr::V4(Ipv4Addr::LOCALHOST));

        for _ in 0..10 {
            throttle.record_failure(operator);
        }
        assert!(throttle.admit(operator));
        assert_eq!(throttle.tracked_clients(), 0);
    }

    #[test]
    fn tracked_clients_are_bounded() {
        let (_clock, now) = fake_clock();
        let throttle = Throttle::with_clock(now);

        for index in 0..(MAX_TRACKED_CLIENTS + 50) {
            let addr = IpAddr::V4(Ipv4Addr::new(
                10,
                ((index >> 16) & 0xff) as u8,
                ((index >> 8) & 0xff) as u8,
                (index & 0xff) as u8,
            ));
            throttle.record_failure(Client::from_addr(addr));
        }
        assert!(throttle.tracked_clients() <= MAX_TRACKED_CLIENTS);
    }
}
