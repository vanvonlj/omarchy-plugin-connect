package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/vanvonlj/omarchy-plugin-connect/internal/config"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/device"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/pairing"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/qr"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/transport"
)

const devicesUsage = `Usage:
  omarchy-connect devices [--json]              list paired and tailnet devices
  omarchy-connect devices allow <id>            let a device type (write)
  omarchy-connect devices readonly <id>         take away write
  omarchy-connect devices rename <id> <name>    change the display name
  omarchy-connect devices revoke <id>           stop a device using the daemon
  omarchy-connect devices unblock <id>          let a blocked device back in
`

func openStores() (*device.Store, *pairing.Store, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, nil, err
	}
	devices, err := device.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	pairs, err := pairing.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	return devices, pairs, nil
}

func devicesCmd(args []string) error {
	devices, _, err := openStores()
	if err != nil {
		return err
	}

	if len(args) == 0 || args[0] == "--json" || args[0] == "list" {
		return listDevices(devices, len(args) > 0 && args[0] == "--json")
	}

	verb := args[0]
	rest := args[1:]

	switch verb {
	case "allow":
		if len(rest) != 1 {
			return errors.New("usage: omarchy-connect devices allow <id>")
		}
		if err := devices.SetCapability(rest[0], device.Write); err != nil {
			return err
		}
		fmt.Printf("%s can now type.\n", rest[0])
		return nil

	case "readonly", "deny":
		if len(rest) != 1 {
			return errors.New("usage: omarchy-connect devices readonly <id>")
		}
		if err := devices.SetCapability(rest[0], device.Read); err != nil {
			return err
		}
		fmt.Printf("%s is now read-only.\n", rest[0])
		return nil

	case "rename":
		if len(rest) != 2 {
			return errors.New("usage: omarchy-connect devices rename <id> <name>")
		}
		if err := devices.Rename(rest[0], rest[1]); err != nil {
			return err
		}
		fmt.Printf("%s renamed to %q.\n", rest[0], rest[1])
		return nil

	case "revoke":
		if len(rest) != 1 {
			return errors.New("usage: omarchy-connect devices revoke <id>")
		}
		d, err := devices.Get(rest[0])
		if err != nil {
			return err
		}
		if err := devices.Revoke(rest[0]); err != nil {
			return err
		}
		// Live attaches notice within capabilityCheckInterval and close
		// themselves, so this is not only a change for next time.
		if d.Kind == device.Tailnet {
			fmt.Printf("%s blocked. Tailscale still vouches for it, so the record stays\n", rest[0])
			fmt.Printf("to keep refusing it; `devices unblock %s` lets it back in read-only.\n", rest[0])
		} else {
			fmt.Printf("%s revoked. Its token no longer works.\n", rest[0])
		}
		fmt.Println("Any terminal it has open will close shortly.")
		return nil

	case "unblock":
		if len(rest) != 1 {
			return errors.New("usage: omarchy-connect devices unblock <id>")
		}
		if err := devices.Unblock(rest[0]); err != nil {
			return err
		}
		fmt.Printf("%s unblocked, read-only.\n", rest[0])
		return nil

	default:
		fmt.Fprint(os.Stderr, devicesUsage)
		return fmt.Errorf("unknown devices command %q", verb)
	}
}

func listDevices(store *device.Store, asJSON bool) error {
	devices, err := store.List()
	if err != nil {
		return err
	}

	if asJSON {
		if devices == nil {
			devices = []device.Device{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(devices)
	}

	if len(devices) == 0 {
		fmt.Println("No devices yet. Run `omarchy-connect pair` to add one,")
		fmt.Println("or open the URL from another machine on your tailnet.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tKIND\tCAPABILITY\tLAST SEEN")
	for _, d := range devices {
		capability := string(d.EffectiveCapability())
		if d.Blocked {
			capability = "blocked"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			d.ID, d.Name, d.Kind, capability, ago(d.LastSeen))
	}
	return tw.Flush()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

type pairOutput struct {
	URL     string   `json:"url"`
	Code    string   `json:"code"`
	Expires string   `json:"expires"`
	Matrix  []string `json:"matrix"`
}

// pairCmd starts a pairing and shows the QR.
//
// --json is what the plugin panel calls: it gets the URL, the expiry, and the
// same 0/1 matrix omarchy-network-qr emits, so the panel draws QML rectangles
// rather than shelling out for an image or decoding one.
func pairCmd(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON for the plugin")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	_, pairs, err := openStores()
	if err != nil {
		return err
	}

	tn, err := transport.Probe(context.Background())
	if err != nil {
		return err
	}

	p, err := pairs.Create()
	if err != nil {
		return err
	}

	url := p.URL(tn.DNSName, cfg.Port)
	matrix, err := qr.Matrix(url)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(pairOutput{
			URL:     url,
			Code:    p.Code,
			Expires: p.Expires.Format(time.RFC3339),
			Matrix:  matrix,
		})
	}

	fmt.Println()
	fmt.Print(qr.Terminal(matrix))
	fmt.Println()
	fmt.Printf("  Scan within %s.\n", pairing.TTL)
	fmt.Printf("  %s\n\n", url)
	fmt.Println("  The device pairs read-only. Give it write access with:")
	fmt.Println("      omarchy-connect devices allow <id>")
	return nil
}
