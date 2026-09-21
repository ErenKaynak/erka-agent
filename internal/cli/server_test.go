package cli

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenKaynak/erka-agent/internal/store"
)

func TestCreateServerTokenStoresOnlyHashAndScopes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := t.TempDir()
	id, secret, scopes, err := createServerToken(ctx, state, "remote CLI", []string{"write", "read", "write"})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || !strings.HasPrefix(secret, "erka_") || strings.Join(scopes, ",") != "read,write" {
		t.Fatalf("created token = %q, %q, %#v", id, secret, scopes)
	}
	database, err := store.OpenSQLite(ctx, filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	principal, err := database.AuthenticateAPIToken(ctx, sha256.Sum256([]byte(secret)), "write")
	if err != nil || principal.ID != id || principal.Name != "remote CLI" {
		t.Fatalf("principal = %#v, %v", principal, err)
	}
	data, err := os.ReadFile(filepath.Join(state, "erka.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("plaintext API token was persisted")
	}
}

func TestValidateServerListenRequiresTLSOffLoopback(t *testing.T) {
	t.Parallel()
	valid := serveOptions{listen: "127.0.0.1:7432", poll: time.Second, claim: time.Second, heartbeat: time.Second}
	if err := validateServerListen(valid); err != nil {
		t.Fatal(err)
	}
	remote := valid
	remote.listen = "0.0.0.0:7432"
	if err := validateServerListen(remote); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("plain remote listen error = %v", err)
	}
	remote.tlsCert, remote.tlsKey = "server.crt", "server.key"
	if err := validateServerListen(remote); err != nil {
		t.Fatal(err)
	}
	remote.tlsKey = ""
	if err := validateServerListen(remote); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("partial TLS error = %v", err)
	}
}

func TestNormalizeServerScopesRejectsUnknownAndEmpty(t *testing.T) {
	t.Parallel()
	if _, err := normalizeServerScopes(nil); err == nil {
		t.Fatal("empty scopes accepted")
	}
	if _, err := normalizeServerScopes([]string{"owner"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func TestValidateServerListenAllowsPlainHTTPOnTrustedVPNAddress(t *testing.T) {
	t.Parallel()
	options := serveOptions{listen: "100.101.102.103:7432", trustedCIDRs: []string{"100.64.0.0/10"}, poll: time.Second, claim: time.Second, heartbeat: time.Second}
	if err := validateServerListen(options); err != nil {
		t.Fatalf("VPN listen error = %v", err)
	}
	for _, listen := range []string{"0.0.0.0:7432", "192.168.1.10:7432"} {
		options.listen = listen
		if err := validateServerListen(options); err == nil {
			t.Fatalf("listen %s outside the trusted network was accepted", listen)
		}
	}
}
