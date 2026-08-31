package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// LAN is the tier-2 listener: plain HTTP on one specific local address, for a
// phone that has no Tailscale on it.
//
// This tier exists because "my phone is on the wifi but not on the tailnet" is
// the ordinary case when handing someone a QR code, and the tailnet URL fails
// there with ERR_NAME_NOT_RESOLVED -- MagicDNS is not resolving for a device
// that is not on the tailnet, so the name has no meaning to it.
type LAN struct {
	// Addr is the single address bound. Never 0.0.0.0: a listener that answers
	// on every interface answers on ones nobody audited.
	Addr netip.Addr

	// Iface is the interface Addr belongs to, for the panel to show.
	Iface string
}

// ErrNoLANAddress means no usable private address was found.
var ErrNoLANAddress = errors.New("no private LAN address found on this machine")

// skipPrefixes are interfaces that are never the LAN.
//
// tailscale is excluded because tier 2 exists precisely for clients that cannot
// use it, and binding the tailnet address here would be a second listener on an
// address that already has one. The container and bridge interfaces are
// excluded because their addresses are reachable only from containers, and a QR
// pointing at 172.17.0.1 scans perfectly and then times out.
var skipPrefixes = []string{"tailscale", "docker", "veth", "br-", "virbr", "podman", "cni", "lo"}

// ProbeLAN finds the address to bind for tier 2.
//
// Interfaces are enumerated rather than inferred from the default route. The
// usual trick -- open a UDP socket to a public address and read back the local
// address -- returns the tailnet address whenever an exit node is in use, which
// is exactly when someone is most likely to be relying on this tier.
func ProbeLAN(override string) (*LAN, error) {
	if override != "" {
		addr, err := netip.ParseAddr(override)
		if err != nil {
			return nil, fmt.Errorf("lan.address %q is not an IP address: %w", override, err)
		}
		return &LAN{Addr: addr, Iface: "configured"}, nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if skipInterface(iface.Name) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			prefix, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(prefix.IP.To4())
			if !ok || !addr.IsValid() {
				continue
			}
			// Private only. A public address on a laptop means it is directly
			// on the internet, and this tier speaks plain HTTP.
			if !addr.IsPrivate() {
				continue
			}
			return &LAN{Addr: addr, Iface: iface.Name}, nil
		}
	}
	return nil, ErrNoLANAddress
}

func skipInterface(name string) bool {
	for _, p := range skipPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// URL is where a phone on the same wifi should be pointed.
//
// Always http. The tailnet certificate carries a single SAN for the MagicDNS
// name, so no certificate will ever be valid for a LAN address -- this is a
// permanent property of the tier, not a gap to be closed later.
func (l *LAN) URL(port int) string {
	return "http://" + net.JoinHostPort(l.Addr.String(), strconv.Itoa(port))
}

// Listen binds the single LAN address.
func (l *LAN) Listen(ctx context.Context, port int) (net.Listener, error) {
	var lc net.ListenConfig
	hostPort := net.JoinHostPort(l.Addr.String(), strconv.Itoa(port))
	ln, err := lc.Listen(ctx, "tcp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", hostPort, err)
	}
	return ln, nil
}

// FirewallHint returns a warning when a firewall is likely to drop tier-2
// traffic, or "" when nothing obvious is in the way.
//
// Binding a socket successfully says nothing about whether packets arrive. On a
// machine with ufw enabled the listener comes up, the QR scans, and the phone
// hangs until it times out -- with nothing in any log to explain it. Naming the
// rule is the difference between a five-second fix and an evening.
func (l *LAN) FirewallHint(port int) string {
	if !ufwActive() || ufwAllows(port, l.Iface) {
		return ""
	}
	// Scoped to the one interface rather than opened globally: this port must
	// not become reachable from a tethered phone or a guest network the laptop
	// joins later.
	return fmt.Sprintf("ufw is active and will drop LAN connections to port %d. Allow them with:\n"+
		"    sudo ufw allow in on %s to any port %d proto tcp", port, l.Iface, port)
}

// ufwRules is world-readable (0644), which is what makes this checkable at all
// from a daemon that deliberately does not run as root.
const ufwRules = "/etc/ufw/user.rules"

// ufwAllows reports whether a rule already accepts this port on this interface.
//
// Without it the warning is unconditional, so it keeps telling someone to add a
// rule they added ten minutes ago -- and a warning that is wrong once is a
// warning nobody reads the next time it is right.
func ufwAllows(port int, iface string) bool {
	raw, err := os.ReadFile(ufwRules)
	if err != nil {
		// Cannot tell. Warning is the safer answer: an unnecessary hint costs a
		// line of output, a missing one costs an evening.
		return false
	}
	return rulesAllow(string(raw), port, iface)
}

func rulesAllow(rules string, port int, iface string) bool {
	want := strconv.Itoa(port)

	for _, line := range strings.Split(rules, "\n") {
		// Fields, not substrings. "--dport 7433" is a substring of
		// "--dport 74330", so a Contains check silences the warning on a
		// machine that is still dropping the traffic.
		fields := strings.Fields(line)

		var dport, in string
		accept := false
		for i, f := range fields {
			switch f {
			case "--dport":
				if i+1 < len(fields) {
					dport = fields[i+1]
				}
			case "-i":
				if i+1 < len(fields) {
					in = fields[i+1]
				}
			case "ACCEPT":
				accept = true
			}
		}

		if !accept || dport != want {
			continue
		}
		// An interface-scoped rule must name our interface; a rule with no -i
		// applies to all of them and covers us too.
		if in == "" || in == iface {
			return true
		}
	}
	return false
}

func ufwActive() bool {
	out, err := exec.Command("systemctl", "is-active", "ufw").Output()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}
