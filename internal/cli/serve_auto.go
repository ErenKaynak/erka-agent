package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"rsc.io/qr"

	"github.com/ErenKaynak/erka-agent/internal/app"
	"github.com/ErenKaynak/erka-agent/internal/config"
	erkaserver "github.com/ErenKaynak/erka-agent/internal/server"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

const defaultServePort = 7432

// vpnAddress is an address on a private VPN interface the server can safely
// serve plain HTTP on, because the tunnel encrypts traffic.
type vpnAddress struct {
	IP      net.IP
	Network *net.IPNet
	Label   string
}

var tailscaleRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// detectVPNAddresses finds Tailscale addresses (the 100.64.0.0/10 range) and
// private addresses on WireGuard-style tunnel interfaces. Ordinary LAN
// addresses are never used: plain HTTP there would expose tokens.
var detectVPNAddresses = func() []vpnAddress {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	result := make([]vpnAddress, 0, 1)
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := network.Addrs()
		if err != nil {
			continue
		}
		name := strings.ToLower(network.Name)
		tunnel := strings.HasPrefix(name, "wg") || strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tun")
		for _, address := range addresses {
			ipNet, ok := address.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			ip := ipNet.IP.To4()
			switch {
			case tailscaleRange.Contains(ip):
				result = append(result, vpnAddress{IP: ip, Network: tailscaleRange, Label: "Tailscale"})
			case tunnel && ip.IsPrivate():
				result = append(result, vpnAddress{IP: ip, Network: &net.IPNet{IP: ip.Mask(ipNet.Mask), Mask: ipNet.Mask}, Label: "VPN (" + network.Name + ")"})
			}
		}
	}
	return result
}

// servePlan is what `erka serve` will listen on and advertise.
type servePlan struct {
	listen    []string
	trusted   []string
	publicURL string
	localURL  string
	vpnLabel  string
}

// planServe chooses listeners. An explicit --listen is used as given;
// otherwise the server listens on loopback and on every detected VPN address,
// trusting only those networks.
func planServe(options serveOptions, resolved config.Resolved) servePlan {
	port := options.port
	if port == 0 {
		port = defaultServePort
	}
	plan := servePlan{trusted: append(append([]string(nil), resolved.Server.TrustedCIDRs...), options.trustedCIDRs...)}
	scheme := "http"
	if options.tlsCert != "" {
		scheme = "https"
	}
	if options.listen != "" {
		plan.listen = []string{options.listen}
		plan.localURL = scheme + "://" + options.listen
	} else {
		local := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		plan.listen = []string{local}
		plan.localURL = scheme + "://" + local
		for _, vpn := range detectVPNAddresses() {
			address := net.JoinHostPort(vpn.IP.String(), strconv.Itoa(port))
			plan.listen = append(plan.listen, address)
			plan.trusted = append(plan.trusted, vpn.Network.String())
			if plan.publicURL == "" {
				plan.publicURL, plan.vpnLabel = scheme+"://"+address, vpn.Label
			}
		}
	}
	if resolved.Server.PublicURL != "" && !loopbackURL(resolved.Server.PublicURL) {
		plan.publicURL, plan.vpnLabel = resolved.Server.PublicURL, "configured"
	}
	return plan
}

// loopbackURL reports whether a URL points at this machine only, which a
// phone can never reach.
func loopbackURL(value string) bool {
	host := strings.TrimPrefix(strings.TrimPrefix(value, "http://"), "https://")
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// ensureProject registers the repository erka serve was started in, so the
// first task needs no setup. An existing registration is reused.
func ensureProject(ctx context.Context, service *erkaserver.Service, database *store.SQLite, workingDirectory string) (store.ServerProject, bool, error) {
	root, err := app.DiscoverRepository(workingDirectory)
	if err != nil {
		return store.ServerProject{}, false, nil
	}
	projects, err := database.ListServerProjects(ctx)
	if err != nil {
		return store.ServerProject{}, false, err
	}
	names := make(map[string]bool, len(projects))
	for _, project := range projects {
		if project.RepositoryRoot == root {
			return project, false, nil
		}
		names[project.Name] = true
	}
	name := filepath.Base(root)
	for suffix := 2; names[name]; suffix++ {
		name = fmt.Sprintf("%s-%d", filepath.Base(root), suffix)
	}
	project, err := service.RegisterProject(ctx, name, root, "HEAD")
	return project, err == nil, err
}

// printServeBanner explains how to reach the server and pair a device.
func printServeBanner(output io.Writer, plan servePlan, code string, project store.ServerProject, created bool, notifications int) {
	fmt.Fprintf(output, "\nerka server is running\n\n")
	fmt.Fprintf(output, "  On this computer:  %s/app/\n", plan.localURL)
	if plan.publicURL != "" {
		label := plan.vpnLabel
		if label == "" {
			label = "public URL"
		}
		fmt.Fprintf(output, "  On your phone:     %s/app/   (%s)\n", plan.publicURL, label)
	} else {
		fmt.Fprintf(output, "  On your phone:     not reachable yet — start Tailscale or WireGuard and restart erka serve\n")
	}
	if project.ID != "" {
		verb := "Project:"
		if created {
			verb = "Registered project:"
		}
		fmt.Fprintf(output, "  %s %s (%s)\n", verb, project.Name, project.RepositoryRoot)
	}
	if code != "" {
		base := plan.publicURL
		if base == "" {
			base = plan.localURL
		}
		link := base + "/app/#pair=" + code
		fmt.Fprintf(output, "\n  Pair a device within %d minutes: scan, open the link, or enter %s\n\n", int(erkaserver.PairingLifetime.Minutes()), code)
		if plan.publicURL != "" {
			printQR(output, link)
		}
		fmt.Fprintf(output, "  %s\n", link)
	}
	if notifications == 0 {
		fmt.Fprintf(output, "\n  Notifications are off; add server.notifications to get pushes (see README).\n")
	}
	fmt.Fprintf(output, "\n  Press Ctrl+C to stop.\n\n")
}

// printQR draws a QR code with half-block characters, two modules per line.
// Light modules are drawn, so it scans on the usual dark terminal.
func printQR(output io.Writer, text string) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return
	}
	const quiet = 2
	light := func(x, y int) bool {
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return true
		}
		return !code.Black(x, y)
	}
	var builder strings.Builder
	for y := -quiet; y < code.Size+quiet; y += 2 {
		builder.WriteString("  ")
		for x := -quiet; x < code.Size+quiet; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				builder.WriteString("█")
			case top:
				builder.WriteString("▀")
			case bottom:
				builder.WriteString("▄")
			default:
				builder.WriteString(" ")
			}
		}
		builder.WriteString("\n")
	}
	_, _ = io.WriteString(output, builder.String())
}

func newServerPairCommand(load func(context.Context) (config.Resolved, error)) *cobra.Command {
	var port int
	return withPortFlag(&cobra.Command{Use: "pair", Short: "Print a one-time code and QR code to pair a phone or browser", Args: noArgs, RunE: func(command *cobra.Command, _ []string) error {
		resolved, err := load(command.Context())
		if err != nil {
			return fmt.Errorf("resolve configuration: %w", err)
		}
		database, err := store.OpenSQLite(command.Context(), filepath.Join(resolved.StateDir, "erka.db"))
		if err != nil {
			return err
		}
		defer database.Close()
		code, err := erkaserver.NewPairing(command.Context(), database, time.Now().UTC())
		if err != nil {
			return err
		}
		plan := planServe(serveOptions{port: port}, resolved)
		base := plan.publicURL
		if base == "" {
			base = plan.localURL
		}
		link := base + "/app/#pair=" + code
		output := command.OutOrStdout()
		fmt.Fprintf(output, "Pair a device within %d minutes: scan, open the link, or enter %s\n\n", int(erkaserver.PairingLifetime.Minutes()), code)
		if plan.publicURL != "" {
			printQR(output, link)
		}
		_, err = fmt.Fprintf(output, "  %s\n", link)
		return err
	}}, &port)
}

func withPortFlag(command *cobra.Command, port *int) *cobra.Command {
	command.Flags().IntVar(port, "port", defaultServePort, "server port")
	return command
}
