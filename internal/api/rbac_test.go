package api_test

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"
)

// newUser has admin create a local user and returns a signed-in client for
// them and their ID.
func newUser(t *testing.T, srv *httptest.Server, admin *client) (*client, string) {
	t.Helper()
	name := unique("user")
	u := admin.mustDo("POST", "/api/v1/users", map[string]any{"username": name, "password": "another-long-password"}, 201)
	return login(t, srv.URL, name, "another-long-password"), u["id"].(string)
}

func grant(c *client, body map[string]any) string {
	c.t.Helper()
	return c.mustDo("POST", "/api/v1/role-grants", body, 201)["id"].(string)
}

func names(list map[string]any) []string {
	var out []string
	for _, it := range list["items"].([]any) {
		out = append(out, it.(map[string]any)["name"].(string))
	}
	return out
}

func TestResourceRoles(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)

	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	invPath := "/api/v1/inventories/" + inv["id"].(string)

	// Without a role, bob can't see the inventory, and can't tell it exists.
	if got := names(bob.mustDo("GET", "/api/v1/inventories", nil, 200)); slices.Contains(got, inv["name"].(string)) {
		t.Errorf("bob lists inventory without a role: %v", got)
	}
	bob.mustDo("GET", invPath, nil, 404)
	bob.mustDo("PATCH", invPath, map[string]any{"description": "x"}, 404)
	bob.mustDo("GET", invPath+"/hosts", nil, 404)
	bob.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 403)

	// Execution environments are for everyone to read and system admins to manage.
	bob.mustDo("GET", "/api/v1/execution-environments", nil, 200)
	bob.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "x"}, 403)

	// read: see it, not change it.
	readGrant := grant(admin, map[string]any{"role": "read", "resourceType": "inventory", "resourceId": inv["id"], "userId": bobID})
	bob.mustDo("GET", invPath, nil, 200)
	if got := names(bob.mustDo("GET", "/api/v1/inventories", nil, 200)); !slices.Equal(got, []string{inv["name"].(string)}) {
		t.Errorf("bob lists %v, want only %s", got, inv["name"])
	}
	bob.mustDo("PATCH", invPath, map[string]any{"description": "x"}, 403)
	bob.mustDo("POST", invPath+"/hosts", map[string]any{"name": "web1"}, 403)
	// Only a resource's admins may see who holds roles on it.
	bob.mustDo("GET", "/api/v1/role-grants?resourceType=inventory&resourceId="+inv["id"].(string), nil, 403)
	bob.mustDo("GET", "/api/v1/role-grants", nil, 403)
	mine := bob.mustDo("GET", "/api/v1/role-grants?userId="+bobID, nil, 200)
	if mine["total"].(float64) != 1 {
		t.Errorf("bob's own grants = %v", mine)
	}

	// admin: change it, and manage roles on it, but not globally.
	grant(admin, map[string]any{"role": "admin", "resourceType": "inventory", "resourceId": inv["id"], "userId": bobID})
	bob.mustDo("PATCH", invPath, map[string]any{"description": "bob's"}, 200)
	host := bob.mustDo("POST", invPath+"/hosts", map[string]any{"name": "web1"}, 201)
	bob.mustDo("GET", "/api/v1/hosts/"+host["id"].(string), nil, 200)
	bob.mustDo("GET", "/api/v1/role-grants?resourceType=inventory&resourceId="+inv["id"].(string), nil, 200)
	bob.mustDo("DELETE", "/api/v1/role-grants/"+readGrant, nil, 204)
	bob.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "system_admin", "userId": bobID}, 403)
	bob.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "admin", "resourceType": "inventory", "userId": bobID}, 403)

	// Invalid grants.
	admin.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "execute", "resourceType": "inventory", "resourceId": inv["id"], "userId": bobID}, 400)
	admin.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "admin", "userId": bobID}, 400)
	admin.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "read", "resourceType": "inventory", "resourceId": inv["id"]}, 400)
	admin.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "admin", "resourceType": "inventory", "resourceId": inv["id"], "userId": bobID}, 409)
	admin.mustDo("POST", "/api/v1/role-grants", map[string]any{"role": "read", "resourceType": "project", "resourceId": inv["id"], "userId": bobID}, 400)

	// Grants go with the resource.
	admin.mustDo("DELETE", invPath, nil, 204)
	if left := admin.mustDo("GET", "/api/v1/role-grants?resourceId="+inv["id"].(string), nil, 200); left["total"].(float64) != 0 {
		t.Errorf("grants outlived their inventory: %v", left)
	}
}

func TestTemplateRolesTeamsAndJobs(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	carol, carolID := newUser(t, srv, admin)

	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	cred := admin.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("cred"), "kind": "generic", "inputs": map[string]string{"token": "s3cret"},
	}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"true"}, "inventoryId": inv["id"], "allowOverrides": []string{"inventory"},
	}, 201)
	jtPath := "/api/v1/job-templates/" + jt["id"].(string)

	// A team's roles apply to its members.
	team := admin.mustDo("POST", "/api/v1/teams", map[string]any{"name": unique("ops")}, 201)
	teamID := team["id"].(string)
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "teamId": teamID})
	bob.mustDo("GET", jtPath, nil, 404)
	bob.mustDo("PUT", "/api/v1/teams/"+teamID+"/members/"+bobID, nil, 403)
	admin.mustDo("PUT", "/api/v1/teams/"+teamID+"/members/"+bobID, nil, 204)
	bob.mustDo("GET", jtPath, nil, 200)
	bob.mustDo("PATCH", jtPath, map[string]any{"description": "x"}, 403)

	// execute: launch, but an inventory given at launch needs use.
	other := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	bob.mustDo("POST", jtPath+"/launch", map[string]any{"inventoryId": other["id"]}, 400)
	job := bob.mustDo("POST", jtPath+"/launch", nil, 201)
	jobPath := "/api/v1/jobs/" + job["id"].(string)
	bob.mustDo("GET", jobPath, nil, 200)
	bob.mustDo("GET", jobPath+"/events", nil, 200)

	// Others can't see the job; having read on the template lets them see
	// it but not cancel it.
	carol.mustDo("GET", jobPath, nil, 404)
	carol.mustDo("POST", jobPath+"/cancel", nil, 404)
	if jobs := carol.mustDo("GET", "/api/v1/jobs", nil, 200); jobs["total"].(float64) != 0 {
		t.Errorf("carol lists jobs: %v", jobs)
	}
	grant(admin, map[string]any{"role": "read", "resourceType": "job_template", "resourceId": jt["id"], "userId": carolID})
	carol.mustDo("GET", jobPath, nil, 200)
	carol.mustDo("POST", jobPath+"/cancel", nil, 403)
	bob.mustDo("POST", jobPath+"/cancel", nil, 202)

	// Creating templates needs admin on all of them, and use on what the
	// template references.
	newTemplate := map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"], "command": []string{"true"},
		"inventoryId": inv["id"], "credentialIds": []any{cred["id"]},
	}
	carol.mustDo("POST", "/api/v1/job-templates", newTemplate, 403)
	grant(admin, map[string]any{"role": "admin", "resourceType": "job_template", "userId": carolID})
	carol.mustDo("POST", "/api/v1/job-templates", newTemplate, 400)
	grant(admin, map[string]any{"role": "use", "resourceType": "inventory", "resourceId": inv["id"], "userId": carolID})
	grant(admin, map[string]any{"role": "read", "resourceType": "credential", "resourceId": cred["id"], "userId": carolID})
	resp, body := carol.do("POST", "/api/v1/job-templates", newTemplate)
	expectStatus(t, resp, body, 403) // read on the credential, but not use
	grant(admin, map[string]any{"role": "use", "resourceType": "credential", "resourceId": cred["id"], "userId": carolID})
	mineJT := carol.mustDo("POST", "/api/v1/job-templates", newTemplate, 201)
	// The creator gets admin on what they create.
	grants := carol.mustDo("GET", "/api/v1/role-grants?resourceType=job_template&resourceId="+mineJT["id"].(string), nil, 200)
	if grants["total"].(float64) != 1 {
		t.Errorf("creator grants = %v", grants)
	}

	// Removing bob from the team takes the team's roles away; his own job
	// stays visible to him.
	admin.mustDo("DELETE", "/api/v1/teams/"+teamID+"/members/"+bobID, nil, 204)
	bob.mustDo("GET", jtPath, nil, 404)
	bob.mustDo("GET", jobPath, nil, 200)
}

func TestScopedTokensUsersAndAudit(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)

	// A read-only token can only GET.
	tok := bob.mustDo("POST", "/api/v1/tokens", map[string]any{"name": "dashboard", "scope": "read"}, 201)
	if tok["scope"] != "read" {
		t.Errorf("token scope = %v", tok["scope"])
	}
	ro := &client{t: t, base: srv.URL, bearer: tok["token"].(string)}
	ro.mustDo("GET", "/api/v1/jobs", nil, 200)
	ro.mustDo("DELETE", "/api/v1/tokens/"+tok["id"].(string), nil, 403)

	// Admins can give a service account a token, and see and revoke its tokens.
	svc := admin.mustDo("POST", "/api/v1/users/"+bobID+"/tokens", map[string]any{"name": "ci"}, 201)
	robot := &client{t: t, base: srv.URL, bearer: svc["token"].(string)}
	if me := robot.mustDo("GET", "/api/v1/me", nil, 200); me["id"] != bobID {
		t.Errorf("service token authenticates as %v", me["id"])
	}
	if list := admin.mustDo("GET", "/api/v1/users/"+bobID+"/tokens", nil, 200); len(list["items"].([]any)) != 2 {
		t.Errorf("bob's tokens = %v", list)
	}
	bob.mustDo("GET", "/api/v1/users/"+bobID+"/tokens", nil, 403)
	admin.mustDo("DELETE", "/api/v1/users/"+bobID+"/tokens/"+svc["id"].(string), nil, 204)
	robot.mustDo("GET", "/api/v1/me", nil, 401)

	// Only system admins manage users; nobody can lock themselves out.
	bob.mustDo("PATCH", "/api/v1/users/"+bobID, map[string]any{"displayName": "Bob"}, 403)
	me := admin.mustDo("GET", "/api/v1/me", nil, 200)
	admin.mustDo("PATCH", "/api/v1/users/"+me["id"].(string), map[string]any{"isActive": false}, 400)
	admin.mustDo("DELETE", "/api/v1/users/"+me["id"].(string), nil, 400)
	if !me["isAdmin"].(bool) || me["isAuditor"].(bool) || !me["isActive"].(bool) {
		t.Errorf("admin's flags = %v", me)
	}

	// Auditors read everything, change nothing.
	inv := admin.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv")}, 201)
	bob.mustDo("GET", "/api/v1/audit-log", nil, 403)
	auditor := grant(admin, map[string]any{"role": "auditor", "userId": bobID})
	bob.mustDo("GET", "/api/v1/inventories/"+inv["id"].(string), nil, 200)
	bob.mustDo("PATCH", "/api/v1/inventories/"+inv["id"].(string), map[string]any{"description": "x"}, 403)
	bob.mustDo("GET", "/api/v1/role-grants", nil, 200)
	bob.mustDo("GET", "/api/v1/audit-log", nil, 200)
	admin.mustDo("DELETE", "/api/v1/role-grants/"+auditor, nil, 204)
	bob.mustDo("GET", "/api/v1/audit-log", nil, 403)

	// The audit log has the changes, with who made them and what changed.
	admin.mustDo("PATCH", "/api/v1/inventories/"+inv["id"].(string), map[string]any{"description": "now described"}, 200)
	log := admin.mustDo("GET", "/api/v1/audit-log?resourceType=inventory&resourceId="+inv["id"].(string), nil, 200)
	var actions []string
	for _, it := range log["items"].([]any) {
		e := it.(map[string]any)
		actions = append(actions, e["action"].(string))
		if e["actorName"] != username {
			t.Errorf("audit actor = %v, want %s", e["actorName"], username)
		}
		if e["action"] == "update" {
			before, after := e["before"].(map[string]any), e["after"].(map[string]any)
			if fmt.Sprint(before) != "map[description:]" || fmt.Sprint(after) != "map[description:now described]" {
				t.Errorf("update recorded before %v after %v", before, after)
			}
		}
	}
	if !slices.Equal(actions, []string{"update", "create"}) {
		t.Errorf("inventory audit actions = %v", actions)
	}
	userLog := admin.mustDo("GET", "/api/v1/audit-log?resourceType=user&resourceId="+bobID, nil, 200)
	actions = nil
	for _, it := range userLog["items"].([]any) {
		actions = append(actions, it.(map[string]any)["action"].(string))
	}
	if !slices.Equal(actions, []string{"revoke", "grant", "login", "create"}) {
		t.Errorf("user audit actions = %v", actions)
	}

	// Deactivating a user ends their session.
	admin.mustDo("PATCH", "/api/v1/users/"+bobID, map[string]any{"isActive": false}, 200)
	bob.mustDo("GET", "/api/v1/me", nil, 401)
	resp, body := (&client{t: t, base: srv.URL}).do("POST", "/api/v1/auth/login",
		map[string]string{"username": userLog["items"].([]any)[0].(map[string]any)["resourceName"].(string), "password": "another-long-password"})
	expectStatus(t, resp, body, 401)
}
