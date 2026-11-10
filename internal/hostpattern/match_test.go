package hostpattern

import (
	"slices"
	"strings"
	"testing"
)

// testInventory:
//
//	web: web1 web2 web3        prod: children web, db; host lb1
//	db: db1 db2                staging: web3 stg1
//	(ungrouped: solo, 10.0.0.5, fe80::1)
var testInventory = Inventory{
	Groups: []Group{
		{Name: "web", Hosts: []string{"web3", "web1", "web2"}},
		{Name: "db", Hosts: []string{"db1", "db2"}},
		{Name: "prod", Children: []string{"web", "db"}, Hosts: []string{"lb1"}},
		{Name: "staging", Hosts: []string{"web3", "stg1"}},
	},
	Hosts: []string{"web1", "web2", "web3", "db1", "db2", "lb1", "stg1", "solo", "10.0.0.5", "fe80::1"},
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern   string
		want      string // space-separated, in order
		unmatched string
	}{
		{"", "10.0.0.5 db1 db2 fe80::1 lb1 solo stg1 web1 web2 web3", ""},
		{"all", "10.0.0.5 db1 db2 fe80::1 lb1 solo stg1 web1 web2 web3", ""},
		{"*", "10.0.0.5 db1 db2 fe80::1 lb1 solo stg1 web1 web2 web3", ""},
		{"web", "web1 web2 web3", ""},
		{"web1", "web1", ""},
		{"web:db", "web1 web2 web3 db1 db2", ""},
		{"web,db", "web1 web2 web3 db1 db2", ""},
		{"web, db ", "web1 web2 web3 db1 db2", ""},
		{"web db", "web1 web2 web3 db1 db2", ""},
		{"prod", "lb1 db1 db2 web1 web2 web3", ""},
		{"prod:&staging", "web3", ""},
		{"&staging:prod", "web3", ""},
		{"prod:!web", "lb1 db1 db2", ""},
		{"!web:prod", "lb1 db1 db2", ""},
		{"!prod", "10.0.0.5 fe80::1 solo stg1", ""},
		{"&web", "web1 web2 web3", ""},
		{"ungrouped", "10.0.0.5 fe80::1 solo", ""},
		{"web*", "web1 web2 web3", ""},
		{"*1", "db1 lb1 stg1 web1 fe80::1", ""}, // Ansible's host insertion order
		{"db?", "db1 db2", ""},
		{"web[!1]*", "web3 web2", ""},
		{"~web[12]", "web1 web2", ""},
		{"~(db|lb)", "db1 db2 lb1", ""},
		{"~eb", "", "~eb"},
		{"web[0]", "web1", ""},
		{"web[-1]", "web3", ""},
		{"web[1:]", "web2 web3", ""},
		{"web[0:1]", "web1 web2", ""},
		{"web[1-2]", "web2 web3", ""},
		{"web[0:9]", "web1 web2 web3", ""},
		{"web[0:1]:db[1]", "web1 web2 db2", ""},
		{"prod[0]", "lb1", ""},
		{"prod[-1]", "web3", ""},
		{"*1[1:2]", "lb1 stg1", ""},
		{"web*[3]", "web3", ""}, // web* lists web's hosts, then the hosts named web*
		{"*[5]", "solo", ""},
		{"db[0:0]", "db1", ""}, // Ansible reads an end of 0 as a single index
		{"web[2:1]", "", "web[2:1]"},
		{"!web:!db", "10.0.0.5 fe80::1 lb1 solo stg1", ""},
		{"&prod:&web:!web1", "web2 web3", ""},
		{"web, db:prod", "web1 web2 web3", "db:prod"},
		{"10.0.0.5", "10.0.0.5", ""},
		{"fe80::1", "fe80::1", ""},
		{"nope:web", "web1 web2 web3", "nope"},
		{"web:!nope", "web1 web2 web3", "!nope"},
		{"WEB", "", "WEB"},
	}
	for _, tt := range tests {
		got, unmatched, err := Match(testInventory, tt.pattern)
		if err != nil {
			t.Errorf("Match(%q) error: %v", tt.pattern, err)
			continue
		}
		if g := strings.Join(got, " "); g != tt.want {
			t.Errorf("Match(%q) = %q, want %q", tt.pattern, g, tt.want)
		}
		if u := strings.Join(unmatched, " "); u != tt.unmatched {
			t.Errorf("Match(%q) unmatched = %q, want %q", tt.pattern, u, tt.unmatched)
		}
	}
}

func TestMatchErrors(t *testing.T) {
	for _, p := range []string{"web[3]", "web[-4]", "*[40]", "~(", "~(?=web)", "web[z-a]"} {
		if got, _, err := Match(testInventory, p); err == nil {
			t.Errorf("Match(%q) = %q, want an error", p, got)
		}
	}
}

func TestMatchEmptyInventory(t *testing.T) {
	got, unmatched, err := Match(Inventory{}, "")
	if err != nil || len(got) != 0 || len(unmatched) != 0 {
		t.Errorf("Match(empty, \"\") = %q, %q, %v", got, unmatched, err)
	}
	got, unmatched, err = Match(Inventory{}, "web")
	if err != nil || len(got) != 0 || !slices.Equal(unmatched, []string{"web"}) {
		t.Errorf("Match(empty, \"web\") = %q, %q, %v", got, unmatched, err)
	}
}

func TestSplitPattern(t *testing.T) {
	tests := map[string]string{
		"web:db":        "web|db",
		"web:&db:!x":    "web|&db|!x",
		"web[0:2]:db":   "web[0:2]|db",
		"fe80::1":       "fe80::1",
		"[fe80::1]:22":  "[fe80::1]:22",
		"web1:22":       "web1:22",
		"a,b:c":         "a|b:c",
		" a , , b ":     "a|b",
		"web[1:3]":      "web[1:3]",
		"~web\\d+:db":   "~web\\d+|db",
		"web\tdb":       "web|db",
		"":              "",
		"::":            "::",
		"web:":          "web",
		"web:22:db":     "web|22|db",
		"web.example:8": "web.example:8",
	}
	for in, want := range tests {
		if got := strings.Join(splitPattern(in), "|"); got != want {
			t.Errorf("splitPattern(%q) = %q, want %q", in, got, want)
		}
	}
}
