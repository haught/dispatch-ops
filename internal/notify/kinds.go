// Package notify sends job notifications to webhooks, Slack and email.
//
// A notification target has a kind, a non-secret config (shown to
// administrators) and secret values (tokens, passwords), which are
// encrypted at rest and never returned by the API.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/url"
	"slices"
	"strings"
)

// Target kinds.
const (
	KindWebhook = "webhook"
	KindSlack   = "slack"
	KindEmail   = "email"
)

// Kinds lists the target kinds.
var Kinds = []string{KindWebhook, KindSlack, KindEmail}

// Events a template's jobs can notify about.
const (
	EventStarted   = "started"
	EventSucceeded = "succeeded"
	EventFailed    = "failed"
	EventCanceled  = "canceled"
	// EventTest is sent by the API's test action.
	EventTest = "test"
)

// Secret fields per kind, and which are required.
var secretFields = map[string]struct{ all, required []string }{
	KindWebhook: {all: []string{"authorization", "signingSecret"}},
	KindSlack:   {all: []string{"webhookUrl"}, required: []string{"webhookUrl"}},
	KindEmail:   {all: []string{"password"}},
}

// WebhookConfig is a webhook target's config.
type WebhookConfig struct {
	// URL receives a POST with the event as JSON.
	URL string `json:"url"`
}

// SlackConfig is a Slack target's config. The incoming webhook URL is a
// secret (webhookUrl), since it is all it takes to post.
type SlackConfig struct{}

// Email security modes.
const (
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// EmailConfig is an email target's config. The SMTP password is a secret.
type EmailConfig struct {
	Host string `json:"host"`
	// Port defaults to 587 (465 with security "tls").
	Port int `json:"port,omitempty"`
	// Security is starttls (the default), tls (implicit TLS) or none.
	Security string   `json:"security,omitempty"`
	Username string   `json:"username,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`
}

// NormalizeConfig validates a target's config for its kind and returns it
// with defaults filled in.
func NormalizeConfig(kind string, config map[string]any) (map[string]any, error) {
	if config == nil {
		config = map[string]any{}
	}
	switch kind {
	case KindWebhook:
		var c WebhookConfig
		if err := decodeStrict(config, &c); err != nil {
			return nil, err
		}
		if err := checkHTTPURL(c.URL, false); err != nil {
			return nil, fmt.Errorf("url: %w", err)
		}
		return toMap(c)
	case KindSlack:
		var c SlackConfig
		if err := decodeStrict(config, &c); err != nil {
			return nil, fmt.Errorf("%w (the Slack webhook URL is the secret webhookUrl)", err)
		}
		return toMap(c)
	case KindEmail:
		var c EmailConfig
		if err := decodeStrict(config, &c); err != nil {
			return nil, err
		}
		if err := c.normalize(); err != nil {
			return nil, err
		}
		return toMap(c)
	}
	return nil, fmt.Errorf("unknown notification target kind %q", kind)
}

func (c *EmailConfig) normalize() error {
	c.Host = strings.TrimSpace(c.Host)
	if c.Host == "" || strings.ContainsAny(c.Host, " /:") {
		return errors.New("host must be the SMTP server's host name")
	}
	switch c.Security {
	case "":
		c.Security = SecurityStartTLS
	case SecurityStartTLS, SecurityTLS, SecurityNone:
	default:
		return fmt.Errorf("security must be starttls, tls or none, not %q", c.Security)
	}
	if c.Port == 0 {
		c.Port = 587
		if c.Security == SecurityTLS {
			c.Port = 465
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("from: %v", err)
	}
	if len(c.To) == 0 {
		return errors.New("to needs at least one address")
	}
	for _, to := range c.To {
		if _, err := mail.ParseAddress(to); err != nil {
			return fmt.Errorf("to %q: %v", to, err)
		}
	}
	return nil
}

// CheckSecrets checks a target's complete set of secret values for its
// kind: only known fields, and the required ones set.
func CheckSecrets(kind string, config map[string]any, secrets map[string]string) error {
	f, ok := secretFields[kind]
	if !ok {
		return fmt.Errorf("unknown notification target kind %q", kind)
	}
	for k := range secrets {
		if !slices.Contains(f.all, k) {
			if len(f.all) == 0 {
				return fmt.Errorf("%s targets have no secrets; got %q", kind, k)
			}
			return fmt.Errorf("unknown secret %q for %s targets (expected %s)", k, kind, strings.Join(f.all, ", "))
		}
	}
	for _, k := range f.required {
		if secrets[k] == "" {
			return fmt.Errorf("%s targets need the secret %s", kind, k)
		}
	}
	switch kind {
	case KindSlack:
		if err := checkHTTPURL(secrets["webhookUrl"], true); err != nil {
			return fmt.Errorf("webhookUrl: %w", err)
		}
	case KindEmail:
		var c EmailConfig
		if err := decodeStrict(config, &c); err != nil {
			return err
		}
		if c.Username != "" && secrets["password"] == "" {
			return errors.New("an email target with a username needs the secret password")
		}
		if c.Username != "" && c.Security == SecurityNone {
			return errors.New("SMTP authentication needs an encrypted connection; use security starttls or tls")
		}
	}
	return nil
}

// SecretFields returns the names of the secret fields kind has.
func SecretFields(kind string) []string { return slices.Clone(secretFields[kind].all) }

// MergeSecrets applies changes to current: a key with a value replaces it,
// and a key with an empty value removes it.
func MergeSecrets(current, changes map[string]string) map[string]string {
	out := maps.Clone(current)
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range changes {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func checkHTTPURL(raw string, httpsOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (httpsOnly || u.Scheme != "http")) {
		if httpsOnly {
			return errors.New("must be an https URL")
		}
		return errors.New("must be an http or https URL")
	}
	return nil
}

func decodeStrict(in map[string]any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid config: %v", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, json.Unmarshal(b, &m)
}
