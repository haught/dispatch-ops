package credentials

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/store"
)

func TestSplit(t *testing.T) {
	ssh, _ := Lookup("ssh")
	public, secret, err := ssh.Split(map[string]string{"username": "deploy", "private_key": "KEY", "passphrase": ""})
	if err != nil {
		t.Fatal(err)
	}
	if public["username"] != "deploy" || len(public) != 1 {
		t.Errorf("public = %v", public)
	}
	if secret["private_key"] != "KEY" || len(secret) != 1 {
		t.Errorf("secret = %v (empty values should be dropped)", secret)
	}

	_, _, err = ssh.Split(map[string]string{"username": "deploy", "bogus": "x"})
	if err == nil || !strings.Contains(err.Error(), `unknown field "bogus"`) || !strings.Contains(err.Error(), `"private_key" is required`) {
		t.Errorf("Split with bad inputs: %v", err)
	}

	generic, _ := Lookup("generic")
	_, secret, err = generic.Split(map[string]string{"API_KEY": "abc"})
	if err != nil || secret["API_KEY"] != "abc" {
		t.Errorf("generic Split = %v, %v", secret, err)
	}
	for _, bad := range []map[string]string{{}, {"bad name": "x"}, {"1x": "y"}} {
		if _, _, err := generic.Split(bad); err == nil {
			t.Errorf("generic Split(%v) succeeded", bad)
		}
	}
}

func TestVaultSealAndInputs(t *testing.T) {
	keys, err := secrets.NewKeyring(secrets.GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	v := Vault{Keys: keys}
	c := store.Credential{ID: uuid.New(), Name: "reg", Kind: "registry"}
	if err := v.Seal(&c, map[string]string{"server": "ghcr.io", "username": "bot", "password": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(c.SecretInputs), "s3cret") {
		t.Fatal("secret stored in clear")
	}
	if len(c.SecretFields) != 1 || c.SecretFields[0] != "password" {
		t.Errorf("SecretFields = %v", c.SecretFields)
	}
	in, err := v.Inputs(c)
	if err != nil {
		t.Fatal(err)
	}
	if in["password"] != "s3cret" || in["server"] != "ghcr.io" || in["username"] != "bot" {
		t.Errorf("Inputs = %v", in)
	}

	// Ciphertext copied onto another credential does not decrypt.
	other := c
	other.ID = uuid.New()
	if _, err := v.Inputs(other); err == nil {
		t.Error("ciphertext decrypted under another credential ID")
	}
}
