package credentials

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/store"
)

// Vault encrypts and decrypts credential inputs.
type Vault struct {
	Keys *secrets.Keyring
}

func aad(c *store.Credential) []byte {
	return []byte("dispatch-ops/credential/" + c.ID.String())
}

// Seal validates a complete set of inputs for c.Kind and stores them on c:
// public values in clear, secret values encrypted. c.ID must be set.
func (v Vault) Seal(c *store.Credential, inputs map[string]string) error {
	kind, ok := Lookup(c.Kind)
	if !ok {
		return fmt.Errorf("unknown credential kind %q", c.Kind)
	}
	public, secret, err := kind.Split(inputs)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(secret)
	if err != nil {
		return err
	}
	ct, keyID, err := v.Keys.Encrypt(plaintext, aad(c))
	if err != nil {
		return err
	}
	c.PublicInputs = public
	c.SecretFields = slices.Sorted(maps.Keys(secret))
	c.SecretInputs = ct
	c.KeyID = keyID
	return nil
}

// Inputs returns all of c's inputs, secret ones decrypted.
func (v Vault) Inputs(c store.Credential) (map[string]string, error) {
	plaintext, err := v.Keys.Decrypt(c.SecretInputs, c.KeyID, aad(&c))
	if err != nil {
		return nil, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	inputs := map[string]string{}
	if err := json.Unmarshal(plaintext, &inputs); err != nil {
		return nil, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	maps.Copy(inputs, c.PublicInputs)
	return inputs, nil
}
