package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/store"
)

func TestNormalizeConfigAndSecrets(t *testing.T) {
	email, err := NormalizeConfig(KindEmail, map[string]any{
		"host": "smtp.example.com", "from": "Dispatch <ops@example.com>", "to": []string{"a@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if email["port"] != float64(587) || email["security"] != "starttls" {
		t.Errorf("email defaults = %v", email)
	}
	tls, _ := NormalizeConfig(KindEmail, map[string]any{
		"host": "smtp.example.com", "security": "tls", "from": "ops@example.com", "to": []string{"a@example.com"},
	})
	if tls["port"] != float64(465) {
		t.Errorf("implicit TLS port = %v", tls["port"])
	}

	for _, tc := range []struct {
		kind   string
		config map[string]any
		want   string
	}{
		{KindWebhook, map[string]any{"url": "ftp://x"}, "http or https"},
		{KindWebhook, map[string]any{"url": "https://x", "token": "t"}, `unknown field "token"`},
		{KindSlack, map[string]any{"url": "https://hooks.slack.com/x"}, "webhookUrl"},
		{KindEmail, map[string]any{"host": "smtp", "from": "x", "to": []string{"a@example.com"}}, "from"},
		{KindEmail, map[string]any{"host": "smtp", "from": "ops@example.com"}, "at least one"},
		{"pager", nil, "unknown"},
	} {
		if _, err := NormalizeConfig(tc.kind, tc.config); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("NormalizeConfig(%s, %v) = %v, want %q", tc.kind, tc.config, err, tc.want)
		}
	}

	for _, tc := range []struct {
		kind    string
		config  map[string]any
		secrets map[string]string
		want    string
	}{
		{KindSlack, nil, map[string]string{}, "need the secret webhookUrl"},
		{KindSlack, nil, map[string]string{"webhookUrl": "http://hooks.slack.com/x"}, "https"},
		{KindWebhook, nil, map[string]string{"password": "x"}, `unknown secret "password"`},
		{KindEmail, map[string]any{"username": "u"}, map[string]string{}, "needs the secret password"},
		{KindEmail, map[string]any{"username": "u", "security": "none"}, map[string]string{"password": "p"}, "encrypted connection"},
	} {
		if err := CheckSecrets(tc.kind, tc.config, tc.secrets); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CheckSecrets(%s, %v) = %v, want %q", tc.kind, tc.secrets, err, tc.want)
		}
	}
	if err := CheckSecrets(KindSlack, nil, map[string]string{"webhookUrl": "https://hooks.slack.com/x"}); err != nil {
		t.Error(err)
	}

	merged := MergeSecrets(map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "", "c": "3"})
	if len(merged) != 2 || merged["b"] != "2" || merged["c"] != "3" {
		t.Errorf("MergeSecrets = %v", merged)
	}
}

func TestVaultRoundTrip(t *testing.T) {
	keys, _ := secrets.NewKeyring(secrets.GenerateKey())
	v := Vault{Keys: keys}
	n := store.NotificationTarget{ID: uuid.New(), Name: "n"}
	if err := v.Seal(&n, map[string]string{"webhookUrl": "https://x", "b": "y"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(n.SecretValues), "https://x") || len(n.SecretFields) != 2 || n.SecretFields[0] != "b" {
		t.Errorf("sealed = %+v", n)
	}
	got, err := v.Open(n)
	if err != nil || got["webhookUrl"] != "https://x" {
		t.Errorf("Open = %v, %v", got, err)
	}
	// The ciphertext is bound to the target.
	n.ID = uuid.New()
	if _, err := v.Open(n); err == nil {
		t.Error("opened another target's secrets")
	}
}

func testJob() *store.Job {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(65 * time.Second)
	return &store.Job{
		ID: uuid.New(), Name: "deploy <prod> & more", Status: store.JobFailed, LaunchType: "manual",
		LaunchedBy: &store.UserRef{Username: "alice"}, ExitCode: new(2), FailureReason: "exit status 2",
		StartedAt: &start, FinishedAt: &end, Config: store.JobConfig{JobType: "ansible"},
	}
}

func TestSlackText(t *testing.T) {
	s := &Sender{ExternalURL: "https://ops.example.com/"}
	j := testJob()
	got := s.SlackText(Event{Name: EventFailed, Job: j})
	want := ":x: *<https://ops.example.com/jobs/" + j.ID.String() + `|Job "deploy &lt;prod&gt; &amp; more" failed>*` +
		"\nStatus: failed\nExit code: 2\nReason: exit status 2\nLaunched by: alice (manual)\nDuration: 1m5s"
	if got != want {
		t.Errorf("SlackText =\n%s\nwant\n%s", got, want)
	}
}

func TestSendClassifiesFailures(t *testing.T) {
	status := 200
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		if status == 302 {
			w.Header().Set("Location", "https://elsewhere.example.com/")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "nope")
	}))
	defer srv.Close()

	s := &Sender{}
	slack := store.NotificationTarget{Kind: KindSlack}
	sec := map[string]string{"webhookUrl": srv.URL}
	if err := s.Send(context.Background(), slack, sec, Event{Name: EventTest}); err != nil {
		t.Fatal(err)
	}
	var msg map[string]string
	if json.Unmarshal(gotBody, &msg) != nil || !strings.Contains(msg["text"], "Test notification") {
		t.Errorf("slack body = %s", gotBody)
	}
	for code, permanent := range map[int]bool{500: false, 429: false, 400: true, 404: true, 302: true} {
		status = code
		err := s.Send(context.Background(), slack, sec, Event{Name: EventTest})
		if err == nil || IsPermanent(err) != permanent || !strings.Contains(err.Error(), strconv.Itoa(code)) {
			t.Errorf("status %d: err = %v, permanent = %v", code, err, IsPermanent(err))
		}
	}
}

// fakeSMTP is just enough of an SMTP server to receive one plain message.
func fakeSMTP(t *testing.T) (addr string, received <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		reply := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
		reply("220 fake ESMTP")
		var transcript strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			transcript.WriteString(line)
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				reply("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				reply("250 ok")
			case cmd == "DATA":
				reply("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					transcript.WriteString(l)
				}
				reply("250 queued")
			case cmd == "QUIT":
				reply("221 bye")
				out <- transcript.String()
				return
			default:
				reply("502 unknown")
			}
		}
	}()
	return ln.Addr().String(), out
}

func TestSendEmail(t *testing.T) {
	addr, received := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	cfg, err := NormalizeConfig(KindEmail, map[string]any{
		"host": host, "port": p, "security": "none", "from": "Dispatch Ops <ops@example.com>",
		"to": []string{"oncall@example.com", "Bob <bob@example.com>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Sender{ExternalURL: "https://ops.example.com", Timeout: 5 * time.Second}
	j := testJob()
	if err := s.Send(context.Background(), store.NotificationTarget{Kind: KindEmail, Config: cfg}, nil, Event{Name: EventFailed, Job: j}); err != nil {
		t.Fatal(err)
	}
	var got string
	select {
	case got = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
	}
	for _, want := range []string{
		"MAIL FROM:<ops@example.com>", "RCPT TO:<oncall@example.com>", "RCPT TO:<bob@example.com>",
		`Subject: [Dispatch Ops] Job "deploy <prod> & more" failed`, "To: oncall@example.com, Bob <bob@example.com>",
		"Exit code: 2", "https://ops.example.com/jobs/" + j.ID.String(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
}

func TestEmailSubjectCannotInjectHeaders(t *testing.T) {
	j := testJob()
	j.Name = "x\r\nBcc: evil@example.com"
	msg := string((&Sender{}).EmailMessage(EmailConfig{From: "a@example.com", To: []string{"b@example.com"}},
		Event{Name: EventFailed, Job: j}, time.Now()))
	headers, _, _ := strings.Cut(msg, "\r\n\r\n")
	if strings.Contains(headers, "\r\nBcc:") {
		t.Errorf("header injection:\n%s", headers)
	}
}
