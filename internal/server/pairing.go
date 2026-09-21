package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
	"github.com/ErenKaynak/erka-agent/internal/store"
)

const (
	// PairingLifetime is how long a printed pairing code can be used.
	PairingLifetime = 10 * time.Minute
	pairingAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
	pairingLength   = 10
	// pairingFailureLimit bounds guesses per window; with a 30^10 code space
	// guessing is hopeless, and the limit keeps it that way.
	pairingFailureLimit  = 20
	pairingFailureWindow = 10 * time.Minute
)

// PairedDeviceScopes are the scopes a paired phone or browser receives.
var PairedDeviceScopes = []string{"approve", "read", "write"}

type PairingStore interface {
	CreatePairing(context.Context, [32]byte, time.Time, time.Time) error
	RedeemPairing(context.Context, [32]byte, store.APIToken, time.Time) error
}

// NewPairing stores a one-time pairing code and returns it for display as
// XXXXX-XXXXX. Only its hash is persisted.
func NewPairing(ctx context.Context, pairings PairingStore, now time.Time) (string, error) {
	var builder strings.Builder
	limit := big.NewInt(int64(len(pairingAlphabet)))
	for index := 0; index < pairingLength; index++ {
		value, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate pairing code: %w", err)
		}
		builder.WriteByte(pairingAlphabet[value.Int64()])
	}
	code := builder.String()
	if err := pairings.CreatePairing(ctx, pairingHash(code), now, now.Add(PairingLifetime)); err != nil {
		return "", err
	}
	return code[:5] + "-" + code[5:], nil
}

func pairingHash(code string) [32]byte {
	normalized := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
	return sha256.Sum256([]byte("erka-pairing\x00" + normalized))
}

// NewTokenSecret returns a fresh bearer token secret and its stored hash.
func NewTokenSecret() (string, [32]byte, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", [32]byte{}, fmt.Errorf("generate token: %w", err)
	}
	secret := "erka_" + base64.RawURLEncoding.EncodeToString(random)
	return secret, sha256.Sum256([]byte(secret)), nil
}

type pairingLimiter struct {
	mu       sync.Mutex
	failures []time.Time
}

func (limiter *pairingLimiter) allow(now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	kept := limiter.failures[:0]
	for _, failure := range limiter.failures {
		if now.Sub(failure) < pairingFailureWindow {
			kept = append(kept, failure)
		}
	}
	limiter.failures = kept
	return len(kept) < pairingFailureLimit
}

func (limiter *pairingLimiter) fail(now time.Time) {
	limiter.mu.Lock()
	limiter.failures = append(limiter.failures, now)
	limiter.mu.Unlock()
}

// pair exchanges a one-time pairing code for a device token. It is the only
// unauthenticated write endpoint and is rate limited.
func (api *API) pair(response http.ResponseWriter, request *http.Request) {
	if api.pairings == nil {
		writeAPIError(response, http.StatusNotImplemented, "pairing_unavailable", "Pairing is not enabled on this server")
		return
	}
	now := api.clock().UTC()
	if !api.pairingLimit.allow(now) {
		writeAPIError(response, http.StatusTooManyRequests, "pairing_rate_limited", "Too many pairing attempts; run erka server pair for a new code")
		return
	}
	var input struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	secret, hash, err := NewTokenSecret()
	if err != nil {
		writeMappedError(response, err)
		return
	}
	id, err := (erkaruntime.RandomIDGenerator{}).NewID("token")
	if err != nil {
		writeMappedError(response, err)
		return
	}
	name := strings.TrimSpace(input.Device)
	if name == "" || len(name) > 64 {
		name = "paired device"
	}
	token := store.APIToken{ID: id, Name: name, Hash: hash, Scopes: PairedDeviceScopes, CreatedAt: now}
	if err := api.pairings.RedeemPairing(request.Context(), pairingHash(input.Code), token, now); err != nil {
		if errors.Is(err, erkaruntime.ErrNotFound) {
			api.pairingLimit.fail(now)
			writeAPIError(response, http.StatusUnauthorized, "invalid_pairing_code", "The pairing code is wrong, expired, or already used")
			return
		}
		writeMappedError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"token": secret, "tokenId": id, "scopes": PairedDeviceScopes})
}

var _ PairingStore = (*store.SQLite)(nil)
