package extravars

import "testing"

func TestRefusedName(t *testing.T) {
	for _, name := range []string{
		"ansible_host", "ANSIBLE_SSH_COMMON_ARGS", "_ansible_debugpy_config",
		"inventory_hostname", "Inventory_Hostname", "inventory_hostname_short", "hostvars", "groups",
		"group_names", "inventory_dir", "inventory_file", "playbook_dir", "role_path", "omit",
	} {
		if RefusedName(name) == "" {
			t.Errorf("RefusedName(%q) = ok, want refused", name)
		}
	}
	for _, name := range []string{"greeting", "host", "inventory", "my_groups", "ansible"} {
		if why := RefusedName(name); why != "" {
			t.Errorf("RefusedName(%q) = %q, want ok", name, why)
		}
	}
}

func TestCheck(t *testing.T) {
	ok := []map[string]any{
		nil,
		{"color": "blue", "n": 3.0, "nested": map[string]any{"inventory_hostname": "x", "ansible_host": "y"}},
		{"text": "a { b } % # jinja2"},
		{"x": map[string]any{"_ansible_vault": 1, "a__ansible_vault": 2}, "s": "__ansible_vault"},
	}
	for _, v := range ok {
		if err := Check(v); err != nil {
			t.Errorf("Check(%v) = %v, want ok", v, err)
		}
	}
	bad := []map[string]any{
		{"inventory_hostname": "evil.invalid"},
		{"x": "#jinja2:variable_start_string:'[['\n[[ 1 ]]"},
		{"x": []any{map[string]any{"#Jinja2:": 1}}},
		{"x": "{{ 1 }}"},
		{"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256\n6162"},
		{"__Ansible_Unsafe": "x"},
		{"x": map[string]any{"__ansible_vault": "$ANSIBLE_VAULT;1.1;AES256\n6162"}},
		{"x": []any{1, map[string]any{"y": map[string]any{"__ANSIBLE_VAULT": "c"}}}},
		{"x": map[string]any{"__ansible_future": 1}},
	}
	for _, v := range bad {
		if err := Check(v); err == nil {
			t.Errorf("Check(%v) = ok, want an error", v)
		}
	}
}

func TestCheckRefusesDeepValues(t *testing.T) {
	nest := func(n int) any {
		var v any = "x"
		for range n {
			v = []any{v}
		}
		return v
	}
	if err := Check(map[string]any{"v": nest(MaxDepth)}); err != nil {
		t.Errorf("Check(depth %d) = %v, want ok", MaxDepth, err)
	}
	if err := Check(map[string]any{"v": nest(5000)}); err == nil {
		t.Error("Check(depth 5000) = nil, want an error")
	}
}
