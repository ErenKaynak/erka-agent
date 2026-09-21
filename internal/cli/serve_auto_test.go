package cli

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/ErenKaynak/erka-agent/internal/config"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestPlanServeListensOnLoopbackAndDetectedVPN(t *testing.T) {
	detect := detectVPNAddresses
	t.Cleanup(func() { detectVPNAddresses = detect })
	detectVPNAddresses = func() []vpnAddress {
		return []vpnAddress{{IP: net.IPv4(100, 101, 102, 103).To4(), Network: tailscaleRange, Label: "Tailscale"}}
	}
	plan := planServe(serveOptions{port: 7432}, config.Resolved{})
	if strings.Join(plan.listen, ",") != "127.0.0.1:7432,100.101.102.103:7432" || strings.Join(plan.trusted, ",") != "100.64.0.0/10" {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.publicURL != "http://100.101.102.103:7432" || plan.vpnLabel != "Tailscale" {
		t.Fatalf("public URL = %q (%s)", plan.publicURL, plan.vpnLabel)
	}
	for _, address := range plan.listen {
		if err := validateServerListen(serveOptions{listen: address, trustedCIDRs: plan.trusted, poll: 1, claim: 1, heartbeat: 1}); err != nil {
			t.Fatalf("planned address %s fails validation: %v", address, err)
		}
	}
	loopbackConfig := planServe(serveOptions{port: 7432}, config.Resolved{Server: config.Server{PublicURL: "http://127.0.0.1:7432"}})
	if loopbackConfig.publicURL != "http://100.101.102.103:7432" {
		t.Fatalf("a loopback public_url replaced the VPN address: %q", loopbackConfig.publicURL)
	}
}

func TestPlanServeWithoutVPNStaysOnLoopback(t *testing.T) {
	detect := detectVPNAddresses
	t.Cleanup(func() { detectVPNAddresses = detect })
	detectVPNAddresses = func() []vpnAddress { return nil }
	plan := planServe(serveOptions{port: 7432}, config.Resolved{})
	if strings.Join(plan.listen, ",") != "127.0.0.1:7432" || plan.publicURL != "" || len(plan.trusted) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
	var output bytes.Buffer
	printServeBanner(&output, plan, "ABCDE-FGHJK", store.ServerProject{}, false, 0)
	if text := output.String(); !strings.Contains(text, "start Tailscale or WireGuard") || !strings.Contains(text, "http://127.0.0.1:7432/app/#pair=ABCDE-FGHJK") {
		t.Fatalf("banner:\n%s", text)
	}
}

func TestPrintQRDrawsASquareCode(t *testing.T) {
	var output bytes.Buffer
	printQR(&output, "http://100.101.102.103:7432/app/#pair=ABCDE-FGHJK")
	rows := strings.Split(strings.TrimRight(output.String(), "\n"), "\n")
	if len(rows) < 10 {
		t.Fatalf("QR has %d rows:\n%s", len(rows), output.String())
	}
	width := len([]rune(rows[0]))
	for _, row := range rows {
		if len([]rune(row)) != width {
			t.Fatalf("QR rows are ragged:\n%s", output.String())
		}
	}
}
