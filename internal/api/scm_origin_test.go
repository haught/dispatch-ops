package api

import "testing"

// R2-M2: a look-alike host such as gİthub.com, which Go lower-cases to
// github.com, is refused, and never shares github.com's origin.
func TestScmURLNonASCIIHost(t *testing.T) {
	for _, u := range []string{
		"https://gİthub.com/x.git",
		"https://g%C4%B0thub.com/x.git",
		"https://g%25C4%25B0thub.com/x.git",
		"https://bot@gİthub.com/x.git",
		"ssh://git@gİthub.com/x.git",
		"https://ｇithub.com/x.git",
		"https://github.com。evil.example/x.git",
	} {
		if err := checkScmURL(u); err == nil {
			t.Errorf("checkScmURL(%q) = ok, want refused", u)
		}
		if o := scmOrigin(u); o == scmOrigin("https://github.com/x.git") || o == scmOrigin("ssh://git@github.com/x.git") {
			t.Errorf("scmOrigin(%q) = %q, github.com's", u, o)
		}
	}
	for _, u := range []string{
		"https://github.com/acme/x.git",
		"https://xn--github-qyd.com/x.git",
		"https://GitHub.com/acme/ünïcode-path.git",
		"git@github.com:acme/x.git",
		"file:///srv/repos/x.git",
	} {
		if err := checkScmURL(u); err != nil {
			t.Errorf("checkScmURL(%q) = %v, want ok", u, err)
		}
	}
	if a, b := scmOrigin("https://GitHub.com/a.git"), scmOrigin("https://github.com:443/b.git"); a != b {
		t.Errorf("origins differ: %q, %q", a, b)
	}
}
