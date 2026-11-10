package api_test

import (
	"net/url"
	"slices"
	"testing"
)

func TestMatchHosts(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)

	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	base := "/api/v1/inventories/" + inv["id"].(string)
	group := func(name string, children ...string) string {
		body := map[string]any{"name": name}
		if children != nil {
			body["childIds"] = children
		}
		return admin.mustDo("POST", base+"/groups", body, 201)["id"].(string)
	}
	web := group("web")
	db := group("db")
	group("prod", web, db)
	host := func(name string, enabled bool, groups ...string) {
		admin.mustDo("POST", base+"/hosts", map[string]any{"name": name, "enabled": enabled, "groupIds": append([]string{}, groups...)}, 201)
	}
	host("web2", true, web)
	host("web1", true, web)
	host("web3", false, web)
	host("db1", true, db)
	host("lone1", true)

	match := func(pattern, extra string) map[string]any {
		t.Helper()
		return admin.mustDo("GET", base+"/host-matches?pattern="+url.QueryEscape(pattern)+extra, nil, 200)
	}
	tests := []struct {
		pattern   string
		want      []string
		unmatched []string
	}{
		{"", []string{"db1", "lone1", "web1", "web2"}, nil}, // disabled web3 left out
		{"prod", []string{"db1", "web1", "web2"}, nil},
		{"prod:!db", []string{"web1", "web2"}, nil},
		{"web[0]", []string{"web1"}, nil},
		{"web*:nope", []string{"web1", "web2"}, []string{"nope"}},
		{"web3", nil, []string{"web3"}},
	}
	for _, tt := range tests {
		got := match(tt.pattern, "")
		if n := names(got); !slices.Equal(n, tt.want) || got["total"].(float64) != float64(len(tt.want)) {
			t.Errorf("match %q = %v, want %v", tt.pattern, got, tt.want)
		}
		var unmatched []string
		for _, u := range got["unmatched"].([]any) {
			unmatched = append(unmatched, u.(string))
		}
		if !slices.Equal(unmatched, tt.unmatched) {
			t.Errorf("match %q unmatched = %v, want %v", tt.pattern, unmatched, tt.unmatched)
		}
	}

	// Paging and search apply after matching; groupIds are direct groups.
	got := match("prod", "&limit=1&offset=1")
	if n := names(got); !slices.Equal(n, []string{"web1"}) || got["total"].(float64) != 3 {
		t.Errorf("paged = %v", got)
	}
	got = match("all", "&search=WEB")
	if n := names(got); !slices.Equal(n, []string{"web1", "web2"}) {
		t.Errorf("search = %v", got)
	}
	if ids := got["items"].([]any)[0].(map[string]any)["groupIds"].([]any); len(ids) != 1 || ids[0] != web {
		t.Errorf("groupIds = %v, want [%s]", ids, web)
	}

	// Patterns Ansible rejects, and file references.
	for _, p := range []string{"~(", "web[5]", "@/etc/passwd"} {
		admin.mustDo("GET", base+"/host-matches?pattern="+url.QueryEscape(p), nil, 400)
	}

	// It needs read on the inventory, like listing its hosts.
	bob, bobID := newUser(t, srv, admin)
	bob.mustDo("GET", base+"/host-matches", nil, 404)
	grant(admin, map[string]any{"role": "read", "resourceType": "inventory", "resourceId": inv["id"], "userId": bobID})
	bob.mustDo("GET", base+"/host-matches?pattern=web", nil, 200)
	admin.mustDo("GET", "/api/v1/inventories/00000000-0000-0000-0000-000000000000/host-matches", nil, 404)
}
