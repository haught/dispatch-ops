package notify

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/store"
)

// Vault encrypts and decrypts notification targets' secret values.
type Vault struct {
	Keys *secrets.Keyring
}

func aad(n *store.NotificationTarget) []byte {
	return []byte("dispatch-ops/notification-target/" + n.ID.String())
}

// Seal encrypts values into n, which must have its ID set.
func (v Vault) Seal(n *store.NotificationTarget, values map[string]string) error {
	if values == nil {
		values = map[string]string{}
	}
	plaintext, err := json.Marshal(values)
	if err != nil {
		return err
	}
	ct, keyID, err := v.Keys.Encrypt(plaintext, aad(n))
	if err != nil {
		return err
	}
	n.SecretValues, n.KeyID = ct, keyID
	n.SecretFields = slices.Sorted(maps.Keys(values))
	return nil
}

// Open decrypts n's secret values.
func (v Vault) Open(n store.NotificationTarget) (map[string]string, error) {
	plaintext, err := v.Keys.Decrypt(n.SecretValues, n.KeyID, aad(&n))
	if err != nil {
		return nil, fmt.Errorf("notification target %q: %w", n.Name, err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(plaintext, &values); err != nil {
		return nil, fmt.Errorf("notification target %q: %w", n.Name, err)
	}
	return values, nil
}
