package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/api"
	"dispatch-ops/internal/oidc"
	"dispatch-ops/internal/store"
)

// L3: a relaunch launches the template as it is now, with the job's launch
// overrides, through the same checks as a launch.
func TestRelaunchUsesCurrentTemplate(t *testing.T) {
	srv, st, username, password := startServer(t)
	admin := login(t, srv.URL, username, password)
	bob, bobID := newUser(t, srv, admin)
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)
	jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
		"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"],
		"command": []string{"echo", "old"}, "extraVars": map[string]any{"tmpl": 1}, "allowOverrides": []string{"extraVars"},
	}, 201)
	jtPath := "/api/v1/job-templates/" + jt["id"].(string)
	grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})

	job := bob.mustDo("POST", jtPath+"/launch", map[string]any{"extraVars": map[string]any{"mine": 2}}, 201)
	relaunch := "/api/v1/jobs/" + job["id"].(string) + "/relaunch"

	// A template admin changes the command: the relaunch runs the new one,
	// with the job's overrides.
	admin.mustDo("PATCH", jtPath, map[string]any{"command": []string{"echo", "new"}}, 200)
	again := bob.mustDo("POST", relaunch, nil, 201)
	cfg := again["config"].(map[string]any)
	if got := cfg["command"]; !slices.Equal(toStrings(got), []string{"echo", "new"}) {
		t.Errorf("relaunched command = %v, want the template's current one", got)
	}
	if vars := cfg["extraVars"].(map[string]any); vars["mine"] != 2.0 || vars["tmpl"] != 1.0 {
		t.Errorf("relaunched extraVars = %v, want the template's plus the job's overrides", vars)
	}
	// So does relaunching the relaunch: its overrides were kept too.
	bob.mustDo("POST", "/api/v1/jobs/"+again["id"].(string)+"/relaunch", nil, 201)

	// The template stops allowing extraVars overrides: the job can't be
	// relaunched as it was launched.
	admin.mustDo("PATCH", jtPath, map[string]any{"allowOverrides": []string{}}, 200)
	bob.mustDo("POST", relaunch, nil, 400)

	// A job from before overrides were kept, whose config has extra vars a
	// launch would now refuse, is relaunched from the template alone.
	tmplID := uuid.MustParse(jt["id"].(string))
	old, err := st.CreateJob(context.Background(), store.NewJob{
		TemplateID: &tmplID, Name: "old", LaunchType: "manual", Config: store.JobConfig{
			JobType: "container", Command: []string{"echo", "old"},
			ExtraVars: map[string]any{"x": "{{ lookup('file', '/runner/secrets/credentials.json') }}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh := admin.mustDo("POST", "/api/v1/jobs/"+old.ID.String()+"/relaunch", nil, 201)["config"].(map[string]any)
	if vars := fresh["extraVars"].(map[string]any); vars["x"] != nil {
		t.Errorf("relaunch of an old job kept its extra vars: %v", vars)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// L2: a schedule runs with its owner's current access, and is disabled once
// they lose it.
func TestScheduleOwnerAccessIsChecked(t *testing.T) {
	srv, st, username, password := startServer(t)
	admin := login(t, srv.URL, username, password)
	ctx := context.Background()
	ee := admin.mustDo("POST", "/api/v1/execution-environments", map[string]any{"name": unique("ee"), "image": "local/test"}, 201)

	// newSchedule has bob create a due schedule of a template he may launch.
	newSchedule := func(t *testing.T) (bob *client, bobID, grantID, scID string) {
		t.Helper()
		bob, bobID = newUser(t, srv, admin)
		jt := admin.mustDo("POST", "/api/v1/job-templates", map[string]any{
			"name": unique("jt"), "jobType": "container", "executionEnvironmentId": ee["id"], "command": []string{"true"},
		}, 201)
		grantID = grant(admin, map[string]any{"role": "execute", "resourceType": "job_template", "resourceId": jt["id"], "userId": bobID})
		sc := bob.mustDo("POST", "/api/v1/schedules", map[string]any{"name": "s", "jobTemplateId": jt["id"], "cron": "@hourly"}, 201)
		scID = sc["id"].(string)
		makeDue(t, st, scID)
		return bob, bobID, grantID, scID
	}
	// run runs the due schedules and returns this one's run.
	run := func(t *testing.T, scID string) store.ScheduleRun {
		t.Helper()
		runs, err := st.RunDueSchedules(ctx, 10000)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Schedule.ID.String() == scID {
				return r
			}
		}
		t.Fatalf("schedule %s didn't run", scID)
		return store.ScheduleRun{}
	}

	t.Run("owner keeps access", func(t *testing.T) {
		_, _, _, scID := newSchedule(t)
		if r := run(t, scID); r.JobID == nil {
			t.Errorf("run = %v, want a job", r.Err)
		}
	})

	revocations := map[string]func(bobID, grantID string){
		"role removed": func(_, grantID string) { admin.mustDo("DELETE", "/api/v1/role-grants/"+grantID, nil, 204) },
		"deactivated": func(bobID, _ string) {
			admin.mustDo("PATCH", "/api/v1/users/"+bobID, map[string]any{"isActive": false}, 200)
		},
		"deleted": func(bobID, _ string) { admin.mustDo("DELETE", "/api/v1/users/"+bobID, nil, 204) },
	}
	for name, revoke := range revocations {
		t.Run(name, func(t *testing.T) {
			_, bobID, grantID, scID := newSchedule(t)
			revoke(bobID, grantID)
			r := run(t, scID)
			if r.JobID != nil || r.Err == nil || !strings.Contains(r.Err.Error(), "schedule disabled") {
				t.Fatalf("run = job %v, err %v; want refused and disabled", r.JobID, r.Err)
			}
			sc := admin.mustDo("GET", "/api/v1/schedules/"+scID, nil, 200)
			if sc["enabled"] != false || !strings.Contains(sc["lastError"].(string), "set it up") {
				t.Errorf("schedule = %v", sc)
			}
			// Turning itself off is in the audit log.
			log := admin.mustDo("GET", "/api/v1/audit-log?resourceType=schedule&action=update&resourceId="+scID, nil, 200)
			items := log["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("schedule audit entries = %v", items)
			}
			e := items[0].(map[string]any)
			after, _ := e["after"].(map[string]any)
			if e["actor"] != nil || e["authMethod"] != "system" || after["enabled"] != false ||
				!strings.Contains(fmt.Sprint(after["lastError"]), "set it up") {
				t.Errorf("audit entry = %v", e)
			}

			// An admin turning it back on becomes its owner, and it runs.
			on := admin.mustDo("PATCH", "/api/v1/schedules/"+scID, map[string]any{"enabled": true}, 200)
			if on["createdBy"] == nil || on["createdBy"].(map[string]any)["id"] == bobID {
				t.Errorf("re-enabled schedule's owner = %v", on["createdBy"])
			}
			makeDue(t, st, scID)
			if r := run(t, scID); r.JobID == nil {
				t.Errorf("re-enabled run = %v, want a job", r.Err)
			}
		})
	}
}

func makeDue(t *testing.T, st *store.Store, scID string) {
	t.Helper()
	past := time.Now().Add(-time.Minute)
	if _, err := st.UpdateSchedule(context.Background(), uuid.MustParse(scID), func(_ store.Schedule, f *store.ScheduleFields) error {
		f.NextRunAt = &past
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// L6: the OIDC state cookie can't be forged.
func TestOIDCStateCookieIsSealed(t *testing.T) {
	idp := newFakeIDP(t)
	srv, _, _, _ := startServerWith(t, func(h *api.Handlers) {
		h.OIDC = oidc.New(oidc.Config{
			Issuer: idp.srv.URL, ClientID: "dispatch", ClientSecret: "s", RedirectURL: "http://dispatch.test/cb",
			Scopes: []string{"profile"}, UsernameClaim: "preferred_username",
		})
	})
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// The old format, unsigned base64 JSON, with a state of the attacker's
	// choosing, is refused.
	forged, _ := json.Marshal(map[string]any{"state": "s", "nonce": "n", "verifier": "v", "next": "/"})
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/auth/oidc/callback?code=c&state=s", nil)
	req.AddCookie(&http.Cookie{Name: "dispatch_oidc", Value: base64.RawURLEncoding.EncodeToString(forged)})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Path != "/login" || loc.Query().Get("error") == "" {
		t.Errorf("forged state: %d to %s, want the login page with an error", resp.StatusCode, loc)
	}
	for _, c := range resp.Cookies() {
		if strings.Contains(c.Name, "session") && c.Value != "" {
			t.Errorf("forged state started a session")
		}
	}

	// A real sign-in's cookie is opaque: it doesn't reveal the state.
	resp, err = noRedirect.Get(srv.URL + "/api/v1/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, _ := url.Parse(resp.Header.Get("Location"))
	for _, c := range resp.Cookies() {
		if c.Name == "dispatch_oidc" {
			if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil && strings.Contains(string(raw), authURL.Query().Get("state")) {
				t.Errorf("state cookie isn't sealed: %s", raw)
			}
			if c.Path != "/" {
				t.Errorf("state cookie path = %q, want /", c.Path)
			}
		}
	}
}

// L6: with secure cookies, the session and OIDC state cookies are
// __Host- cookies, and a plain-named session cookie isn't accepted.
func TestSecureCookieNames(t *testing.T) {
	idp := newFakeIDP(t)
	srv, _, username, password := startServerWith(t, func(h *api.Handlers) {
		h.SecureCookies = true
		h.OIDC = oidc.New(oidc.Config{
			Issuer: idp.srv.URL, ClientID: "dispatch", ClientSecret: "s", RedirectURL: "http://dispatch.test/cb",
			Scopes: []string{"profile"}, UsernameClaim: "preferred_username",
		})
	})
	anon := &client{t: t, base: srv.URL}
	resp, body := anon.do("POST", "/api/v1/auth/login", map[string]string{"username": username, "password": password})
	expectStatus(t, resp, body, 200)
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == api.SecureSessionCookieName {
			session = c
		}
	}
	if session == nil || !session.Secure || session.Path != "/" {
		t.Fatalf("login set %v, want a Secure %s cookie with Path=/", resp.Cookies(), api.SecureSessionCookieName)
	}
	(&client{t: t, base: srv.URL, cookie: session}).mustDo("GET", "/api/v1/me", nil, 200)
	planted := &http.Cookie{Name: api.SessionCookieName, Value: session.Value}
	(&client{t: t, base: srv.URL, cookie: planted}).mustDo("GET", "/api/v1/me", nil, 401)

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + "/api/v1/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !slices.ContainsFunc(resp.Cookies(), func(c *http.Cookie) bool {
		return c.Name == "__Host-dispatch_oidc" && c.Secure && c.Path == "/"
	}) {
		t.Errorf("OIDC login set %v, want a Secure __Host-dispatch_oidc cookie with Path=/", resp.Cookies())
	}
}
