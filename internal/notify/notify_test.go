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
		{KindGoogleChat, map[string]any{"url": "https://chat.googleapis.com/x"}, "webhookUrl"},
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
		{KindGoogleChat, nil, map[string]string{}, "need the secret webhookUrl"},
		{KindGoogleChat, nil, map[string]string{"webhookUrl": "http://chat.googleapis.com/x"}, "https"},
		{KindGoogleChat, nil, map[string]string{"webhookUrl": "https://chat.googleapis.com/x", "token": "t"}, `unknown secret "token"`},
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
	if err := CheckSecrets(KindGoogleChat, nil, map[string]string{
		"webhookUrl": "https://chat.googleapis.com/v1/spaces/AAA/messages?key=k&token=t",
	}); err != nil {
		t.Error(err)
	}
	if c, err := NormalizeConfig(KindGoogleChat, nil); err != nil || len(c) != 0 {
		t.Errorf("NormalizeConfig(googlechat) = %v, %v", c, err)
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

func TestGoogleChatMessage(t *testing.T) {
	s := &Sender{ExternalURL: "https://ops.example.com/"}
	j := testJob()
	got, _ := json.Marshal(s.GoogleChatMessage(Event{Name: EventFailed, Job: j}))
	want := `{"fallbackText":"Job \"deploy \u003cprod\u003e \u0026 more\" failed","cardsV2":[{"cardId":"dispatch-ops","card":{` +
		`"header":{"title":"❌ Job \"deploy \u003cprod\u003e \u0026 more\" failed","subtitle":"Dispatch Ops"},"sections":[{"widgets":[` +
		`{"decoratedText":{"text":"\u003cfont color=\"#d93025\"\u003efailed\u003c/font\u003e","topLabel":"Status"}},` +
		`{"decoratedText":{"text":"2","topLabel":"Exit code"}},` +
		`{"decoratedText":{"text":"exit status 2","topLabel":"Reason"}},` +
		`{"decoratedText":{"text":"alice (manual)","topLabel":"Launched by"}},` +
		`{"decoratedText":{"text":"1m5s","topLabel":"Duration"}},` +
		`{"buttonList":{"buttons":[{"onClick":{"openLink":{"url":"https://ops.example.com/jobs/` + j.ID.String() + `"}},"text":"Open job"}]}}` +
		`]}]}}]}`
	if string(got) != want {
		t.Errorf("GoogleChatMessage =\n%s\nwant\n%s", got, want)
	}

	// Widget text is HTML-escaped; the header is plain text.
	j.FailureReason = "<b>boom</b> & more"
	msg := s.GoogleChatMessage(Event{Name: EventFailed, Job: j})
	if w := msg.CardsV2[0].Card.Sections[0].Widgets[2]["decoratedText"].(map[string]any); w["text"] != "&lt;b&gt;boom&lt;/b&gt; &amp; more" {
		t.Errorf("reason widget = %v", w)
	}

	// A test message has no job, so no button (nor without an external URL).
	for _, s := range []*Sender{s, {}} {
		msg := s.GoogleChatMessage(Event{Name: EventTest})
		c := msg.CardsV2[0].Card
		if c.Header.Title != "🔔 Test notification from Dispatch Ops" || len(c.Sections[0].Widgets) != 1 ||
			c.Sections[0].Widgets[0]["textParagraph"] == nil {
			t.Errorf("test message = %+v", c)
		}
	}
	if b, _ := json.Marshal((&Sender{}).GoogleChatMessage(Event{Name: EventSucceeded, Job: j})); strings.Contains(string(b), "buttonList") {
		t.Errorf("message without an external URL has a button: %s", b)
	}
}

func TestSendGoogleChat(t *testing.T) {
	status := 200
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()

	s := &Sender{}
	target := store.NotificationTarget{Kind: KindGoogleChat}
	sec := map[string]string{"webhookUrl": srv.URL + "/v1/spaces/AAA/messages?key=k&token=t%3D"}
	j := testJob()
	if err := s.Send(context.Background(), target, sec, Event{Name: EventFailed, Job: j}); err != nil {
		t.Fatal(err)
	}
	q := got.URL.Query()
	if got.Method != http.MethodPost || got.URL.Path != "/v1/spaces/AAA/messages" ||
		!strings.HasPrefix(got.Header.Get("Content-Type"), "application/json") ||
		q.Get("key") != "k" || q.Get("token") != "t=" ||
		q.Get("threadKey") != "dispatch-ops-job-"+j.ID.String() ||
		q.Get("messageReplyOption") != "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" {
		t.Errorf("request = %s %s %v", got.Method, got.URL, got.Header)
	}
	var msg GoogleChatMessage
	if json.Unmarshal(gotBody, &msg) != nil || len(msg.CardsV2) != 1 || !strings.Contains(msg.FallbackText, "failed") {
		t.Errorf("body = %s", gotBody)
	}

	// A test message isn't threaded.
	if err := s.Send(context.Background(), target, sec, Event{Name: EventTest}); err != nil {
		t.Fatal(err)
	}
	if q := got.URL.Query(); q.Has("threadKey") || q.Has("messageReplyOption") || q.Get("key") != "k" {
		t.Errorf("test request URL = %s", got.URL)
	}

	for code, permanent := range map[int]bool{500: false, 429: false, 400: true, 403: true} {
		status = code
		err := s.Send(context.Background(), target, sec, Event{Name: EventTest})
		if err == nil || IsPermanent(err) != permanent {
			t.Errorf("status %d: err = %v, permanent = %v", code, err, IsPermanent(err))
		}
	}
	if err := s.Send(context.Background(), target, nil, Event{Name: EventTest}); !IsPermanent(err) {
		t.Errorf("no webhookUrl: err = %v", err)
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

// A failed delivery's error is stored and shown to every reader of the job,
// so it must not quote the webhook URL, whose path and query are the secret.
func TestSendErrorsDoNotLeakWebhookURL(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	host := strings.TrimPrefix(srv.URL, "http://")
	const pathSecret = "T000/B000/PATHSECRET"
	const querySecret = "QUERYSECRET"
	s := &Sender{Timeout: 50 * time.Millisecond}
	cases := map[string]struct {
		target  store.NotificationTarget
		secrets map[string]string
	}{
		"slack": {store.NotificationTarget{Kind: KindSlack},
			map[string]string{"webhookUrl": srv.URL + "/services/" + pathSecret}},
		"google chat": {store.NotificationTarget{Kind: KindGoogleChat},
			map[string]string{"webhookUrl": srv.URL + "/v1/spaces/X/messages?key=" + querySecret + "&token=" + querySecret}},
		"webhook": {store.NotificationTarget{Kind: KindWebhook,
			Config: map[string]any{"url": srv.URL + "/hook/" + pathSecret + "?token=" + querySecret}}, nil},
		"unreachable": {store.NotificationTarget{Kind: KindSlack},
			map[string]string{"webhookUrl": "http://127.0.0.1:1/services/" + pathSecret}},
	}
	for name, c := range cases {
		err := s.Send(context.Background(), c.target, c.secrets, Event{Name: EventTest})
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		msg := err.Error()
		if strings.Contains(msg, "PATHSECRET") || strings.Contains(msg, querySecret) || strings.Contains(msg, "/services/") {
			t.Errorf("%s: error leaks the URL: %s", name, msg)
		}
		if name != "unreachable" && !strings.HasPrefix(msg, host+": ") {
			t.Errorf("%s: error = %q, want it to name the host %s", name, msg, host)
		}
		if IsPermanent(err) {
			t.Errorf("%s: a transport failure should be retried: %v", name, err)
		}
	}
}
