package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/store"
)

// Event is something to notify about: a job event, or a test.
type Event struct {
	// Name is one of the Event* constants.
	Name string
	// Job is the job the event is about; nil for a test.
	Job *store.Job
	// DeliveryID identifies this notification (webhook header
	// X-Dispatch-Delivery); 0 for a test.
	DeliveryID int64
}

// PermanentError is a send failure that retrying won't fix, such as a 4xx
// response.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err should not be retried.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// Sender sends notifications.
type Sender struct {
	// ExternalURL is the server's base URL, for links to jobs. Without it,
	// notifications have no links.
	ExternalURL string
	// Version goes in the User-Agent.
	Version string
	// HTTPClient sends webhook and Slack notifications; nil uses a default
	// with a timeout that doesn't follow redirects.
	HTTPClient *http.Client
	// Timeout bounds each send (default 30s).
	Timeout time.Duration
}

func (s *Sender) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{
		Timeout: s.timeout(),
		// A redirect is reported as an error rather than followed, so a
		// POST never turns into a GET somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (s *Sender) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 30 * time.Second
}

// Send sends ev to target, whose decrypted secret values are secrets.
func (s *Sender) Send(ctx context.Context, target store.NotificationTarget, secrets map[string]string, ev Event) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	switch target.Kind {
	case KindWebhook:
		var c WebhookConfig
		if err := decodeStrict(target.Config, &c); err != nil {
			return &PermanentError{err}
		}
		return s.sendWebhook(ctx, c, secrets, ev)
	case KindSlack:
		return s.sendSlack(ctx, secrets["webhookUrl"], ev)
	case KindEmail:
		var c EmailConfig
		if err := decodeStrict(target.Config, &c); err != nil {
			return &PermanentError{err}
		}
		return s.sendEmail(ctx, c, secrets["password"], ev)
	}
	return &PermanentError{fmt.Errorf("unknown notification target kind %q", target.Kind)}
}

// --- Content -------------------------------------------------------------------

// JobURL is the job's page in the UI, or "".
func (s *Sender) JobURL(j *store.Job) string {
	if s.ExternalURL == "" || j == nil {
		return ""
	}
	return strings.TrimRight(s.ExternalURL, "/") + "/jobs/" + j.ID.String()
}

// Payload is the JSON body of a webhook notification.
type Payload struct {
	Event string      `json:"event"`
	Job   *JobPayload `json:"job"`
}

// JobPayload describes the job in a webhook notification.
type JobPayload struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	URL           string     `json:"url,omitempty"`
	JobTemplateID *uuid.UUID `json:"jobTemplateId"`
	JobType       string     `json:"jobType"`
	LaunchType    string     `json:"launchType"`
	LaunchedBy    *string    `json:"launchedBy"`
	Schedule      *string    `json:"schedule"`
	ExitCode      *int       `json:"exitCode"`
	FailureReason string     `json:"failureReason"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
}

func (s *Sender) payload(ev Event) Payload {
	p := Payload{Event: ev.Name}
	if j := ev.Job; j != nil {
		p.Job = &JobPayload{
			ID: j.ID, Name: j.Name, Status: j.Status, URL: s.JobURL(j), JobTemplateID: j.TemplateID,
			JobType: j.Config.JobType, LaunchType: j.LaunchType, ExitCode: j.ExitCode, FailureReason: j.FailureReason,
			CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
		}
		if j.LaunchedBy != nil {
			p.Job.LaunchedBy = &j.LaunchedBy.Username
		}
		if j.Schedule != nil {
			p.Job.Schedule = &j.Schedule.Name
		}
	}
	return p
}

var eventVerbs = map[string]string{
	EventStarted: "started", EventSucceeded: "succeeded", EventFailed: "failed", EventCanceled: "was canceled",
}

// Summary is a one-line description of ev, e.g. `Job "deploy" failed`.
func Summary(ev Event) string {
	j := ev.Job
	if j == nil {
		return "Test notification from Dispatch Ops"
	}
	s := fmt.Sprintf("Job %q %s", j.Name, eventVerbs[ev.Name])
	if ev.Name == EventFailed && j.Status == store.JobError {
		s = fmt.Sprintf("Job %q could not run", j.Name)
	}
	return s
}

// details lists facts about the job, one "Label: value" per line.
func (s *Sender) details(ev Event) []string {
	j := ev.Job
	if j == nil {
		return []string{"This target is set up correctly."}
	}
	lines := []string{"Status: " + j.Status}
	if j.ExitCode != nil {
		lines = append(lines, "Exit code: "+strconv.Itoa(*j.ExitCode))
	}
	if j.FailureReason != "" {
		lines = append(lines, "Reason: "+j.FailureReason)
	}
	launched := j.LaunchType
	switch {
	case j.Schedule != nil:
		launched = "schedule " + j.Schedule.Name
	case j.LaunchedBy != nil:
		launched = j.LaunchedBy.Username + " (" + j.LaunchType + ")"
	}
	lines = append(lines, "Launched by: "+launched)
	if j.StartedAt != nil && j.FinishedAt != nil {
		lines = append(lines, "Duration: "+j.FinishedAt.Sub(*j.StartedAt).Round(time.Second).String())
	}
	return lines
}

// --- Webhook -------------------------------------------------------------------

func (s *Sender) sendWebhook(ctx context.Context, c WebhookConfig, secrets map[string]string, ev Event) error {
	body, err := json.Marshal(s.payload(ev))
	if err != nil {
		return &PermanentError{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return &PermanentError{err}
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Dispatch-Event", ev.Name)
	if ev.DeliveryID != 0 {
		h.Set("X-Dispatch-Delivery", strconv.FormatInt(ev.DeliveryID, 10))
	}
	if a := secrets["authorization"]; a != "" {
		h.Set("Authorization", a)
	}
	if key := secrets["signingSecret"]; key != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		h.Set("X-Dispatch-Timestamp", ts)
		h.Set("X-Dispatch-Signature", "sha256="+Sign(key, ts, body))
	}
	return s.post(req)
}

// Sign returns the hex HMAC-SHA256, keyed with secret, of
// "<timestamp>.<body>": the X-Dispatch-Signature of a webhook notification.
// Receivers should also reject old timestamps, to stop replays.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// post sends req, classifying failures: 4xx responses (except timeouts and
// rate limits) and redirects are permanent.
func (s *Sender) post(req *http.Request) error {
	req.Header.Set("User-Agent", "dispatch-ops/"+s.Version)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("%s responded %s", req.URL.Host, resp.Status)
	if msg := strings.TrimSpace(string(snippet)); msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	switch {
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooManyRequests:
		return err
	case resp.StatusCode < 500:
		return &PermanentError{err}
	}
	return err
}

// --- Slack ---------------------------------------------------------------------

var slackIcons = map[string]string{
	EventStarted: ":arrow_forward:", EventSucceeded: ":white_check_mark:", EventFailed: ":x:",
	EventCanceled: ":no_entry_sign:", EventTest: ":bell:",
}

// slackEscape escapes text for Slack's mrkdwn.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// SlackText is the message posted to Slack for ev.
func (s *Sender) SlackText(ev Event) string {
	title := slackEscape(Summary(ev))
	if u := s.JobURL(ev.Job); u != "" {
		title = fmt.Sprintf("<%s|%s>", u, title)
	}
	lines := []string{slackIcons[ev.Name] + " *" + title + "*"}
	for _, d := range s.details(ev) {
		lines = append(lines, slackEscape(d))
	}
	return strings.Join(lines, "\n")
}

func (s *Sender) sendSlack(ctx context.Context, webhookURL string, ev Event) error {
	if webhookURL == "" {
		return &PermanentError{errors.New("the Slack target has no webhookUrl")}
	}
	body, _ := json.Marshal(map[string]string{"text": s.SlackText(ev)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return &PermanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	return s.post(req)
}

// --- Email ---------------------------------------------------------------------

// EmailMessage returns the RFC 5322 message sent for ev.
func (s *Sender) EmailMessage(c EmailConfig, ev Event, now time.Time) []byte {
	subject := "[Dispatch Ops] " + strings.NewReplacer("\r", " ", "\n", " ").Replace(Summary(ev))
	var body strings.Builder
	body.WriteString(Summary(ev) + ".\n\n")
	for _, d := range s.details(ev) {
		body.WriteString(d + "\n")
	}
	if u := s.JobURL(ev.Job); u != "" {
		body.WriteString("\n" + u + "\n")
	}

	var msg bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&msg, "%s: %s\r\n", k, v) }
	header("From", c.From)
	header("To", strings.Join(c.To, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", "<"+rand.Text()+"@dispatch-ops>")
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "quoted-printable")
	header("Auto-Submitted", "auto-generated")
	msg.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&msg)
	_, _ = qp.Write([]byte(strings.ReplaceAll(body.String(), "\n", "\r\n")))
	_ = qp.Close()
	return msg.Bytes()
}

func (s *Sender) sendEmail(ctx context.Context, c EmailConfig, password string, ev Event) error {
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return &PermanentError{fmt.Errorf("from: %w", err)}
	}
	var to []string
	for _, a := range c.To {
		addr, err := mail.ParseAddress(a)
		if err != nil {
			return &PermanentError{fmt.Errorf("to %q: %w", a, err)}
		}
		to = append(to, addr.Address)
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	tlsConfig := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{}
	var conn net.Conn
	if c.Security == SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP %s: %w", addr, err)
	}
	defer client.Close()

	if c.Security == SecurityStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return &PermanentError{fmt.Errorf("SMTP server %s does not offer STARTTLS; set security to tls or none", addr)}
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS with %s: %w", addr, err)
		}
	}
	if c.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", c.Username, password, c.Host)); err != nil {
			return smtpErr("authenticate", err)
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return smtpErr("MAIL FROM", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return smtpErr("RCPT TO "+rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return smtpErr("DATA", err)
	}
	if _, err := w.Write(s.EmailMessage(c, ev, time.Now())); err != nil {
		return smtpErr("DATA", err)
	}
	if err := w.Close(); err != nil {
		return smtpErr("DATA", err)
	}
	return client.Quit()
}

// smtpErr wraps an SMTP command's error; 5xx replies are permanent.
func smtpErr(what string, err error) error {
	var reply *textproto.Error
	permanent := errors.As(err, &reply) && reply.Code >= 500
	err = fmt.Errorf("SMTP %s: %w", what, err)
	if permanent {
		return &PermanentError{err}
	}
	return err
}
