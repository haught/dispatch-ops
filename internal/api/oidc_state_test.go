package api

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/oidc"
	"dispatch-ops/internal/secrets"
)

func TestOIDCStateSealing(t *testing.T) {
	keys, err := secrets.NewKeyring(secrets.GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{Vault: credentials.Vault{Keys: keys}}
	login := oidcLogin{Login: oidc.Login{State: "st"}, Next: "/jobs", Expires: time.Now().Add(time.Minute).Unix()}

	sealed, err := h.sealOIDCState(login)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := h.openOIDCState(sealed)
	if !ok || got.State != "st" || got.Next != "/jobs" {
		t.Fatalf("open(seal(x)) = %+v, %v", got, ok)
	}

	// Tampered, garbled, expired, or sealed with another key: refused.
	tampered := []byte(sealed)
	tampered[len(tampered)-2] ^= 1
	login.Expires = time.Now().Add(-time.Second).Unix()
	expired, _ := h.sealOIDCState(login)
	otherKeys, _ := secrets.NewKeyring(secrets.GenerateKey())
	other, _ := (&Handlers{Vault: credentials.Vault{Keys: otherKeys}}).sealOIDCState(oidcLogin{
		Login: oidc.Login{State: "st"}, Expires: time.Now().Add(time.Minute).Unix(),
	})
	for name, v := range map[string]string{
		"tampered": string(tampered), "garbled": "x.y", "empty": "", "expired": expired, "other key": other,
	} {
		if _, ok := h.openOIDCState(v); ok {
			t.Errorf("%s state cookie accepted", name)
		}
	}
}

// The state is sealed with a key derived from the credential key, not the
// key itself, so the sign-in endpoint isn't an encryption oracle for
// credentials; state sealed before a key rotation still opens.
func TestOIDCStateKeyIsDerived(t *testing.T) {
	oldKey, newKey := secrets.GenerateKey(), secrets.GenerateKey()
	oldKeys, _ := secrets.NewKeyring(oldKey)
	h := &Handlers{Vault: credentials.Vault{Keys: oldKeys}}
	login := oidcLogin{Login: oidc.Login{State: "st"}, Expires: time.Now().Add(time.Minute).Unix()}
	sealed, err := h.sealOIDCState(login)
	if err != nil {
		t.Fatal(err)
	}
	keyID, enc, _ := strings.Cut(sealed, ".")
	ct, _ := base64.RawURLEncoding.DecodeString(enc)
	if keyID == oldKeys.CurrentKeyID() {
		t.Error("state names the credential key")
	}
	if _, err := oldKeys.Decrypt(ct, oldKeys.CurrentKeyID(), oidcStateAAD); err == nil {
		t.Error("state sealed with the credential key itself")
	}
	derived, _ := oldKeys.Derive("dispatch-ops oidc-state v1")
	if _, err := derived.Decrypt(ct, keyID, oidcStateAAD); err != nil {
		t.Errorf("state not sealed with the HKDF-derived key: %v", err)
	}

	// After rotation, state sealed with the old key still opens.
	rotated, _ := secrets.NewKeyring(newKey, oldKey)
	h2 := &Handlers{Vault: credentials.Vault{Keys: rotated}}
	if got, ok := h2.openOIDCState(sealed); !ok || got.State != "st" {
		t.Errorf("after rotation: %+v, %v", got, ok)
	}
	resealed, _ := h2.sealOIDCState(login)
	if got, ok := h2.openOIDCState(resealed); !ok || got.State != "st" {
		t.Errorf("after rotation, new state: %+v, %v", got, ok)
	}
}
