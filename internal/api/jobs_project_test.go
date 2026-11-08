package api_test

import (
	"bytes"
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dispatch-ops/internal/runner"
)

// launchTemplate creates a template (with the env's EE) and launches it.
func (e *jobEnv) launchTemplate(tmpl map[string]any) string {
	e.t.Helper()
	tmpl["name"] = unique("jt")
	tmpl["executionEnvironmentId"] = e.ee
	jt := e.c.mustDo("POST", "/api/v1/job-templates", tmpl, 201)
	job := e.c.mustDo("POST", "/api/v1/job-templates/"+jt["id"].(string)+"/launch", nil, 201)
	return job["id"].(string)
}

// project creates a project for a repository URL.
func (e *jobEnv) project(url, ref string) string {
	e.t.Helper()
	return e.c.mustDo("POST", "/api/v1/projects", map[string]any{"name": unique("p"), "scmUrl": url, "scmRef": ref}, 201)["id"].(string)
}

// run runs the job's runner to the end and returns the job.
func (e *jobEnv) run(jobID string) map[string]any {
	e.t.Helper()
	var out bytes.Buffer
	if err := runner.Run(context.Background(), e.runnerConfig(jobID, e.claim(jobID), &out)); err != nil {
		e.t.Fatalf("runner: %v\n%s", err, out.String())
	}
	return e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
}

func TestScriptJob(t *testing.T) {
	e := newJobEnv(t)
	url, commit := gitRepo(t, map[string]string{
		"bin/hello.sh": "#!/bin/sh\necho \"args: $*\"\necho \"in: $(basename \"$PWD\")\"\ntest -f README && echo has-readme\ntr -d ' \\n' < \"$DISPATCH_VARS\"; echo\n",
		"plain.sh":     "echo \"plain $1\"\n", // no #! line and not executable: run with sh
		"README":       "hi\n",
	})
	proj := e.project(url, "main")

	jobID := e.launchTemplate(map[string]any{
		"jobType": "script", "projectId": proj, "entrypoint": "bin/hello.sh",
		"arguments": []string{"one", "two words"}, "extraVars": map[string]any{"n": 1},
	})
	job := e.run(jobID)
	if job["status"] != "successful" || job["scmRevision"] != commit {
		t.Errorf("job = %s", jsonOf(job))
	}
	if got, want := e.stdout(jobID), "args: one two words\nin: project\nhas-readme\n{\"n\":1}"; got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}

	jobID = e.launchTemplate(map[string]any{
		"jobType": "script", "projectId": proj, "entrypoint": "plain.sh", "arguments": []string{"x"},
	})
	if job := e.run(jobID); job["status"] != "successful" || e.stdout(jobID) != "plain x" {
		t.Errorf("job = %s, stdout %q", jsonOf(job), e.stdout(jobID))
	}

	// A commit ID works as the ref too.
	jobID = e.launchTemplate(map[string]any{
		"jobType": "script", "projectId": e.project(url, commit), "entrypoint": "plain.sh",
	})
	if job := e.run(jobID); job["status"] != "successful" || job["scmRevision"] != commit {
		t.Errorf("job at commit = %s", jsonOf(job))
	}

	// A missing script and a missing ref are errors, explained.
	jobID = e.launchTemplate(map[string]any{"jobType": "script", "projectId": proj, "entrypoint": "nope.sh"})
	if job := e.run(jobID); job["status"] != "error" || !strings.Contains(job["failureReason"].(string), "nope.sh is not in the project") {
		t.Errorf("missing script: %s", jsonOf(job))
	}
	jobID = e.launchTemplate(map[string]any{"jobType": "script", "projectId": e.project(url, "no-such-branch"), "entrypoint": "plain.sh"})
	if job := e.run(jobID); job["status"] != "error" ||
		!strings.Contains(job["failureReason"].(string), "Could not check out the project") ||
		!strings.Contains(job["failureReason"].(string), "no-such-branch") {
		t.Errorf("missing ref: %s", jsonOf(job))
	}
}

func TestOldRunnerCantRunProjectJobs(t *testing.T) {
	e := newJobEnv(t)
	url, _ := gitRepo(t, map[string]string{"x.sh": "true\n"})
	jobID := e.launchTemplate(map[string]any{"jobType": "script", "projectId": e.project(url, "main"), "entrypoint": "x.sh"})
	r := &client{t: t, base: e.c.base, bearer: e.claim(jobID)}
	resp, body := r.do("GET", "/runner/v1/jobs/"+jobID+"/spec?protocol=1&version=0.1.0", nil)
	expectStatus(t, resp, body, 400)
	job := e.c.mustDo("GET", "/api/v1/jobs/"+jobID, nil, 200)
	if job["status"] != "error" || !strings.Contains(job["failureReason"].(string), "too old to run script jobs") {
		t.Errorf("job = %s", jsonOf(job))
	}
}

func TestJobHostSummaries(t *testing.T) {
	e := newJobEnv(t)
	jobID := e.launch(map[string]any{"command": []string{"true"}})
	r := &client{t: t, base: e.c.base, bearer: e.claim(jobID)}
	r.mustDo("GET", "/runner/v1/jobs/"+jobID+"/spec?protocol=2&version=x", nil, 200)

	ev := func(seq int, typ, host string, data map[string]any) map[string]any {
		m := map[string]any{"seq": seq, "type": typ, "stdout": ""}
		if host != "" {
			m["hostName"] = host
		}
		if data != nil {
			m["data"] = data
		}
		return m
	}
	r.mustDo("POST", "/runner/v1/jobs/"+jobID+"/events", map[string]any{"events": []any{
		ev(1, "task_start", "", map[string]any{"task": "a"}),
		ev(2, "host_ok", "web1", map[string]any{"changed": true}),
		ev(3, "host_ok", "web2", map[string]any{"changed": false}),
		ev(4, "host_failed", "web2", map[string]any{"ignored": true}),
		ev(5, "host_skipped", "web1", nil),
		ev(6, "host_failed", "db1", map[string]any{"msg": "boom"}),
		ev(7, "host_unreachable", "db2", nil),
		ev(8, "stdout", "", nil),
	}}, 200)

	type row struct {
		status                                                      string
		ok, changed, failed, unreachable, skipped, rescued, ignored float64
	}
	hosts := func() map[string]row {
		out := map[string]row{}
		var order []string
		for _, it := range e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/hosts", nil, 200)["items"].([]any) {
			h := it.(map[string]any)
			order = append(order, h["hostName"].(string))
			out[h["hostName"].(string)] = row{h["status"].(string), h["ok"].(float64), h["changed"].(float64),
				h["failed"].(float64), h["unreachable"].(float64), h["skipped"].(float64), h["rescued"].(float64), h["ignored"].(float64)}
		}
		if strings.Join(order, ",") != strings.Join(slices.Sorted(maps.Keys(out)), ",") {
			t.Errorf("hosts not sorted: %v", order)
		}
		return out
	}
	got := hosts()
	want := map[string]row{
		"web1": {status: "changed", ok: 1, changed: 1, skipped: 1},
		"web2": {status: "ok", ok: 2, ignored: 1},
		"db1":  {status: "failed", failed: 1},
		"db2":  {status: "unreachable", unreachable: 1},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("host %s = %+v, want %+v", name, got[name], w)
		}
	}

	// Ansible's recap, in chunks, replaces the counts.
	r.mustDo("POST", "/runner/v1/jobs/"+jobID+"/events", map[string]any{"events": []any{
		ev(9, "playbook_stats", "", map[string]any{"hosts": map[string]any{
			"db1": map[string]any{"ok": 2, "changed": 0, "failures": 0, "unreachable": 0, "skipped": 0, "rescued": 1, "ignored": 0},
		}}),
		ev(10, "playbook_stats", "", map[string]any{"hosts": map[string]any{
			"web1": map[string]any{"ok": 5, "changed": 2, "failures": 0, "unreachable": 0, "skipped": 1, "rescued": 0, "ignored": 0},
		}}),
	}}, 200)
	got = hosts()
	if got["db1"] != (row{status: "ok", ok: 2, rescued: 1}) || got["web1"].ok != 5 || got["web2"].ignored != 1 {
		t.Errorf("after recap: %+v", got)
	}

	// Events can be filtered by host.
	items := e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/events?hostName=web2", nil, 200)["items"].([]any)
	if len(items) != 2 {
		t.Errorf("web2 events = %s", jsonOf(items))
	}
	e.c.mustDo("GET", "/api/v1/jobs/00000000-0000-0000-0000-000000000000/hosts", nil, 404)
}

// TestAnsibleJob runs a real playbook. It needs ansible-playbook and
// ansible-vault on PATH; hosts use the local connection.
func TestAnsibleJob(t *testing.T) {
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook is not installed")
	}
	e := newJobEnv(t)

	// A vault-encrypted variable, decrypted with a vault credential.
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("vaultpw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enc, err := exec.Command("ansible-vault", "encrypt_string", "--vault-id", "prod@"+pw, "v4ult", "--name", "secret").Output()
	if err != nil {
		t.Fatalf("ansible-vault: %v", err)
	}
	url, commit := gitRepo(t, map[string]string{
		"vars.yml": string(enc),
		"playbooks/site.yml": `- name: Configure
  hosts: all
  gather_facts: false
  vars_files: [../vars.yml]
  tasks:
    - name: Say hello
      ansible.builtin.debug:
        msg: "hello {{ inventory_hostname }} color={{ color }} vault_ok={{ secret == 'v4ult' }}"
    - name: Change something
      ansible.builtin.command: "true"
      changed_when: true
    - name: Optional
      ansible.builtin.command: "false"
      ignore_errors: true
      when: inventory_hostname == 'web2'
    - name: Hidden
      ansible.builtin.debug:
        msg: "{{ 'top' ~ 'secret' }}"
      no_log: true
    - name: Break web2
      ansible.builtin.fail:
        msg: boom
      when: inventory_hostname == 'web2'
    - name: Tagged
      ansible.builtin.debug:
        msg: tagged
      tags: [extra]
`,
	})
	inv := e.c.mustDo("POST", "/api/v1/inventories", map[string]any{"name": unique("inv"), "variables": map[string]any{
		"ansible_connection": "local", "ansible_python_interpreter": "{{ ansible_playbook_python }}",
	}}, 201)["id"].(string)
	for _, h := range []string{"web1", "web2", "db1"} {
		e.c.mustDo("POST", "/api/v1/inventories/"+inv+"/hosts", map[string]any{"name": h}, 201)
	}
	vault := e.c.mustDo("POST", "/api/v1/credentials", map[string]any{
		"name": unique("vault"), "kind": "vault", "inputs": map[string]string{"vault_id": "prod", "password": "vaultpw"},
	}, 201)["id"].(string)

	jobID := e.launchTemplate(map[string]any{
		"jobType": "ansible", "projectId": e.project(url, "main"), "entrypoint": "playbooks/site.yml",
		"inventoryId": inv, "limit": "web*", "skipTags": "extra", "verbosity": 1,
		"extraVars": map[string]any{"color": "blue"}, "credentialIds": []string{vault},
	})
	job := e.run(jobID)
	if job["status"] != "failed" || job["exitCode"].(float64) != 2 || job["scmRevision"] != commit {
		t.Errorf("job = %s", jsonOf(job))
	}

	out := e.stdout(jobID)
	for _, want := range []string{"hello web1 color=blue vault_ok=True", "PLAY RECAP", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"topsecret", "dispatch-event", "\x1e", "db1", "tagged"} {
		if strings.Contains(out, bad) {
			t.Errorf("stdout has %q:\n%s", bad, out)
		}
	}

	// Per-host results come from Ansible's recap.
	hosts := e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/hosts", nil, 200)["items"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("hosts = %s", jsonOf(hosts))
	}
	web1, web2 := hosts[0].(map[string]any), hosts[1].(map[string]any)
	if web1["hostName"] != "web1" || web1["status"] != "changed" || web1["changed"].(float64) != 1 || web1["failed"].(float64) != 0 {
		t.Errorf("web1 = %v", web1)
	}
	if web2["status"] != "failed" || web2["failed"].(float64) != 1 || web2["ignored"].(float64) != 1 {
		t.Errorf("web2 = %v", web2)
	}

	// Structured events: per-task results, with no_log output hidden.
	var types []string
	var failMsg string
	for _, it := range e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/events?hostName=web2&limit=5000", nil, 200)["items"].([]any) {
		ev := it.(map[string]any)
		types = append(types, ev["type"].(string))
		data := ev["data"].(map[string]any)
		if strings.Contains(jsonOf(data), "topsecret") {
			t.Errorf("no_log result leaked: %v", data)
		}
		if ev["type"] == "host_failed" && data["ignored"] == false {
			failMsg = data["msg"].(string)
			if data["task"] != "Break web2" || data["play"] != "Configure" {
				t.Errorf("failed event = %v", data)
			}
		}
	}
	if strings.Join(types, ",") != "host_ok,host_ok,host_failed,host_ok,host_failed" || failMsg != "boom" {
		t.Errorf("web2 events = %v, fail msg %q", types, failMsg)
	}
	all := e.c.mustDo("GET", "/api/v1/jobs/"+jobID+"/events?limit=5000", nil, 200)["items"].([]any)
	seen := map[string]bool{}
	for _, it := range all {
		seen[it.(map[string]any)["type"].(string)] = true
	}
	for _, typ := range []string{"playbook_start", "play_start", "task_start", "host_skipped", "playbook_stats", "runner", "stdout"} {
		if !seen[typ] {
			t.Errorf("no %s event", typ)
		}
	}
}
