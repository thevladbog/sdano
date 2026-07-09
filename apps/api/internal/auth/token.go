package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 30 * 24 * time.Hour
)

type accessClaims struct {
	jwt.RegisteredClaims
	Tenant string `json:"tenant"`
	Role   string `json:"role"`
}

// IssueAccessToken mints a short-lived HS256 JWT carrying the principal.
func IssueAccessToken(secret string, p Principal, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Tenant: p.TenantID.String(),
		Role:   string(p.Role),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("signing access token: %w", err)
	}
	return signed, nil
}

// ParseAccessToken verifies signature + expiry and reconstructs the principal.
func ParseAccessToken(secret, raw string) (Principal, error) {
	var claims accessClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return Principal{}, fmt.Errorf("parsing access token: %w", err)
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("bad subject: %w", err)
	}
	tid, err := uuid.Parse(claims.Tenant)
	if err != nil {
		return Principal{}, fmt.Errorf("bad tenant: %w", err)
	}
	return Principal{UserID: uid, TenantID: tid, Role: Role(claims.Role)}, nil
}

// GenerateOpaqueToken returns a random 256-bit token (base64url plaintext for
// the client) and its hex SHA-256 hash (stored at rest).
func GenerateOpaqueToken() (plaintext, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashOpaqueToken(plaintext), nil
}

// HashOpaqueToken returns the hex SHA-256 of an opaque token plaintext.
func HashOpaqueToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
