package store

import (
	"errors"
	"testing"
)

func TestLaunchOverridesCheckValues(t *testing.T) {
	str := func(s string) *string { return &s }
	ok := []LaunchOverrides{
		{},
		{Limit: str("web,db:&prod")},
		{ExtraVars: map[string]any{"color": "blue", "n": 3.0, "list": []any{"a", map[string]any{"b": "c"}}}},
		{ExtraVars: map[string]any{"text": "a { b } %"}},
	}
	bad := []LaunchOverrides{
		{Limit: str("@/etc/passwd")},
		{Limit: str("web,@/runner/secrets/credentials.json")},
		{ExtraVars: map[string]any{"ansible_ssh_common_args": "-o ProxyCommand=true"}},
		{ExtraVars: map[string]any{"Ansible_Become_Exe": "sh"}},
		{ExtraVars: map[string]any{"x": "{{ lookup('file', '/runner/secrets/credentials.json') }}"}},
		{ExtraVars: map[string]any{"x": "{% if true %}y{% endif %}"}},
		{ExtraVars: map[string]any{"x": "{# c #}"}},
		{ExtraVars: map[string]any{"{{ x }}": "y"}},
		{ExtraVars: map[string]any{"x": map[string]any{"{{ k }}": "v"}}},
		{ExtraVars: map[string]any{"x": []any{"a", []any{"{{ pipe('id') }}"}}}},
	}
	for _, o := range ok {
		if err := o.CheckValues(); err != nil {
			t.Errorf("CheckValues(%+v) = %v, want ok", o, err)
		}
	}
	for _, o := range bad {
		var le *LaunchError
		if err := o.CheckValues(); !errors.As(err, &le) {
			t.Errorf("CheckValues(%+v) = %v, want a LaunchError", o, err)
		}
	}
}
