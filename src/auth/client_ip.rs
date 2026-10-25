//! Resolving the caller's address for throttling and logging.

use std::net::{IpAddr, Ipv4Addr, SocketAddr};

use crate::auth::throttle::Client;

/// Parses the address of the machine that opened the connection.
pub fn parse_peer_addr(remote: &str) -> Option<IpAddr> {
    let value = remote.trim();
    if value.is_empty() {
        return None;
    }
    if let Ok(socket) = value.parse::<SocketAddr>() {
        return Some(normalize(socket.ip()));
    }
    if let Ok(addr) = value.parse::<IpAddr>() {
        return Some(normalize(addr));
    }
    if let Some(rest) = value.strip_prefix('[') {
        if let Some((host, _)) = rest.split_once(']') {
            if let Ok(addr) = host.parse::<IpAddr>() {
                return Some(normalize(addr));
            }
        }
    }
    if let Some((host, _)) = value.rsplit_once(':') {
        if let Ok(addr) = host.parse::<IpAddr>() {
            return Some(normalize(addr));
        }
    }
    None
}

/// Accepts the bare address, `host:port` and `[v6]:port` forms.
pub fn parse_forwarded_addr(value: &str) -> Option<IpAddr> {
    let value = value.trim();
    if value.is_empty() {
        return None;
    }
    if let Ok(addr) = value.parse::<IpAddr>() {
        return Some(normalize(addr));
    }
    if let Ok(socket) = value.parse::<SocketAddr>() {
        return Some(normalize(socket.ip()));
    }
    if let Some(rest) = value.strip_prefix('[') {
        if let Some((host, _)) = rest.split_once(']') {
            if let Ok(addr) = host.parse::<IpAddr>() {
                return Some(normalize(addr));
            }
        }
    }
    if let Some((host, _)) = value.rsplit_once(':') {
        if let Ok(addr) = host.parse::<IpAddr>() {
            return Some(normalize(addr));
        }
    }
    None
}

/// Reads the last comma-separated entry, which is the one the proxy wrote.
pub fn last_forwarded_addr(value: &str) -> Option<IpAddr> {
    parse_forwarded_addr(value.rsplit(',').next().unwrap_or(""))
}

/// Decides which address identifies the caller.
///
/// A forwarded header is consulted only when the operator named one and the
/// connection came from somewhere a reverse proxy could be. An address learned
/// that way is always treated as remote.
pub fn resolve_client(
    peer: Option<IpAddr>,
    client_ip_header_configured: bool,
    forwarded: Option<&str>,
) -> Client {
    if client_ip_header_configured {
        if let Some(peer_addr) = peer {
            if could_be_proxy(peer_addr) {
                if let Some(value) = forwarded {
                    if let Some(addr) = last_forwarded_addr(value) {
                        return Client::from_addr(addr);
                    }
                }
            }
        }
    }

    match peer {
        Some(addr) => Client::from_addr(addr),
        None => Client::unknown(),
    }
}

/// Whether a connection plausibly came from the operator's own reverse proxy.
pub fn could_be_proxy(addr: IpAddr) -> bool {
    match addr {
        IpAddr::V4(v4) => {
            v4.is_loopback() || v4.is_private() || v4.is_link_local() || is_carrier_grade_nat(v4)
        }
        IpAddr::V6(v6) => v6.is_loopback() || v6.is_unique_local() || v6.is_unicast_link_local(),
    }
}

fn is_carrier_grade_nat(addr: Ipv4Addr) -> bool {
    let octets = addr.octets();
    octets[0] == 100 && (octets[1] & 0xc0) == 64
}

fn normalize(addr: IpAddr) -> IpAddr {
    match addr {
        IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
            Some(v4) => IpAddr::V4(v4),
            None => IpAddr::V6(v6),
        },
        v4 => v4,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::throttle::ClientKind;

    #[test]
    fn peer_addresses_are_parsed_and_unmapped() {
        assert_eq!(
            parse_peer_addr("203.0.113.9:4567"),
            Some(IpAddr::V4(Ipv4Addr::new(203, 0, 113, 9)))
        );
        assert_eq!(
            parse_peer_addr("[2001:db8::1]:4567"),
            Some(IpAddr::V6("2001:db8::1".parse().unwrap()))
        );
        assert_eq!(
            parse_peer_addr("::ffff:203.0.113.9"),
            Some(IpAddr::V4(Ipv4Addr::new(203, 0, 113, 9)))
        );
        assert_eq!(parse_peer_addr("nonsense"), None);
    }

    #[test]
    fn forwarded_addresses_accept_the_shapes_proxies_emit() {
        assert_eq!(
            parse_forwarded_addr(" 203.0.113.9 "),
            Some(IpAddr::V4(Ipv4Addr::new(203, 0, 113, 9)))
        );
        assert_eq!(
            parse_forwarded_addr("203.0.113.9:80"),
            Some(IpAddr::V4(Ipv4Addr::new(203, 0, 113, 9)))
        );
        assert_eq!(
            parse_forwarded_addr("[2001:db8::1]:80"),
            Some(IpAddr::V6("2001:db8::1".parse().unwrap()))
        );
        assert_eq!(
            parse_forwarded_addr("[2001:db8::1]"),
            Some(IpAddr::V6("2001:db8::1".parse().unwrap()))
        );
        assert_eq!(
            last_forwarded_addr("198.51.100.1, 203.0.113.9"),
            Some(IpAddr::V4(Ipv4Addr::new(203, 0, 113, 9)))
        );
        assert_eq!(parse_forwarded_addr(""), None);
    }

    #[test]
    fn proxy_peers_cover_private_and_mesh_ranges() {
        for value in [
            "127.0.0.1",
            "10.1.2.3",
            "172.16.0.1",
            "192.168.1.1",
            "169.254.1.1",
            "100.64.0.1",
            "100.127.255.254",
            "::1",
            "fc00::1",
            "fe80::1",
        ] {
            let addr: IpAddr = value.parse().unwrap();
            assert!(could_be_proxy(addr), "{value}");
        }
        for value in ["203.0.113.9", "8.8.8.8", "100.128.0.1", "2001:db8::1"] {
            let addr: IpAddr = value.parse().unwrap();
            assert!(!could_be_proxy(addr), "{value}");
        }
    }

    #[test]
    fn forwarded_header_is_only_trusted_from_a_proxy_peer() {
        let forwarded = Some("203.0.113.9");
        let remote_peer: IpAddr = "198.51.100.7".parse().unwrap();
        let client = resolve_client(Some(remote_peer), true, forwarded);
        assert_eq!(client.kind, ClientKind::Remote);
        assert_eq!(client.addr, Some(remote_peer));

        let proxy_peer: IpAddr = "127.0.0.1".parse().unwrap();
        let client = resolve_client(Some(proxy_peer), true, forwarded);
        assert_eq!(client.kind, ClientKind::Remote);
        assert_eq!(client.addr, Some(IpAddr::V4(Ipv4Addr::new(203, 0, 113, 9))));

        let client = resolve_client(Some(proxy_peer), false, forwarded);
        assert_eq!(client.kind, ClientKind::Loopback);
    }

    #[test]
    fn an_unresolved_peer_is_unknown() {
        let client = resolve_client(None, true, Some("203.0.113.9"));
        assert_eq!(client.kind, ClientKind::Unknown);
        assert_eq!(client.addr, None);
    }
}
