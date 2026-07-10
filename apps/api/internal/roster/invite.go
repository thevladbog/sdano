// Package roster manages the tenant's workers and their invite codes —
// people-management, distinct from auth's token machinery.
package roster

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"sdano.app/api/internal/db"
)

const inviteTTL = 72 * time.Hour

// GenerateInviteCode returns a 6-digit crypto-random code (leading zeros kept).
func GenerateInviteCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generating invite code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// CreateInvite voids the worker's previous unused invites and issues a fresh
// 6-digit code, retrying on the unique-while-unclaimed index collision.
func CreateInvite(ctx context.Context, q *db.Queries, tenantID, userID uuid.UUID) (string, time.Time, error) {
	if err := q.VoidWorkerInvites(ctx, db.VoidWorkerInvitesParams{TenantID: tenantID, UserID: userID}); err != nil {
		return "", time.Time{}, fmt.Errorf("voiding old invites: %w", err)
	}
	expires := time.Now().Add(inviteTTL)
	for attempt := 0; attempt < 5; attempt++ {
		code, err := GenerateInviteCode()
		if err != nil {
			return "", time.Time{}, err
		}
		err = q.InsertInvite(ctx, db.InsertInviteParams{
			TenantID: tenantID, UserID: userID, Code: code,
			ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
		})
		if err == nil {
			return code, expires, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: live code collision
			continue
		}
		return "", time.Time{}, fmt.Errorf("inserting invite: %w", err)
	}
	return "", time.Time{}, errors.New("could not generate a unique invite code after 5 attempts")
}
