package store_test

import (
	"testing"

	"dispatch-ops/internal/store"
)

// R2-L2: sign-in throttling counts IPv6 clients by /64, except translated
// and tunnelled IPv4 clients.
func TestLoginClientKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":          "203.0.113.5",
		"::ffff:203.0.113.5":   "203.0.113.5",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2::1":      "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"fe80::1%eth0":         "fe80::/64",
		"::1":                  "::/64",
		// NAT64 and Teredo clients are keyed by their IPv4 address.
		"64:ff9b::203.0.113.5":                 "203.0.113.5",
		"64:ff9b::cb00:7106":                   "203.0.113.6",
		"64:ff9b:1:2:3::198.51.100.7":          "198.51.100.7",
		"64:ff9b:2::1":                         "64:ff9b:2::/64",
		"2001:0:4136:e378:8000:63bf:3fff:fdd2": "192.0.2.45",
		"2001:0:4136:e378:8000:1111:3fff:fdd2": "192.0.2.45", // another mapped port
		"2001:0:4136:e378:8000:63bf:3fff:fdd3": "192.0.2.44",
		"2001:1::1":                            "2001:1::/64", // outside 2001::/32
		"2001:db8::1":                          "2001:db8::/64",
		"":                                     "",
		"not an address":                       "not an address",
	} {
		if got := store.LoginClientKey(in); got != want {
			t.Errorf("LoginClientKey(%q) = %q, want %q", in, got, want)
		}
	}
}
