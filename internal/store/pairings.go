package store

import (
	"context"
	"errors"
	"time"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

// CreatePairing stores a one-time device pairing code by its hash.
func (store *SQLite) CreatePairing(ctx context.Context, hash [32]byte, now, expires time.Time) error {
	if !expires.After(now) {
		return errors.New("pairing must expire in the future")
	}
	_, err := store.db.ExecContext(ctx, `INSERT INTO device_pairings(code_hash, created_at, expires_at) VALUES(?, ?, ?)`, hash[:], formatTime(now), formatTime(expires))
	return mapInsertError(err, "pairing", "code")
}

// RedeemPairing exchanges an unused, unexpired pairing code for a new API
// token in one transaction, so a code can never mint two tokens.
func (store *SQLite) RedeemPairing(ctx context.Context, hash [32]byte, token APIToken, now time.Time) error {
	if err := validateAPIToken(token); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE device_pairings SET redeemed_at = ?, token_id = ? WHERE code_hash = ? AND redeemed_at IS NULL AND expires_at > ?`, formatTime(now), token.ID, hash[:], formatTime(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return erkaruntime.ErrNotFound
	}
	if err := insertAPIToken(ctx, tx, token); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_pairings WHERE expires_at <= ? AND redeemed_at IS NULL`, formatTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}
