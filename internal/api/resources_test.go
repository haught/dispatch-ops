package api_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// signedIn starts a server and returns a client with a browser session.
func signedIn(t *testing.T) *client {
	t.Helper()
	srv, username, password := testServer(t)
	return login(t, srv.URL, username, password)
}

func login(t *testing.T, base, username, password string) *client {
	t.Helper()
	anon := &client{t: t, base: base}
	resp, body := anon.do("POST", "/api/v1/auth/login", map[string]string{"username": username, "password": password})
	expectStatus(t, resp, body, 200)
	for _, c := range resp.Cookies() {
		if c.Name == "dispatch_session" {
			return &client{t: t, base: base, cookie: c}
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// unique returns a name that doesn't clash with earlier test runs, which
// share the test database.
func unique(prefix string) string {
	return prefix + "-" + strings.ToLower(rand.Text()[:8])
}

// mustDo does a request and checks the status.
func (c *client) mustDo(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	resp, out := c.do(method, path, body)
	expectStatus(c.t, resp, out, want)
	return out
}

func (c *client) raw(method, path string) (int, string) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, nil)
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestInventoryHostsAndGroups(t *testing.T) {
	c := signedIn(t)

	inv := c.mustDo("POST", "/api/v1/inventories", map[string]any{
		"name": unique("inv"), "variables": map[string]any{"env": "prod"},
	}, 201)
	invID := inv["id"].(string)
	c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": inv["name"]}, 409)
	c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": "x", "bogus": 1}, 400)

	base := "/api/v1/inventories/" + invID
	group := func(name string, children ...string) string {
		body := map[string]any{"name": name}
		if children != nil {
			body["childIds"] = children
		}
		return c.mustDo("POST", base+"/groups", body, 201)["id"].(string)
	}
	web := group("web")
	db := group("db")
	servers := group("servers", web, db)

	// Reserved and invalid group names.
	c.mustDo("POST", base+"/groups", map[string]any{"name": "all"}, 400)
	c.mustDo("POST", base+"/groups", map[string]any{"name": "my group"}, 400)
	c.mustDo("POST", base+"/groups", map[string]any{"name": "web"}, 409)

	// A group can't become its own ancestor; the failed update changes nothing.
	c.mustDo("PATCH", "/api/v1/groups/"+web, map[string]any{"childIds": []string{servers}, "description": "x"}, 400)
	c.mustDo("PATCH", "/api/v1/groups/"+web, map[string]any{"childIds": []string{web}}, 400)
	if g := c.mustDo("GET", "/api/v1/groups/"+web, nil, 200); g["description"] != "" || len(g["childIds"].([]any)) != 0 {
		t.Errorf("failed group update was applied: %v", g)
	}

	host := func(body map[string]any) string {
		return c.mustDo("POST", base+"/hosts", body, 201)["id"].(string)
	}
	web1 := host(map[string]any{"name": "web1.example.com", "groupIds": []string{web}, "variables": map[string]any{"port": 80}})
	host(map[string]any{"name": "db1", "groupIds": []string{db}})
	host(map[string]any{"name": "lone1"})
	host(map[string]any{"name": "off1", "enabled": false})
	c.mustDo("POST", base+"/hosts", map[string]any{"name": "has space"}, 400)
	c.mustDo("POST", base+"/hosts", map[string]any{"name": "lone1"}, 409)

	// Groups from another inventory are rejected.
	other := c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)["id"].(string)
	foreign := c.mustDo("POST", "/api/v1/inventories/"+other+"/groups", map[string]any{"name": "web"}, 201)["id"].(string)
	c.mustDo("POST", base+"/hosts", map[string]any{"name": "x1", "groupIds": []string{foreign}}, 400)

	// Listing, filtering, paging.
	got := c.mustDo("GET", base+"/hosts?groupId="+web, nil, 200)
	if got["total"].(float64) != 1 || got["items"].([]any)[0].(map[string]any)["name"] != "web1.example.com" {
		t.Errorf("hosts in web = %v", got)
	}
	got = c.mustDo("GET", base+"/hosts?search=1&limit=2", nil, 200)
	if got["total"].(float64) != 4 || len(got["items"].([]any)) != 2 {
		t.Errorf("paged search = %v", got)
	}
	got = c.mustDo("GET", "/api/v1/inventories/"+invID, nil, 200)
	if got["hostCount"].(float64) != 4 || got["groupCount"].(float64) != 3 {
		t.Errorf("counts = %v", got)
	}

	// Export in Ansible's format.
	exp := c.mustDo("GET", base+"/export", nil, 200)
	all := exp["all"].(map[string]any)
	if top := strs(all["children"]); !slices.Equal(top, []string{"ungrouped", "servers"}) {
		t.Errorf("all.children = %v", top)
	}
	if all["vars"].(map[string]any)["env"] != "prod" {
		t.Errorf("all.vars = %v", all["vars"])
	}
	if ch := strs(exp["servers"].(map[string]any)["children"]); !slices.Equal(ch, []string{"db", "web"}) {
		t.Errorf("servers.children = %v", ch)
	}
	if h := strs(exp["web"].(map[string]any)["hosts"]); !slices.Equal(h, []string{"web1.example.com"}) {
		t.Errorf("web.hosts = %v", h)
	}
	if h := strs(exp["ungrouped"].(map[string]any)["hosts"]); !slices.Equal(h, []string{"lone1"}) {
		t.Errorf("ungrouped.hosts = %v (disabled hosts must be left out)", h)
	}
	hostvars := exp["_meta"].(map[string]any)["hostvars"].(map[string]any)
	if hostvars["web1.example.com"].(map[string]any)["port"].(float64) != 80 {
		t.Errorf("hostvars = %v", hostvars)
	}
	if _, ok := hostvars["off1"]; ok {
		t.Error("disabled host exported")
	}

	// PATCH changes only what is sent.
	h := c.mustDo("PATCH", "/api/v1/hosts/"+web1, map[string]any{"groupIds": []string{db}}, 200)
	if h["variables"].(map[string]any)["port"].(float64) != 80 || strs(h["groupIds"])[0] != db {
		t.Errorf("patched host = %v", h)
	}
	h = c.mustDo("PATCH", "/api/v1/hosts/"+web1, map[string]any{"name": "web1"}, 200)
	if len(h["groupIds"].([]any)) != 1 {
		t.Errorf("rename dropped group memberships: %v", h)
	}

	// Deleting a group keeps its hosts and children.
	c.mustDo("DELETE", "/api/v1/groups/"+servers, nil, 204)
	c.mustDo("GET", "/api/v1/groups/"+web, nil, 200)
	c.mustDo("GET", "/api/v1/hosts/"+web1, nil, 200)

	// Deleting the inventory removes everything in it.
	c.mustDo("DELETE", "/api/v1/inventories/"+invID, nil, 204)
	c.mustDo("GET", "/api/v1/inventories/"+invID, nil, 404)
	c.mustDo("GET", "/api/v1/hosts/"+web1, nil, 404)
	c.mustDo("GET", base+"/hosts", nil, 404)
}

func TestCredentialsAreWriteOnly(t *testing.T) {
	c := signedIn(t)

	kinds := c.mustDo("GET", "/api/v1/credential-kinds", nil, 200)
	if len(kinds["items"].([]any)) != 6 {
		t.Errorf("credential kinds = %v", kinds)
	}

	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nsupersecret\n-----END OPENSSH PRIVATE KEY-----"
	cred := c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("ssh"), "kind": "ssh", "inputs": map[string]string{"username": "deploy", "private_key": key},
	}, 201)
	id := cred["id"].(string)
	if cred["publicInputs"].(map[string]any)["username"] != "deploy" || !slices.Equal(strs(cred["secretFields"]), []string{"private_key"}) {
		t.Errorf("created credential = %v", cred)
	}
	for _, path := range []string{"/api/v1/credentials/" + id, "/api/v1/credentials?kind=ssh&limit=500"} {
		if status, body := c.raw("GET", path); status != 200 || strings.Contains(body, "supersecret") {
			t.Errorf("GET %s = %d, leaks secret: %v", path, status, strings.Contains(body, "supersecret"))
		}
	}

	// Validation against the kind.
	c.mustDo("POST", "/api/v1/credentials", map[string]any{"name": unique("ssh"), "kind": "ssh", "inputs": map[string]string{"username": "x"}}, 400)
	c.mustDo("POST", "/api/v1/credentials", map[string]any{"name": unique("ssh"), "kind": "ssh", "inputs": map[string]string{"private_key": "k", "nope": "x"}}, 400)
	c.mustDo("POST", "/api/v1/credentials", map[string]any{"name": unique("x"), "kind": "magic", "inputs": map[string]string{}}, 400)

	// Inputs are merged on update; an empty string removes one.
	cred = c.mustDo("PATCH", "/api/v1/credentials/"+id, map[string]any{"inputs": map[string]string{"passphrase": "pp"}}, 200)
	if !slices.Equal(strs(cred["secretFields"]), []string{"passphrase", "private_key"}) {
		t.Errorf("secretFields after adding = %v", cred["secretFields"])
	}
	cred = c.mustDo("PATCH", "/api/v1/credentials/"+id, map[string]any{"inputs": map[string]string{"passphrase": ""}}, 200)
	if !slices.Equal(strs(cred["secretFields"]), []string{"private_key"}) {
		t.Errorf("secretFields after removing = %v", cred["secretFields"])
	}
	c.mustDo("PATCH", "/api/v1/credentials/"+id, map[string]any{"inputs": map[string]string{"private_key": ""}}, 400)
	c.mustDo("PATCH", "/api/v1/credentials/"+id, map[string]any{"kind": "generic"}, 400)

	c.mustDo("DELETE", "/api/v1/credentials/"+id, nil, 204)
	c.mustDo("GET", "/api/v1/credentials/"+id, nil, 404)
}

func TestExecutionEnvironmentsAndTemplates(t *testing.T) {
	c := signedIn(t)

	ssh := c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("ssh"), "kind": "ssh", "inputs": map[string]string{"private_key": "k"},
	}, 201)["id"].(string)
	reg := c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("reg"), "kind": "registry", "inputs": map[string]string{"server": "ghcr.io", "username": "u", "password": "p"},
	}, 201)["id"].(string)

	// Only registry credentials can pull images.
	c.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "x", "registryCredentialId": ssh}, 400)
	c.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "has space"}, 400)
	ee := c.mustDo("POST", "/api/v1/execution-environments", map[string]any{
		"name": unique("ee"), "image": "ghcr.io/acme/ee-shell:1", "registryCredentialId": reg,
	}, 201)
	eeID := ee["id"].(string)
	if ee["pullPolicy"] != "IfNotPresent" || ee["registryCredentialId"] != reg {
		t.Errorf("created EE = %v", ee)
	}
	// An absent field is left alone; null clears it.
	ee = c.mustDo("PATCH", "/api/v1/execution-environments/"+eeID, map[string]any{"pullPolicy": "Always"}, 200)
	if ee["registryCredentialId"] != reg {
		t.Errorf("PATCH without registryCredentialId changed it: %v", ee)
	}
	ee = c.mustDo("PATCH", "/api/v1/execution-environments/"+eeID, map[string]any{"registryCredentialId": nil}, 200)
	if ee["registryCredentialId"] != nil || ee["pullPolicy"] != "Always" {
		t.Errorf("PATCH registryCredentialId null = %v", ee)
	}

	inv := c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)["id"].(string)

	// Only container jobs are supported so far.
	c.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "ansible", "executionEnvironmentId": eeID, "command": []string{"x"},
	}, 400)
	c.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": eeID, "command": []string{},
	}, 400)
	c.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": eeID, "command": []string{"x"},
		"inventoryId": "00000000-0000-0000-0000-000000000000",
	}, 400)
	jt := c.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": eeID,
		"command": []string{"echo", "hi"}, "inventoryId": inv, "credentialIds": []string{ssh},
		"extraVars": map[string]any{"a": 1, "b": 2}, "allowOverrides": []string{"extraVars", "limit"},
	}, 201)
	jtID := jt["id"].(string)
	if jt["timeoutSeconds"].(float64) != 3600 || !slices.Equal(strs(jt["credentialIds"]), []string{ssh}) {
		t.Errorf("created template = %v", jt)
	}

	// Things a template uses can't be deleted.
	c.mustDo("DELETE", "/api/v1/execution-environments/"+eeID, nil, 409)
	c.mustDo("DELETE", "/api/v1/credentials/"+ssh, nil, 409)
	c.mustDo("DELETE", "/api/v1/inventories/"+inv, nil, 409)

	// Launch: only allowed overrides; extra vars are merged.
	c.mustDo("POST", "/api/v1/job-templates/"+jtID+"/launch", map[string]any{"command": []string{"rm", "-rf", "/"}}, 400)
	job := c.mustDo("POST", "/api/v1/job-templates/"+jtID+"/launch", map[string]any{
		"extraVars": map[string]any{"b": 3}, "limit": "web*",
	}, 201)
	cfg := job["config"].(map[string]any)
	if job["status"] != "pending" || job["launchType"] != "manual" || cfg["limit"] != "web*" {
		t.Errorf("launched job = %v", job)
	}
	if v := cfg["extraVars"].(map[string]any); v["a"].(float64) != 1 || v["b"].(float64) != 3 {
		t.Errorf("merged extra vars = %v", v)
	}
	if cfg["executionEnvironment"].(map[string]any)["image"] != "ghcr.io/acme/ee-shell:1" {
		t.Errorf("job EE = %v", cfg["executionEnvironment"])
	}

	// The job keeps its configuration after the template changes, and
	// survives the template's deletion.
	c.mustDo("PATCH", "/api/v1/job-templates/"+jtID, map[string]any{"command": []string{"true"}, "inventoryId": nil}, 200)
	jobID := job["id"].(string)
	got := c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if strs(got["config"].(map[string]any)["command"])[0] != "echo" {
		t.Errorf("job config changed with the template: %v", got["config"])
	}
	list := c.mustDo("GET", "/api/v1/jobs?jobTemplateId="+jtID, nil, 200)
	if list["total"].(float64) != 1 {
		t.Errorf("jobs for template = %v", list)
	}
	c.mustDo("DELETE", "/api/v1/job-templates/"+jtID, nil, 204)
	got = c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if got["jobTemplateId"] != nil {
		t.Errorf("jobTemplateId after template deletion = %v", got["jobTemplateId"])
	}

	// A pending job is canceled at once; a finished one can't be canceled.
	got = c.mustDo("POST", "/api/v1/jobs/"+jobID+"/cancel", nil, 202)
	if got["status"] != "canceled" || got["finishedAt"] == nil {
		t.Errorf("canceled pending job = %v", got)
	}
	c.mustDo("POST", "/api/v1/jobs/"+jobID+"/cancel", nil, 409)
}

// jsonOf re-encodes v, for error messages.
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return fmt.Sprint(string(b))
}
