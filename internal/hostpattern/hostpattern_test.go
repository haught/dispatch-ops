package hostpattern

import "testing"

func TestCheckLimit(t *testing.T) {
	ok := []string{
		"", "web", "web,db", "web:db", "web:&prod:!db", "web[0:3]", "~web\\d+", "10.0.0.1", "fe80::1",
		"user@host", "a.b-c_d@e", "web, db", "all",
	}
	bad := []string{
		"@/runner/secrets/credentials.json", "@/etc/passwd", "web,@/etc/passwd", "web, @/etc/passwd",
		"web:@/etc/passwd", "!@/etc/passwd", "&@/etc/passwd", "web\t@x", "web]@x", "web[@x",
		"web, @x", "web @x", "web\x1f@x", " @x", "a@@x",
	}
	for _, s := range ok {
		if err := CheckLimit(s); err != nil {
			t.Errorf("CheckLimit(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range bad {
		if err := CheckLimit(s); err == nil {
			t.Errorf("CheckLimit(%q) = nil, want an error", s)
		}
	}
}

func TestCheckHostName(t *testing.T) {
	ok := []string{"web1", "web1.example.com", "10.0.0.1", "fe80::1", "user@host"}
	bad := []string{"@/etc/passwd", "a,b", "a b", "a:@x", "a,@x", "a\t@x"}
	for _, s := range ok {
		if err := CheckHostName(s); err != nil {
			t.Errorf("CheckHostName(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range bad {
		if err := CheckHostName(s); err == nil {
			t.Errorf("CheckHostName(%q) = nil, want an error", s)
		}
	}
}
