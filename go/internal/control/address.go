package control

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// matchesAddress reports whether addr falls inside prefix.
//
// prefix is either a bare address or CIDR notation. addr is what the relay saw,
// which may carry a port and may be either family.
//
// The IPv4-in-IPv6 case is the one that bites: a relay bound to a dual-stack
// socket reports an IPv4 client as ::ffff:203.0.113.9, so a ban written as
// 203.0.113.0/24 has to match it. Comparing the strings, or the families, gets
// this wrong in the direction of letting a banned address through.
func matchesAddress(prefix, addr string) bool {
	if prefix == "" || addr == "" {
		return false
	}
	target, ok := parseAddress(addr)
	if !ok {
		return false
	}
	target = target.Unmap()

	if network, err := netip.ParsePrefix(prefix); err == nil {
		return network.Contains(canonicalFor(network, target))
	}
	single, err := netip.ParseAddr(prefix)
	if err != nil {
		return false
	}
	return single.Unmap() == target
}

// canonicalFor puts target in the same family as the prefix it is tested
// against, so a v4 address is comparable to a v4 prefix however it arrived.
func canonicalFor(network netip.Prefix, target netip.Addr) netip.Addr {
	if network.Addr().Is4() && target.Is4In6() {
		return target.Unmap()
	}
	if network.Addr().Is6() && target.Is4() {
		return netip.AddrFrom16(target.As16())
	}
	return target
}

func parseAddress(addr string) (netip.Addr, bool) {
	if parsed, err := netip.ParseAddr(addr); err == nil {
		return parsed, true
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if parsed, err := netip.ParseAddr(host); err == nil {
			return parsed, true
		}
	}
	// A bracketed IPv6 literal with no port, which is how the relay's own
	// listen address is written and therefore what someone will paste in.
	trimmed := strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
	parsed, err := netip.ParseAddr(trimmed)
	return parsed, err == nil
}

// ValidateBanSubject rejects a ban that cannot match anything.
//
// Worth doing at the edge: a malformed CIDR silently matches nothing, so the API
// would report success for a ban that does not exist. A moderator would then
// watch the person carry on and conclude the platform is broken -- which, in
// that moment, it would be.
func ValidateBanSubject(b Ban) error {
	switch b.Kind {
	case BanByUsername:
		if b.Username == "" {
			return fmt.Errorf("%w: ban by username needs a username", ErrInvalid)
		}
	case BanByDevice:
		if b.Username == "" || b.Device == "" {
			return fmt.Errorf("%w: ban by device needs a username and a device", ErrInvalid)
		}
	case BanByAddress:
		if _, err := netip.ParsePrefix(b.IPPrefix); err != nil {
			if _, err := netip.ParseAddr(b.IPPrefix); err != nil {
				return fmt.Errorf("%w: %q is neither an address nor a CIDR prefix", ErrInvalid, b.IPPrefix)
			}
		}
	default:
		return fmt.Errorf("%w: unknown ban kind %q", ErrInvalid, b.Kind)
	}
	return nil
}
