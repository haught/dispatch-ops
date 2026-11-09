// Package extravars checks the extra vars a launcher gives a job, as
// opposed to the ones its template sets. The API checks them when a job is
// launched (by a user, a schedule or a relaunch), and the runner checks
// them again before it hands them to Ansible.
package extravars

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// magicNames are top-level variables, other than ansible_*, that a
// launcher may not set, lower-cased. inventory_hostname is the only
// variable that a connection, become or shell plugin of ansible-core 2.22
// reads that doesn't start with ansible_ (it is the ssh, winrm and psrp
// plugins' default host, so setting it points the connection, and the
// machine credential, at another host). The others are Ansible's magic
// variables, which playbooks trust to describe the inventory and the play.
var magicNames = map[string]bool{
	"inventory_hostname":       true,
	"inventory_hostname_short": true,
	"hostvars":                 true,
	"groups":                   true,
	"group_names":              true,
	"inventory_dir":            true,
	"inventory_file":           true,
	"playbook_dir":             true,
	"role_path":                true,
	"omit":                     true,
}

// RefusedName returns why a launcher may not set the top-level variable
// name, or "" if it may. Names are compared case-insensitively.
func RefusedName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "ansible_"), strings.HasPrefix(lower, "_ansible_"):
		return "ansible_* variables control how Ansible connects to hosts"
	case magicNames[lower]:
		return "it is one of Ansible's magic variables, which say how and where Ansible connects"
	}
	return ""
}

// HasJinja reports whether s contains the start of a Jinja expression,
// statement or comment, or a #jinja2: header, which makes Ansible treat
// the string as a template with delimiters of its choosing.
func HasJinja(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%") || strings.Contains(s, "{#") ||
		strings.Contains(strings.ToLower(s), "#jinja2:")
}

// FindJinja looks for Jinja (see HasJinja) in the key at path and in the
// strings of v, keys included, at any depth, and returns where it is.
func FindJinja(path string, v any) (string, bool) {
	if HasJinja(path) {
		return path, true
	}
	switch v := v.(type) {
	case string:
		return path, HasJinja(v)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if HasJinja(k) {
				return path + "." + k, true
			}
			if p, bad := FindJinja(path+"."+k, v[k]); bad {
				return p, true
			}
		}
	case []any:
		for i, x := range v {
			if p, bad := FindJinja(fmt.Sprintf("%s[%d]", path, i), x); bad {
				return p, true
			}
		}
	}
	return "", false
}

// IsAnsibleMarker reports whether key, a key at any depth, is one Ansible's
// JSON loader may treat as a marker (__ansible_vault, __ansible_unsafe and
// any it adds later), compared case-insensitively. A dict with
// __ansible_vault would otherwise be decrypted with the job's vault
// password.
func IsAnsibleMarker(key string) bool {
	return strings.HasPrefix(strings.ToLower(key), "__ansible_")
}

// FindMarkerKey looks for a key, at any depth of v (whose own key is
// path), that IsAnsibleMarker, and returns where it is.
func FindMarkerKey(path string, v any) (string, bool) {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if IsAnsibleMarker(k) {
				return path + "." + k, true
			}
			if p, bad := FindMarkerKey(path+"."+k, v[k]); bad {
				return p, true
			}
		}
	case []any:
		for i, x := range v {
			if p, bad := FindMarkerKey(fmt.Sprintf("%s[%d]", path, i), x); bad {
				return p, true
			}
		}
	}
	return "", false
}

// Check returns an error if vars, a launcher's extra vars, could reach the
// job's credentials rather than just steer the playbook:
//
//   - variables named ansible_* or _ansible_*, which set how Ansible
//     connects and becomes (ansible_ssh_common_args can run a command, for
//     one), and the magic variables in magicNames (inventory_hostname
//     redirects the connection);
//   - Jinja ({{, {% or {#) or a #jinja2: header in a key or string value at
//     any depth: Ansible evaluates extra vars as trusted templates, so
//     lookup('file', …) or pipe(…) could read the credential files;
//   - keys starting with __ansible_ at any depth, which Ansible's JSON
//     loader turns into vaulted or unsafe values: {"__ansible_vault": …}
//     would be decrypted with the template's vault password.
//
// It also refuses values nested deeper than MaxDepth: Ansible's parsers
// recurse, so a deeper value would fail the job rather than load.
//
// The message names the variable and is meant for the launcher.
func Check(vars map[string]any) error {
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if depth(vars[k]) > MaxDepth {
			return fmt.Errorf("extra var %q is nested more than %d levels deep", k, MaxDepth)
		}
		if why := RefusedName(k); why != "" {
			return fmt.Errorf("extra var %q can't be overridden at launch: %s", k, why)
		}
		if IsAnsibleMarker(k) {
			return fmt.Errorf("extra var %q can't be set at launch: keys starting with __ansible_ are Ansible's markers for vaulted and unsafe values", k)
		}
		if path, bad := FindMarkerKey(k, vars[k]); bad {
			return fmt.Errorf("extra var %q can't be set at launch: keys starting with __ansible_ are Ansible's markers for vaulted and unsafe values", path)
		}
		if path, bad := FindJinja(k, vars[k]); bad {
			return fmt.Errorf("extra var %q can't be set to a Jinja template ({{, {%%, {# or #jinja2:) at launch", path)
		}
	}
	return nil
}

// MaxDepth is how deeply a launcher's extra var may nest lists and maps.
const MaxDepth = 64

// depth is how deeply v nests lists and maps; it stops counting past
// MaxDepth.
func depth(v any) int {
	return depthFrom(v, 0)
}

func depthFrom(v any, d int) int {
	if d > MaxDepth {
		return d
	}
	deepest := d
	switch v := v.(type) {
	case map[string]any:
		for _, e := range v {
			deepest = max(deepest, depthFrom(e, d+1))
		}
	case []any:
		for _, e := range v {
			deepest = max(deepest, depthFrom(e, d+1))
		}
	}
	return deepest
}
