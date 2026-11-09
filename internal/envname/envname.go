// Package envname decides which environment variable names a generic
// credential may set in a job's environment. The API refuses reserved
// names when a credential is saved; the runner skips them as well, for
// credentials saved before the rule existed.
//
// A name a credential can set is one its owner controls in every job that
// uses the credential. Anything that makes a shell, interpreter, linker or
// network client load code, change what it trusts or route its traffic would
// let a credential owner run code in those jobs and read the job's other
// credentials, so all such names are reserved. Names are compared without
// regard to case: some tools honor lower-case variants (http_proxy), and no
// credential needs a lower-case twin of a reserved name.
package envname

import "strings"

// reservedNames are variables the job's tools depend on, or that make a
// shell or interpreter run code, load libraries or modules, or change where
// network traffic goes or which TLS certificates are trusted.
var reservedNames = map[string]bool{
	// Basic process environment.
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PWD": true, "OLDPWD": true,
	"LANG": true, "LANGUAGE": true, "TMPDIR": true, "HOSTNAME": true, "TERM": true, "TZ": true, "TZDIR": true,
	// Shells: startup files and settings that run code.
	"ENV": true, "BASH_ENV": true, "SHELLOPTS": true, "BASHOPTS": true, "PS4": true, "PROMPT_COMMAND": true,
	"IFS": true, "CDPATH": true, "GLOBIGNORE": true, "INPUTRC": true, "HISTFILE": true,
	// Perl, Ruby, Node.js, Java and others.
	"PERL5OPT": true, "PERL5LIB": true, "PERLLIB": true, "PERL5DB": true, "PERL_MB_OPT": true, "PERL_MM_OPT": true,
	"RUBYOPT": true, "RUBYLIB": true, "GEM_HOME": true, "GEM_PATH": true,
	"NODE_OPTIONS": true, "NODE_PATH": true,
	"JAVA_TOOL_OPTIONS": true, "_JAVA_OPTIONS": true, "JDK_JAVA_OPTIONS": true, "CLASSPATH": true,
	"TCLLIBPATH": true, "LUA_PATH": true, "LUA_CPATH": true, "LUA_INIT": true,
	// glibc: library loading, locale and resolver behavior.
	"GCONV_PATH": true, "GLIBC_TUNABLES": true, "LOCPATH": true, "NLSPATH": true, "HOSTALIASES": true,
	"RES_OPTIONS": true, "LOCALDOMAIN": true, "MALLOC_CHECK_": true,
	// Programs other tools start.
	"EDITOR": true, "VISUAL": true, "PAGER": true, "BROWSER": true,
	// Config file locations that tools read code or settings from.
	"XDG_CONFIG_HOME": true, "XDG_CONFIG_DIRS": true, "XDG_DATA_HOME": true, "XDG_DATA_DIRS": true,
	"XDG_RUNTIME_DIR": true, "GNUPGHOME": true, "WGETRC": true, "CURL_HOME": true, "NETRC": true,
	// Which TLS certificate authorities are trusted.
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true, "REQUESTS_CA_BUNDLE": true,
	"OPENSSL_CONF": true, "OPENSSL_MODULES": true, "OPENSSL_ENGINES": true, "NODE_EXTRA_CA_CERTS": true,
}

// reservedPrefixes cover the runner's own settings and variables that change
// how Ansible, Python, Git, SSH, pip, Bash or the dynamic linker behave.
var reservedPrefixes = []string{
	"DISPATCH_", "ANSIBLE_", "LD_", "LC_", "PYTHON", "PIP_", "VIRTUAL_ENV", "GIT_", "SSH_", "KUBERNETES_",
	"BASH_FUNC_", "MALLOC_", "KRB5",
}

// reservedSuffixes route a tool's traffic through another host:
// HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, NO_PROXY, FTP_PROXY and the like.
var reservedSuffixes = []string{"_PROXY"}

// Reserved reports whether a credential may not set the variable name.
func Reserved(name string) bool {
	n := strings.ToUpper(name)
	if reservedNames[n] {
		return true
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	for _, s := range reservedSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}
