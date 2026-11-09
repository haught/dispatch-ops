// Package credentials defines the credential kinds and validates their inputs.
package credentials

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"dispatch-ops/internal/envname"
)

// Field is one input of a credential kind.
type Field struct {
	Name      string
	Label     string
	Secret    bool
	Required  bool
	Multiline bool
	// Destination fields decide where the secret values are sent (a
	// registry's server, an API's URL). Changing one needs the secrets
	// given again (see Kind.CheckDestinationChange), so whoever may edit a
	// credential can't redirect secrets they don't know to their own host.
	Destination bool
	// Pattern, if set, restricts the value; PatternHelp describes it.
	Pattern     *regexp.Regexp
	PatternHelp string
}

// Kind describes a type of credential.
type Kind struct {
	Name  string
	Label string
	// Freeform kinds accept any input names; all their values are secret.
	// Jobs also get each input as an environment variable of that name, so
	// names that envname.Reserved reports are refused.
	Freeform bool
	Fields   []Field
}

// Kinds lists every credential kind. The names match the credentials.kind
// CHECK constraint in the database.
var Kinds = []Kind{
	{Name: "ssh", Label: "SSH key", Fields: []Field{
		{Name: "username", Label: "Username"},
		{Name: "private_key", Label: "Private key", Secret: true, Required: true, Multiline: true},
		{Name: "passphrase", Label: "Key passphrase", Secret: true},
		{Name: "become_password", Label: "Become (sudo) password", Secret: true},
	}},
	{Name: "username_password", Label: "Username and password", Fields: []Field{
		{Name: "username", Label: "Username", Required: true},
		{Name: "password", Label: "Password", Secret: true, Required: true},
		{Name: "become_password", Label: "Become (sudo) password", Secret: true},
	}},
	{Name: "api_token", Label: "API token", Fields: []Field{
		{Name: "url", Label: "API URL", Destination: true},
		{Name: "token", Label: "Token", Secret: true, Required: true},
	}},
	{Name: "registry", Label: "Container registry", Fields: []Field{
		{Name: "server", Label: "Registry server (e.g. ghcr.io)", Required: true, Destination: true},
		{Name: "username", Label: "Username", Required: true},
		{Name: "password", Label: "Password or token", Secret: true, Required: true},
	}},
	{Name: "scm", Label: "Source control", Fields: []Field{
		{Name: "username", Label: "Username"},
		{Name: "password", Label: "Password or token", Secret: true},
		{Name: "private_key", Label: "SSH private key", Secret: true, Multiline: true},
		{Name: "passphrase", Label: "Key passphrase", Secret: true},
	}},
	{Name: "vault", Label: "Ansible Vault password", Fields: []Field{
		{Name: "vault_id", Label: "Vault ID (optional)", Pattern: vaultID,
			PatternHelp: "letters, digits, and . _ -"},
		{Name: "password", Label: "Vault password", Secret: true, Required: true},
	}},
	{Name: "generic", Label: "Generic secrets (key/value)", Freeform: true},
}

// Lookup returns the kind with the given name.
func Lookup(name string) (Kind, bool) {
	i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.Name == name })
	if i < 0 {
		return Kind{}, false
	}
	return Kinds[i], true
}

var vaultID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// freeformName keeps generic input names usable as file and variable names.
var freeformName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidationError reports inputs that don't fit the credential kind.
type ValidationError struct {
	msg string
}

func (e *ValidationError) Error() string { return e.msg }

// CheckDestinationChange returns a ValidationError if an update of a
// credential of kind k changes a Destination field without giving every
// secret value the credential holds again. oldPublic are the credential's
// public values before the update, secretNames the names of the secrets it
// holds, and changes the update's inputs (an empty value removes one).
func (k Kind) CheckDestinationChange(oldPublic map[string]string, secretNames []string, changes map[string]string) error {
	var moved []string
	for _, f := range k.Fields {
		if v, ok := changes[f.Name]; f.Destination && ok && v != oldPublic[f.Name] {
			moved = append(moved, f.Name)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	var missing []string
	for _, name := range secretNames {
		if changes[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &ValidationError{fmt.Sprintf("changing %s decides where this credential's secrets are sent, so %s must be given again in the same change",
		strings.Join(moved, " and "), strings.Join(missing, ", "))}
}

// Split validates a complete set of inputs for the kind and separates the
// public values from the secret ones. Empty values are treated as absent.
func (k Kind) Split(inputs map[string]string) (public, secret map[string]string, err error) {
	public, secret = map[string]string{}, map[string]string{}
	var problems []string

	if k.Freeform {
		for name, v := range inputs {
			if !freeformName.MatchString(name) {
				problems = append(problems, fmt.Sprintf("%q is not a valid name (letters, digits and underscores)", name))
				continue
			}
			if envname.Reserved(name) {
				problems = append(problems, fmt.Sprintf("%q is reserved: each name is also set as an environment variable in jobs", name))
				continue
			}
			if v != "" {
				secret[name] = v
			}
		}
		if len(secret) == 0 && len(problems) == 0 {
			problems = append(problems, "at least one value is required")
		}
	} else {
		for name := range inputs {
			if !slices.ContainsFunc(k.Fields, func(f Field) bool { return f.Name == name }) {
				problems = append(problems, fmt.Sprintf("unknown field %q", name))
			}
		}
		for _, f := range k.Fields {
			v := inputs[f.Name]
			switch {
			case v == "" && f.Required:
				problems = append(problems, fmt.Sprintf("%q is required", f.Name))
			case v == "":
			case f.Pattern != nil && !f.Pattern.MatchString(v):
				problems = append(problems, fmt.Sprintf("%q may only contain %s", f.Name, f.PatternHelp))
			case f.Secret:
				secret[f.Name] = v
			default:
				public[f.Name] = v
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, nil, &ValidationError{fmt.Sprintf("invalid %s credential: %s", k.Name, strings.Join(problems, "; "))}
	}
	return public, secret, nil
}
