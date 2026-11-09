package runner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/runner/runnerapi"
	"dispatch-ops/internal/tlscert"
)

// errJobOver means the API no longer accepts reports for this job: the token
// was revoked because the job was ended (canceled, timed out, or failed by
// the task manager). The runner should stop the command and exit.
var errJobOver = errors.New("the job has been ended by the server")

// apiError is a non-retryable error response from the API.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("API returned %d: %s", e.status, strings.TrimSpace(e.body))
}

// classify turns a response into nil (success), errJobOver, an *apiError
// (don't retry), or a retryable error.
func classify(status int, body []byte, want int) error {
	switch {
	case status == want:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusConflict:
		return fmt.Errorf("%w (%d: %s)", errJobOver, status, strings.TrimSpace(string(body)))
	case status >= 500 || status == http.StatusTooManyRequests:
		return fmt.Errorf("API returned %d (will retry)", status)
	default:
		return &apiError{status: status, body: string(body)}
	}
}

func permanent(err error) bool {
	var ae *apiError
	return errors.Is(err, errJobOver) || errors.As(err, &ae)
}

// retry calls fn until it succeeds, fails permanently, or maxElapsed passes.
func retry(ctx context.Context, maxElapsed time.Duration, fn func(context.Context) error) error {
	deadline := time.Now().Add(maxElapsed)
	backoff := 500 * time.Millisecond
	for {
		err := fn(ctx)
		if err == nil || permanent(err) {
			return err
		}
		if time.Now().Add(backoff).After(deadline) {
			return fmt.Errorf("giving up after %s: %w", maxElapsed, err)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// newClient returns an API client that authenticates with the job token,
// trusting the system's roots and the PEM certificates in ca.
func newClient(apiURL, token, version string, ca []byte) (*runnerapi.ClientWithResponses, error) {
	roots, err := tlscert.TrustPool(ca)
	if err != nil {
		return nil, fmt.Errorf("the API's CA certificates: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return runnerapi.NewClientWithResponses(apiURL,
		runnerapi.WithHTTPClient(&http.Client{Timeout: 30 * time.Second, Transport: transport}),
		runnerapi.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("User-Agent", "dispatch-runner/"+version)
			return nil
		}),
	)
}

// maxInventoryBytes is the largest inventory the API accepts from a sync.
const maxInventoryBytes = 32 << 20

// reportInventory sends the inventory file at path, as ansible-inventory
// wrote it, for the API to store.
func reportInventory(ctx context.Context, client *runnerapi.ClientWithResponses, jobID uuid.UUID, path string) (*runnerapi.RunnerInventoryResult, error) {
	var out *runnerapi.RunnerInventoryResult
	err := retry(ctx, sendRetryTime, func(ctx context.Context) error {
		f, err := os.Open(path)
		if err != nil {
			return &apiError{status: 0, body: err.Error()}
		}
		defer f.Close()
		resp, err := client.PostRunnerInventoryWithBodyWithResponse(ctx, jobID, "application/json", f)
		if err != nil {
			return err
		}
		if err := classify(resp.StatusCode(), resp.Body, 200); err != nil {
			return err
		}
		out = resp.JSON200
		return nil
	})
	if err == nil && out == nil {
		err = errors.New("the API sent no result")
	}
	return out, err
}

// problemText is the most useful part of an API error for a job's failure
// reason: the problem's detail if the API sent one.
func problemText(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		var p struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal([]byte(ae.body), &p) == nil && (p.Detail != "" || p.Title != "") {
			if p.Detail != "" {
				return p.Detail
			}
			return p.Title
		}
	}
	return err.Error()
}
