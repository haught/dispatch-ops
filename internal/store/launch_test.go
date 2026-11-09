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
		// Magic variable names are refused only at the top level, where they
		// would set the variable.
		{ExtraVars: map[string]any{"app": map[string]any{"inventory_hostname": "x", "groups": []any{"a"}}}},
		{ExtraVars: map[string]any{"jinja2": "#jinja2 is a template engine", "my_inventory_hostname": "x"}},
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
		// R2-H1: a #jinja2: header sets other delimiters.
		{ExtraVars: map[string]any{"greeting": "#jinja2:variable_start_string:'[[', variable_end_string:']]'\n[[ lookup('file', '/runner/secrets/credentials.json') ]]"}},
		{ExtraVars: map[string]any{"greeting": "  \n#JINJA2:variable_start_string:'[['"}},
		{ExtraVars: map[string]any{"x": map[string]any{"y": []any{"#jinja2:block_start_string:'<%'"}}}},
		{ExtraVars: map[string]any{"x": map[string]any{"#jinja2:": "v"}}},
		{ExtraVars: map[string]any{"#jinja2:x": "v"}},
		// R2-M3: magic variables, inventory_hostname redirects the connection.
		{ExtraVars: map[string]any{"inventory_hostname": "evil.invalid"}},
		{ExtraVars: map[string]any{"Inventory_HostName_Short": "evil"}},
		{ExtraVars: map[string]any{"hostvars": map[string]any{}}},
		{ExtraVars: map[string]any{"groups": map[string]any{}}},
		{ExtraVars: map[string]any{"group_names": []any{}}},
		{ExtraVars: map[string]any{"inventory_dir": "/tmp"}},
		{ExtraVars: map[string]any{"inventory_file": "/tmp/x"}},
		{ExtraVars: map[string]any{"playbook_dir": "/tmp"}},
		{ExtraVars: map[string]any{"role_path": "/tmp"}},
		{ExtraVars: map[string]any{"OMIT": "x"}},
		{ExtraVars: map[string]any{"_ansible_coverage_remote_output": "/tmp/x"}},
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
