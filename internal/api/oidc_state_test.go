package api

import (
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
