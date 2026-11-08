package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dispatch-ops/internal/runner/runnerapi"
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

// newClient returns an API client that authenticates with the job token.
func newClient(apiURL, token, version string) (*runnerapi.ClientWithResponses, error) {
	return runnerapi.NewClientWithResponses(apiURL,
		runnerapi.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
		runnerapi.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("User-Agent", "dispatch-runner/"+version)
			return nil
		}),
	)
}
