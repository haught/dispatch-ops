package runner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"dispatch-ops/internal/runner/runnerapi"
)

// Batching limits for sending events.
const (
	maxEventBytes = 16 << 10  // longer lines are split into several events
	maxBatchCount = 500       // events per request
	maxBatchBytes = 256 << 10 // output and data bytes per request (the API caps bodies at 1 MiB)
	flushInterval = 250 * time.Millisecond
	sendRetryTime = 2 * time.Minute
)

// emitter numbers events and sends them to the API in ordered batches, one
// request at a time, retrying failures. Adding blocks when the queue is
// full, which slows the command down rather than dropping output.
type emitter struct {
	client *runnerapi.ClientWithResponses
	jobID  uuid.UUID
	echo   io.Writer

	queue chan queued
	done  chan struct{}

	// sending is held (shared) while queueing and (exclusively) to close
	// the queue, so add never sends on a closed channel.
	sending sync.RWMutex
	closed  bool

	mu  sync.Mutex // guards seq and queue order
	seq int64

	errMu sync.Mutex
	err   error // first permanent failure

	// onCancel is called when the API says the job should be canceled;
	// onOver when the API no longer accepts reports for the job.
	onCancel func()
	onOver   func()
}

// queued is an event waiting to be sent, with its approximate size.
type queued struct {
	ev   runnerapi.RunnerEvent
	size int
}

// eventType is the API's pattern for event types.
var eventType = regexp.MustCompile(`^[a-z][a-z_]{0,63}$`)

func newEmitter(client *runnerapi.ClientWithResponses, jobID uuid.UUID, echo io.Writer, onCancel, onOver func()) *emitter {
	e := &emitter{
		client: client, jobID: jobID, echo: echo,
		queue: make(chan queued, 2000), done: make(chan struct{}),
		onCancel: onCancel, onOver: onOver,
	}
	go e.loop()
	return e
}

// add queues one output event. It also echoes the text to the runner's own
// output, so `kubectl logs` shows the job's output too.
func (e *emitter) add(typ, text string) {
	echo := text + "\n"
	if typ == "runner" {
		echo = "[dispatch-runner] " + echo
	}
	e.queueEvent(runnerapi.RunnerEvent{Type: typ, Stdout: text}, len(text), echo)
}

// addStructured queues a structured event, with the lines of output that
// belong to it (if any), and returns its seq. size is roughly how many
// bytes its data takes as JSON.
func (e *emitter) addStructured(typ, host string, taskSeq int64, data map[string]any, lines []string, size int) int64 {
	ev := runnerapi.RunnerEvent{Type: typ}
	if host != "" {
		ev.HostName = &host
	}
	if taskSeq > 0 {
		ev.TaskSeq = &taskSeq
	}
	if data != nil {
		ev.Data = &data
	}
	echo := ""
	if len(lines) > 0 {
		ev.Stdout = strings.Join(lines, "\n")
		echo = ev.Stdout + "\n"
	}
	return e.queueEvent(ev, size+len(ev.Stdout), echo)
}

// queueEvent numbers and queues ev, and returns its seq.
func (e *emitter) queueEvent(ev runnerapi.RunnerEvent, size int, echo string) int64 {
	e.sending.RLock()
	defer e.sending.RUnlock()
	if e.closed {
		return 0
	}
	// Number and queue under one lock: events must reach the API in seq
	// order, or a stream reader could move past a seq that arrives later.
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	now := time.Now().UTC()
	ev.Seq, ev.Time = e.seq, &now
	if echo != "" {
		_, _ = io.WriteString(e.echo, echo)
	}
	e.queue <- queued{ev: ev, size: size}
	return e.seq
}

// runner queues a message from the runner itself.
func (e *emitter) runner(msg string) { e.add("runner", msg) }

// maxEventText is the most output one structured event carries (the API
// takes 64 KiB).
const maxEventText = 60 << 10

// openEvent is a structured event whose output is still being read.
type openEvent struct {
	eventLine
	taskSeq int64
	lines   []string
	text    int // bytes of lines, with separators
}

// consume reads r line by line into events until EOF. Lines that start
// with marker (if not empty) open and close structured events (see
// parseEventLine); the lines in between are the event's output. Other
// lines are stdout events.
func (e *emitter) consume(r io.Reader, marker string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxEventBytes+1)
	sc.Split(splitLines)
	// The seq of each task's task_start event, by task UUID, for the
	// task's host results to refer to.
	tasks := map[string]int64{}
	var open *openEvent
	flush := func() {
		if open == nil {
			return
		}
		seq := e.addStructured(open.typ, open.host, open.taskSeq, open.data, open.lines, open.size)
		if open.typ == "task_start" && open.task != "" {
			tasks[open.task] = seq
		}
		open = nil
	}
	for sc.Scan() {
		line := string(sc.Bytes())
		ev, kind := parseEventLine(line, marker)
		switch kind {
		case eventOpen:
			flush()
			open = &openEvent{eventLine: ev}
			if strings.HasPrefix(ev.typ, "host_") {
				open.taskSeq = tasks[ev.task]
			}
		case eventClose:
			flush()
		default:
			if open != nil {
				if open.text+len(line)+1 <= maxEventText {
					open.lines = append(open.lines, line)
					open.text += len(line) + 1
					continue
				}
				// Too much to go with the event: send it, and the rest of
				// its output as stdout.
				flush()
			}
			e.add("stdout", line)
		}
	}
	flush()
}

// lineWriter returns a writer whose lines become stdout events, and a
// function that ends it and waits until its last line is queued.
func (e *emitter) lineWriter() (io.Writer, func()) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		e.consume(pr, "")
		close(done)
	}()
	return pw, func() {
		pw.Close()
		<-done
	}
}

// splitLines is bufio.ScanLines, except that lines longer than
// maxEventBytes are returned in pieces instead of failing the scan.
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 && i <= maxEventBytes {
		return i + 1, bytes.TrimSuffix(data[:i], []byte("\r")), nil
	}
	if len(data) >= maxEventBytes {
		return maxEventBytes, data[:maxEventBytes], nil
	}
	if atEOF {
		return len(data), bytes.TrimSuffix(data, []byte("\r")), nil
	}
	return 0, nil, nil
}

// close sends everything queued and stops. It returns the first error that
// stopped events from being delivered.
func (e *emitter) close() error {
	e.sending.Lock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	e.sending.Unlock()
	<-e.done
	e.errMu.Lock()
	defer e.errMu.Unlock()
	return e.err
}

func (e *emitter) loop() {
	defer close(e.done)
	var batch []runnerapi.RunnerEvent
	size := 0
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	flush := func() {
		if len(batch) > 0 {
			e.send(batch)
		}
		batch, size = nil, 0
	}
	for {
		select {
		case q, ok := <-e.queue:
			if !ok {
				flush()
				return
			}
			if len(batch) == 0 {
				timer.Reset(flushInterval)
			}
			batch = append(batch, q.ev)
			size += q.size
			if len(batch) >= maxBatchCount || size >= maxBatchBytes {
				flush()
			}
		case <-timer.C:
			flush()
		}
	}
}

func (e *emitter) send(batch []runnerapi.RunnerEvent) {
	e.errMu.Lock()
	failed := e.err != nil
	e.errMu.Unlock()
	if failed {
		return // keep draining the queue so the command isn't blocked
	}
	err := retry(context.Background(), sendRetryTime, func(ctx context.Context) error {
		resp, err := e.client.PostRunnerJobEventsWithResponse(ctx, e.jobID, runnerapi.RunnerEventBatch{Events: batch})
		if err != nil {
			return err
		}
		if err := classify(resp.StatusCode(), resp.Body, 200); err != nil {
			return err
		}
		if resp.JSON200 != nil && resp.JSON200.CancelRequested {
			e.onCancel()
		}
		return nil
	})
	if err != nil {
		e.errMu.Lock()
		e.err = err
		e.errMu.Unlock()
		e.onOver()
	}
}
