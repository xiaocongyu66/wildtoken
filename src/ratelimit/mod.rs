//! Token and channel rate-limit expressions.

pub mod parser;

pub use parser::parse_rate_limit;
pub use parser::RateLimit;
