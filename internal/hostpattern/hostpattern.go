// Package hostpattern checks Ansible host patterns (the --limit option)
// and host names for the one form Ansible treats as an instruction rather
// than a pattern: an element starting with @, which makes Ansible read the
// named file as a list of patterns and echo each line it can't match into
// the job output. Since job pods hold the decrypted credentials at fixed
// paths, a limit such as @/runner/secrets/credentials.json would print them.
//
// Ansible splits a limit on commas, or else on colons and brackets, and
// strips whitespace (in Python's broad sense) around each element. Rather
// than mirror that, an @ is allowed only right after a letter, digit, '.',
// '_' or '-', which can't end a separator or whitespace; so no element, as
// Ansible splits it, can start with @.
package hostpattern

import (
	"fmt"
	"regexp"
)

// LimitPattern is the regular expression form of CheckLimit, for the API
// schema (it needs no look-behind, so it works in Go and ECMAScript).
const LimitPattern = `^(?:[^@]*[A-Za-z0-9._-]@)*[^@]*$`

// HostNamePattern is the regular expression form of CheckHostName.
const HostNamePattern = `^(?:[^\s,@]*[A-Za-z0-9._-]@)*[^\s,@]*$`

var (
	limitRE    = regexp.MustCompile(LimitPattern)
	hostNameRE = regexp.MustCompile(HostNamePattern)
)

// CheckLimit returns an error if Ansible could read a file named in the
// limit pattern s.
func CheckLimit(s string) error {
	if !limitRE.MatchString(s) {
		return fmt.Errorf("limit %q names a file (@path); only host and group patterns are allowed", s)
	}
	return nil
}

// CheckHostName returns an error if name can't be used as a host name in
// a limit: it has whitespace or a comma (which separate patterns), or an
// @ that could start a file reference.
func CheckHostName(name string) error {
	if !hostNameRE.MatchString(name) {
		return fmt.Errorf("host name %q can't contain whitespace or commas, or @ other than after a letter, digit, '.', '_' or '-'", name)
	}
	return nil
}
