// Package envname decides which environment variable names a generic
// credential may set in a job's environment. The API refuses reserved
// names when a credential is saved; the runner skips them as well, for
// credentials saved before the rule existed.
package envname

import "strings"

// reservedNames are variables the job's tools depend on.
var reservedNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true, "PWD": true,
	"LANG": true, "TMPDIR": true, "HOSTNAME": true,
}

// reservedPrefixes cover the runner's own settings and variables that change
// how Ansible, Python, Git, SSH or the dynamic linker behave.
var reservedPrefixes = []string{"DISPATCH_", "ANSIBLE_", "LD_", "LC_", "PYTHON", "GIT_", "SSH_", "KUBERNETES_"}

// Reserved reports whether a credential may not set the variable name.
func Reserved(name string) bool {
	if reservedNames[name] {
		return true
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
