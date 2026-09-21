package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPairingCodeMintsOneScopedDeviceToken(t *testing.T) {
	t.Parallel()
	fixture := newAPIFixture(t)
	code, err := NewPairing(context.Background(), fixture.database, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 11 || code[5] != '-' {
		t.Fatalf("pairing code = %q", code)
	}
	response := fixture.request(t, http.MethodPost, "/v1/pair", "", map[string]string{"code": strings.ToLower(strings.ReplaceAll(code, "-", " ")), "device": "iPhone"})
	if response.Code != http.StatusCreated {
		t.Fatalf("pair status = %d: %s", response.Code, response.Body.String())
	}
	var paired struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &paired); err != nil || !strings.HasPrefix(paired.Token, "erka_") {
		t.Fatalf("pair response = %s", response.Body.String())
	}
	me := fixture.request(t, http.MethodGet, "/v1/me", paired.Token, nil)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"approve"`) || strings.Contains(me.Body.String(), `"admin"`) {
		t.Fatalf("paired identity = %d %s", me.Code, me.Body.String())
	}
	if again := fixture.request(t, http.MethodPost, "/v1/pair", "", map[string]string{"code": code}); again.Code != http.StatusUnauthorized {
		t.Fatalf("reused pairing code status = %d", again.Code)
	}
}

func TestPairingRejectsExpiredCodesAndLimitsGuessing(t *testing.T) {
	t.Parallel()
	fixture := newAPIFixture(t)
	expired, err := NewPairing(context.Background(), fixture.database, time.Now().UTC().Add(-2*PairingLifetime))
	if err != nil {
		t.Fatal(err)
	}
	if response := fixture.request(t, http.MethodPost, "/v1/pair", "", map[string]string{"code": expired}); response.Code != http.StatusUnauthorized {
		t.Fatalf("expired code status = %d", response.Code)
	}
	valid, err := NewPairing(context.Background(), fixture.database, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < pairingFailureLimit; attempt++ {
		fixture.request(t, http.MethodPost, "/v1/pair", "", map[string]string{"code": "WRONG-GUESS"})
	}
	if response := fixture.request(t, http.MethodPost, "/v1/pair", "", map[string]string{"code": valid}); response.Code != http.StatusTooManyRequests {
		t.Fatalf("pairing after %d failures status = %d, want rate limited", pairingFailureLimit, response.Code)
	}
}
