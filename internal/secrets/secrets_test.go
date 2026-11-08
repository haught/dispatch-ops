package secrets

import (
	"bytes"
	"strings"
	"testing"
)

func TestKeyringRoundTripAndRotation(t *testing.T) {
	oldKey, newKey := GenerateKey(), GenerateKey()

	k1, err := NewKeyring(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	ct, id1, err := k1.Encrypt([]byte("hunter2"), []byte("row-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("ciphertext contains the plaintext")
	}

	pt, err := k1.Decrypt(ct, id1, []byte("row-1"))
	if err != nil || string(pt) != "hunter2" {
		t.Fatalf("Decrypt = %q, %v", pt, err)
	}

	// The associated data must match: a value can't be moved to another row.
	if _, err := k1.Decrypt(ct, id1, []byte("row-2")); err == nil {
		t.Error("decrypting with different associated data succeeded")
	}

	// After rotation, old values still decrypt and new ones use the new key.
	k2, err := NewKeyring(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := k2.Decrypt(ct, id1, []byte("row-1")); err != nil || string(pt) != "hunter2" {
		t.Fatalf("Decrypt after rotation = %q, %v", pt, err)
	}
	if _, id2, _ := k2.Encrypt([]byte("x"), nil); id2 == id1 {
		t.Error("new values are still encrypted with the old key")
	}

	// Without the old key, the error names the missing key.
	k3, _ := NewKeyring(newKey)
	if _, err := k3.Decrypt(ct, id1, []byte("row-1")); err == nil || !strings.Contains(err.Error(), id1) {
		t.Errorf("Decrypt with unknown key: %v", err)
	}
}

func TestNewKeyringRejectsBadKeys(t *testing.T) {
	for _, key := range []string{"", "not base64!", "c2hvcnQ="} {
		if _, err := NewKeyring(key); err == nil {
			t.Errorf("NewKeyring(%q) succeeded", key)
		}
	}
}
