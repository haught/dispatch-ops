package auth

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}
	ok, err = VerifyPassword("wrong password", hash)
	if err != nil || ok {
		t.Fatalf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestHashPasswordUsesUniqueSalt(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$a2V5",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=x,t=2,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$a2V5",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
	} {
		if _, err := VerifyPassword("pw", h); err == nil {
			t.Errorf("VerifyPassword accepted malformed hash %q", h)
		}
	}
}

func TestNewToken(t *testing.T) {
	secret, hash, display, err := NewToken(APITokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, APITokenPrefix) {
		t.Errorf("secret %q lacks prefix %q", secret, APITokenPrefix)
	}
	if !LooksLikeToken(secret) {
		t.Errorf("LooksLikeToken(%q) = false", secret)
	}
	if string(hash) != string(HashToken(secret)) {
		t.Error("returned hash does not match HashToken(secret)")
	}
	if !strings.HasPrefix(secret, display) || len(display) != displayPrefixLen {
		t.Errorf("display prefix %q is not the first %d chars of the secret", display, displayPrefixLen)
	}

	other, _, _, _ := NewToken(APITokenPrefix)
	if other == secret {
		t.Error("two generated tokens are identical")
	}
}

func TestLooksLikeToken(t *testing.T) {
	session, _, _, _ := NewToken(SessionTokenPrefix)
	for s, want := range map[string]bool{
		session:              true,
		"":                   false,
		"dop_short":          false,
		"xyz_" + session[4:]: false,
	} {
		if got := LooksLikeToken(s); got != want {
			t.Errorf("LooksLikeToken(%q) = %v, want %v", s, got, want)
		}
	}
}
