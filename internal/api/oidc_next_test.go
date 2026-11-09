package api

import "testing"

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/":                       "/",
		"/jobs":                   "/jobs",
		"/jobs/123?tab=output#x":  "/jobs/123?tab=output#x",
		"/templates/a%20b":        "/templates/a%20b",
		"":                        "/",
		"jobs":                    "/",
		"//evil.com":              "/",
		"///evil.com":             "/",
		"/\\evil.com":             "/",
		"\\\\evil.com":            "/",
		"/\t/evil.com":            "/",
		"/\n/evil.com":            "/",
		"/\r/evil.com":            "/",
		"/\x00/evil.com":          "/",
		"/\x7f/evil.com":          "/",
		"/%09/evil.com":           "/",
		"/%0a/evil.com":           "/",
		"/%2F/evil.com":           "/",
		"/%5Cevil.com":            "/",
		"/%5c%5cevil.com":         "/",
		"https://evil.com":        "/",
		"http:/evil.com":          "/",
		"javascript:alert(1)":     "/",
		"/ok?x=%zz":               "/",
		" /jobs":                  "/",
		"/jobs?next=//evil.com":   "/jobs?next=//evil.com",
		"/jobs?next=https://x.io": "/jobs?next=https://x.io",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
