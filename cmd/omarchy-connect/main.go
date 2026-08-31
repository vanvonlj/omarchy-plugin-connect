// Command omarchy-connect is the daemon and CLI for omarchy-plugin-connect.
//
// The QML plugin calls this binary rather than reimplementing any of it, so the
// panel and the terminal can never disagree about what the daemon thinks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/vanvonlj/omarchy-plugin-connect/internal/config"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/server"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/transport"
)

// version is overridden at build time via -ldflags.
var version = "dev"

const usage = `omarchy-connect - remote access to this machine's tmux and agent sessions

Usage:
  omarchy-connect serve            run the daemon (the systemd user unit does this)
  omarchy-connect status [--json]  report listeners, TLS, and preconditions
  omarchy-connect pair [--json]    show a QR code to pair a new device
  omarchy-connect devices ...      list, rename, promote, or revoke devices
  omarchy-connect lan on|off       serve on the LAN too, for phones without Tailscale
  omarchy-connect version

Settings live in ~/.config/omarchy/connect/config.json and are edited from the
Omarchy plugin panel.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "pair":
		err = pairCmd(os.Args[2:])
	case "devices":
		err = devicesCmd(os.Args[2:])
	case "lan":
		err = lanCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "omarchy-connect: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "omarchy-connect: %v\n", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	verbose := fs.Bool("verbose", false, "log at debug level")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Signals are hooked before the first slow call so an early Ctrl-C during
	// the certificate warm-up still exits promptly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tn, err := transport.Probe(ctx)
	if err != nil {
		return err
	}

	if err := tn.WarmCert(ctx); err != nil {
		// Never fatal: the handshake will retry, and a daemon that refuses to
		// start over a slow ACME round trip is worse than a slow first request.
		log.Warn("certificate warm-up failed; the first request will retry", "err", err)
	}

	devices, pairs, err := openStores()
	if err != nil {
		return err
	}

	srv := server.New(tn, cfg, log, version, devices, pairs)

	if cfg.LAN.Enabled {
		lan, err := transport.ProbeLAN(cfg.LAN.Address)
		if err != nil {
			// Not fatal. The tailnet listener is the one that matters, and
			// taking it down because an optional second listener could not find
			// an address would be a poor trade.
			log.Error("LAN tier is enabled but no address was found; serving tailnet only", "err", err)
		} else {
			srv = srv.WithLAN(lan)
		}
	}

	return srv.Run(ctx)
}

type statusReport struct {
	Version        string   `json:"version"`
	Serving        bool     `json:"serving"`
	Node           string   `json:"node"`
	URL            string   `json:"url"`
	Addrs          []string `json:"addrs"`
	CertsAvailable bool     `json:"certsAvailable"`
	Port           int      `json:"port"`
	LANEnabled     bool     `json:"lanEnabled"`
	LANURL         string   `json:"lanUrl,omitempty"`
	LANServing     bool     `json:"lanServing"`
	LANProblem     string   `json:"lanProblem,omitempty"`
	Firewall       string   `json:"firewall,omitempty"`
	ConfigPath     string   `json:"configPath"`
	Problem        string   `json:"problem,omitempty"`
}

// status reports what the daemon can do without needing it to be running. The
// plugin panel renders this, so it must stay machine-readable under --json and
// must still say something useful when a precondition has failed.
func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON for the plugin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfgPath, _ := config.Path()
	report := statusReport{Version: version, ConfigPath: cfgPath}

	cfg, err := config.Load()
	if err != nil {
		report.Problem = err.Error()
	} else {
		report.Port = cfg.Port
		report.LANEnabled = cfg.LAN.Enabled
	}

	tn, probeErr := transport.Probe(context.Background())
	if probeErr != nil {
		if report.Problem == "" {
			report.Problem = probeErr.Error()
		}
	} else {
		report.Node = tn.DNSName
		report.CertsAvailable = tn.CertsAvailable
		report.URL = tn.URL(report.Port)
		for _, a := range tn.Addrs {
			report.Addrs = append(report.Addrs, a.String())
		}
		report.Serving = serving(tn, report.Port)
	}

	if cfg.LAN.Enabled {
		lan, lanErr := transport.ProbeLAN(cfg.LAN.Address)
		if lanErr != nil {
			report.LANProblem = lanErr.Error()
		} else {
			report.LANURL = lan.URL(cfg.LAN.Port)
			report.LANServing = dialable(lan.Addr.String(), cfg.LAN.Port)
			report.Firewall = lan.FirewallHint(cfg.LAN.Port)
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	printStatus(report, probeErr)

	// A failed probe is an error exit even though the report printed: the unit
	// and the plugin both branch on the exit code.
	if probeErr != nil {
		return errors.New("tailnet is not usable; see above")
	}
	return nil
}

// serving reports whether anything is actually accepting connections on the
// listener port.
//
// The obvious check -- ask systemd whether the unit is active -- answers a
// different question, and answers it wrongly for a daemon started by hand from
// a terminal. A panel that says "Stopped" while a phone is happily attached is
// worse than no status at all.
func serving(tn *transport.Tailnet, port int) bool {
	if port == 0 || len(tn.Addrs) == 0 {
		return false
	}
	return dialable(tn.Addrs[0].String(), port)
}

func dialable(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func printStatus(r statusReport, probeErr error) {
	fmt.Printf("omarchy-connect %s\n\n", r.Version)

	if probeErr != nil {
		fmt.Printf("  tailnet    UNAVAILABLE\n\n%v\n", probeErr)
		return
	}

	fmt.Printf("  serving    %s\n", servingWord(r.Serving))
	fmt.Printf("  node       %s\n", r.Node)
	fmt.Printf("  url        %s\n", r.URL)
	fmt.Printf("  addrs      %v\n", r.Addrs)

	if r.CertsAvailable {
		fmt.Printf("  tls        yes (Let's Encrypt, via the Tailscale LocalAPI)\n")
	} else {
		fmt.Printf("  tls        NO - serving plain HTTP on the tailnet\n")
		fmt.Printf("             traffic is still WireGuard-encrypted, but an http://\n")
		fmt.Printf("             origin is not a secure context: no PWA install, no\n")
		fmt.Printf("             service worker, no push.\n")
		fmt.Printf("             fix: admin console > DNS > HTTPS Certificates\n")
	}

	if r.LANEnabled {
		if r.LANProblem != "" {
			fmt.Printf("  lan tier   enabled, but: %s\n", r.LANProblem)
		} else {
			fmt.Printf("  lan tier   %s  (%s)\n", r.LANURL, servingWord(r.LANServing))
			fmt.Printf("             http only - no app install, no notifications\n")
		}
	} else {
		fmt.Printf("  lan tier   disabled - phones without Tailscale cannot connect\n")
		fmt.Printf("             enable with: omarchy-connect lan on\n")
	}
	fmt.Printf("  config     %s\n", r.ConfigPath)

	if r.Firewall != "" {
		fmt.Printf("\n  %s\n", r.Firewall)
	}

	if r.Problem != "" {
		fmt.Printf("\n  problem    %s\n", r.Problem)
	}
}

func servingWord(b bool) string {
	if b {
		return "yes"
	}
	return "no - nothing is listening"
}
