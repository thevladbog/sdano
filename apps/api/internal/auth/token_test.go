package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAccessTokenRoundTrip(t *testing.T) {
	p := Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: RoleAdmin}
	tok, err := IssueAccessToken("secret", p, AccessTTL)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	got, err := ParseAccessToken("secret", tok)
	if err != nil {
		t.Fatalf("ParseAccessToken: %v", err)
	}
	if got != p {
		t.Errorf("round-trip principal = %+v, want %+v", got, p)
	}
}

func TestParseAccessTokenRejectsWrongSecret(t *testing.T) {
	tok, _ := IssueAccessToken("secret", Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: RoleWorker}, AccessTTL)
	if _, err := ParseAccessToken("other-secret", tok); err == nil {
		t.Error("token signed with a different secret must not parse")
	}
}

func TestParseAccessTokenRejectsExpired(t *testing.T) {
	tok, _ := IssueAccessToken("secret", Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: RoleManager}, -1*time.Minute)
	if _, err := ParseAccessToken("secret", tok); err == nil {
		t.Error("expired token must not parse")
	}
}

func TestOpaqueTokenHashing(t *testing.T) {
	plain, hash, err := GenerateOpaqueToken()
	if err != nil {
		t.Fatalf("GenerateOpaqueToken: %v", err)
	}
	if plain == "" || hash == "" || plain == hash {
		t.Fatalf("bad token/hash: plain=%q hash=%q", plain, hash)
	}
	if HashOpaqueToken(plain) != hash {
		t.Error("HashOpaqueToken(plain) must equal the returned hash")
	}
	plain2, _, _ := GenerateOpaqueToken()
	if plain2 == plain {
		t.Error("two generated tokens must differ")
	}
}
