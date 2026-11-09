package api_test

import (
	"bufio"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"dispatch-ops/internal/api"
)

// L4: a password change ends every session and revokes every API token of
// that user, except the session an admin uses to change their own password.
func TestPasswordChangeRevokesSessionsAndTokens(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	bobToken := bob.mustDo("POST", "/api/v1/tokens", map[string]any{"name": "ci"}, 201)["token"].(string)
	robot := &client{t: t, base: srv.URL, bearer: bobToken}
	robot.mustDo("GET", "/api/v1/me", nil, 200)

	// Other changes leave sessions alone.
	admin.mustDo("PATCH", "/api/v1/users/"+bobID, map[string]any{"displayName": "Bob"}, 200)
	bob.mustDo("GET", "/api/v1/me", nil, 200)

	admin.mustDo("PATCH", "/api/v1/users/"+bobID, map[string]any{"password": "a-brand-new-password"}, 200)
	bob.mustDo("GET", "/api/v1/me", nil, 401)
	robot.mustDo("GET", "/api/v1/me", nil, 401)
	login(t, srv.URL, bob.username(t, admin, bobID), "a-brand-new-password").mustDo("GET", "/api/v1/me", nil, 200)

	// An admin changing their own password keeps the session they used, but
	// their other sessions end.
	other := login(t, srv.URL, username, password)
	me := admin.mustDo("GET", "/api/v1/me", nil, 200)["id"].(string)
	admin.mustDo("PATCH", "/api/v1/users/"+me, map[string]any{"password": "another-new-password"}, 200)
	admin.mustDo("GET", "/api/v1/me", nil, 200)
	other.mustDo("GET", "/api/v1/me", nil, 401)
}

// username looks up a user's name.
func (c *client) username(t *testing.T, admin *client, id string) string {
	t.Helper()
	return admin.mustDo("GET", "/api/v1/users/"+id, nil, 200)["username"].(string)
}

// L4: launching a job grants nothing once the launcher's roles are gone.
func TestLauncherLosesJobWithRoles(t *testing.T) {
	srv, username, password := testServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	jobPath, grantID := launchAs(t, admin, bob, bobID)

	bob.mustDo("GET", jobPath, nil, 200)
	admin.mustDo("DELETE", "/api/v1/role-grants/"+grantID, nil, 204)
	bob.mustDo("GET", jobPath, nil, 404)
	bob.mustDo("GET", jobPath+"/events", nil, 404)
	bob.mustDo("POST", jobPath+"/cancel", nil, 404)
	if jobs := bob.mustDo("GET", "/api/v1/jobs", nil, 200); jobs["total"].(float64) != 0 {
		t.Errorf("bob still lists his job: %v", jobs)
	}

	// With read back, he sees it again, and as its launcher may cancel it.
	grant(admin, map[string]any{"role": "read", "resourceType": "job_template", "resourceId": templateOf(t, admin, jobPath), "userId": bobID})
	bob.mustDo("GET", jobPath, nil, 200)
	bob.mustDo("POST", jobPath+"/cancel", nil, 202)
}

// L4: an open job stream ends when the caller loses access.
func TestStreamEndsWhenAccessEnds(t *testing.T) {
	srv, _, username, password := startServerWith(t, func(h *api.Handlers) { h.StreamRecheck = 50 * time.Millisecond })
	admin := login(t, srv.URL, username, password)

	cases := map[string]func(bob *client, bobID, grantID string){
		"role removed": func(_ *client, _, grantID string) {
			admin.mustDo("DELETE", "/api/v1/role-grants/"+grantID, nil, 204)
		},
		"logout": func(bob *client, _, _ string) {
			bob.mustDo("POST", "/api/v1/auth/logout", nil, 204)
		},
		"deactivated": func(_ *client, bobID, _ string) {
			admin.mustDo("PATCH", "/api/v1/users/"+bobID, map[string]any{"isActive": false}, 200)
		},
	}
	for name, revoke := range cases {
		t.Run(name, func(t *testing.T) {
			bob, bobID := newUser(t, srv, admin)
			jobPath, grantID := launchAs(t, admin, bob, bobID)
			events := openStream(t, bob, jobPath)

			// The job is pending, so the stream stays open until access ends.
			select {
			case got := <-events:
				t.Fatalf("stream ended before access did: %v", got)
			case <-time.After(300 * time.Millisecond):
			}
			revoke(bob, bobID, grantID)
			select {
			case got := <-events:
				if !slices.Contains(got, "revoked") {
					t.Errorf("stream ended with %v, want a revoked event", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stream still open after access ended")
			}
		})
	}
}

// launchAs gives bob execute on a new template and has him launch it. It
// returns the job's path and the grant's ID.
func launchAs(t *testing.T, admin, bob *client, bobID string) (jobPath, grantID string) {
	t.Helper()
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"], "command": []string{"true"},
	}, 201)
	grantID = grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})
	job := bob.mustDo("POST", "/api/v1/job-templates/"+jt["id"].(string)+"/launch", nil, 201)
	return "/api/v1/jobs/" + job["id"].(string), grantID
}

func templateOf(t *testing.T, admin *client, jobPath string) string {
	t.Helper()
	return admin.mustDo("GET", jobPath, nil, 200)["jobTemplateId"].(string)
}

// openStream follows a job's live stream and sends the names of the events
// it got once the server closes it.
func openStream(t *testing.T, c *client, jobPath string) <-chan []string {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+jobPath+"/stream", nil)
	req.AddCookie(c.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	out := make(chan []string, 1)
	go func() {
		defer resp.Body.Close()
		var names []string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				names = append(names, name)
			}
		}
		out <- names
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return out
}
