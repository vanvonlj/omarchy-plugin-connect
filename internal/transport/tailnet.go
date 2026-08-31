// Package transport brings up the daemon's listeners.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// Tailnet is the daemon's view of the local Tailscale node.
type Tailnet struct {
	// Client is the LocalAPI handle. It carries both halves of the design:
	// WhoIs for admission and GetCertificate for TLS.
	Client *local.Client

	// DNSName is the MagicDNS name without its trailing dot, e.g.
	// "omarchy-starfighter.tail18c58.ts.net".
	DNSName string

	// Addrs are the node's Tailscale addresses.
	Addrs []netip.Addr

	// UserID owns this node; admission compares callers against it.
	UserID tailcfg.UserID

	// CertsAvailable reports whether the control plane will issue a TLS
	// certificate for DNSName. When false the daemon still serves, over plain
	// HTTP on the tailnet, and says so loudly.
	CertsAvailable bool
}

// ErrNoLocalAPI means tailscaled could not be reached. It is called out
// separately because the fix is a specific command, not a general "is
// Tailscale working" investigation.
var ErrNoLocalAPI = errors.New("cannot reach the Tailscale LocalAPI")

// Probe inspects the local Tailscale node and reports what the daemon can do
// with it.
//
// Every failure here is a precondition failure with a known remedy, so each one
// names the remedy rather than returning the underlying error alone.
func Probe(ctx context.Context) (*Tailnet, error) {
	lc := &local.Client{}

	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		// The overwhelmingly common cause is the operator not being set: the
		// LocalAPI socket is root-owned, and this daemon deliberately runs as a
		// systemd *user* unit. Saying so here saves reading a permissions error
		// from two layers down.
		return nil, fmt.Errorf("%w: %v\n\nIf Tailscale is running, the operator is probably not set:\n    sudo tailscale set --operator=$USER", ErrNoLocalAPI, err)
	}

	if st.BackendState != ipn.Running.String() {
		return nil, fmt.Errorf("Tailscale is not running (state: %s); try: tailscale up", st.BackendState)
	}
	if st.Self == nil {
		return nil, errors.New("Tailscale reported no status for this node")
	}

	dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
	if dnsName == "" {
		return nil, errors.New("this node has no MagicDNS name; enable MagicDNS in the admin console")
	}

	tn := &Tailnet{
		Client:         lc,
		DNSName:        dnsName,
		Addrs:          st.Self.TailscaleIPs,
		UserID:         st.Self.UserID,
		CertsAvailable: certsAvailable(st, dnsName),
	}
	if len(tn.Addrs) == 0 {
		return nil, errors.New("this node has no Tailscale addresses")
	}
	return tn, nil
}

// certsAvailable reports whether the control plane advertises a cert for our
// name. CertDomains is empty on a tailnet that has not enabled HTTPS in the
// admin console, which is the difference between a PWA and a bare terminal.
func certsAvailable(st *ipnstate.Status, dnsName string) bool {
	for _, d := range st.CertDomains {
		if strings.EqualFold(strings.TrimSuffix(d, "."), dnsName) {
			return true
		}
	}
	return false
}

// URL is where a browser should be pointed.
func (t *Tailnet) URL(port int) string {
	scheme := "http"
	if t.CertsAvailable {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(t.DNSName, strconv.Itoa(port))
}

// TLSConfig returns a TLS config that sources certificates from the LocalAPI,
// or nil when the tailnet cannot issue them.
//
// Certificates are fetched and renewed inside the handshake rather than by this
// daemon. tailscaled caches them on disk and reissues before expiry, so there
// is no renewal timer to get wrong and no certificate file to leave stale on a
// machine nobody logs into.
func (t *Tailnet) TLSConfig() *tls.Config {
	if !t.CertsAvailable {
		return nil
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: t.getCertificate,
	}
}

// getCertificate wraps the LocalAPI lookup to survive a handshake with no SNI.
//
// A browser pointed at https://100.98.170.119:7433 sends no server name, so an
// unwrapped lookup fails and OpenSSL reports it to the user as "tlsv1 alert
// internal error" -- which says nothing about what to do next. Substituting our
// own MagicDNS name means the handshake completes and the browser can raise the
// error it should have raised all along: this certificate is for
// omarchy-starfighter.tail18c58.ts.net, and you asked for an IP. That is a
// mismatch anyone can act on.
func (t *Tailnet) getCertificate(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hi.ServerName == "" {
		withName := *hi
		withName.ServerName = t.DNSName
		return t.Client.GetCertificate(&withName)
	}
	return t.Client.GetCertificate(hi)
}

// WarmCert fetches the certificate once so the first visitor does not wait on
// an ACME round trip mid-handshake.
//
// A failure is returned for logging, never for aborting: a daemon that refuses
// to start because a certificate was slow is worse than one that serves the
// first request a little late.
func (t *Tailnet) WarmCert(ctx context.Context) error {
	if !t.CertsAvailable {
		return nil
	}
	_, err := t.Client.GetCertificate(&tls.ClientHelloInfo{
		ServerName: t.DNSName,
		Conn:       nil,
	})
	if err != nil {
		return fmt.Errorf("warming certificate for %s: %w", t.DNSName, err)
	}
	return nil
}

// Listen binds one listener per Tailscale address on the given port.
//
// Binding the specific addresses, never 0.0.0.0, is a security property: the
// tailnet listener must be unreachable from a coffee-shop network even if the
// firewall is misconfigured. The caller closes every returned listener.
func (t *Tailnet) Listen(ctx context.Context, port int) ([]net.Listener, error) {
	var lns []net.Listener
	var lc net.ListenConfig

	for _, addr := range t.Addrs {
		hostPort := net.JoinHostPort(addr.String(), strconv.Itoa(port))
		ln, err := lc.Listen(ctx, "tcp", hostPort)
		if err != nil {
			for _, open := range lns {
				open.Close()
			}
			return nil, fmt.Errorf("listening on %s: %w", hostPort, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// WhoIs identifies the peer behind a remote address, which must be IP or
// IP:port. The result is what admission decides on.
func (t *Tailnet) WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	return t.Client.WhoIs(ctx, remoteAddr)
}
