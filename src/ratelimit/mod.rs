//! Token and channel rate-limit expressions.

pub mod parser;

pub use parser::{RateLimit, parse_rate_limit};
