package envname

import "testing"

func TestReserved(t *testing.T) {
	reserved := []string{
		"PATH", "path", "HOME", "DISPATCH_TOKEN", "ANSIBLE_CONFIG", "LD_PRELOAD", "PYTHONPATH",
		"GIT_SSH_COMMAND", "SSH_AUTH_SOCK",
		// L1: names that make shells and interpreters run code, or reroute traffic.
		"BASH_ENV", "ENV", "PERL5OPT", "PERL5LIB", "NODE_OPTIONS", "RUBYOPT", "JAVA_TOOL_OPTIONS",
		"BASH_FUNC_foo%%", "SHELLOPTS", "PS4", "PROMPT_COMMAND", "GCONV_PATH", "GLIBC_TUNABLES",
		"HTTP_PROXY", "https_proxy", "ALL_PROXY", "no_proxy", "FTP_PROXY",
		"PIP_INDEX_URL", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "XDG_CONFIG_HOME", "EDITOR",
	}
	allowed := []string{
		"AKIPS_PASSWORD", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "VAULT_TOKEN", "API_KEY",
		"MY_ENV", "ENVIRONMENT", "PROXY_PASSWORD", "NETBOX_TOKEN",
	}
	for _, n := range reserved {
		if !Reserved(n) {
			t.Errorf("Reserved(%q) = false, want true", n)
		}
	}
	for _, n := range allowed {
		if Reserved(n) {
			t.Errorf("Reserved(%q) = true, want false", n)
		}
	}
}
