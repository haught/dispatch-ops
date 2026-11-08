// Package jobstream tells live job streams in this API server when a job has
// changed, using PostgreSQL LISTEN/NOTIFY so that any replica can serve any
// stream.
package jobstream

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"dispatch-ops/internal/store"
)

// Hub fans job change notifications out to subscribers. Subscribers get a
// wake-up signal, not data: on each signal they re-read the job and its new
// events from the database. That keeps notifications small and makes a
// missed or merged signal harmless.
type Hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[chan struct{}]struct{}
}

// NewHub returns an idle hub; start it with Run.
func NewHub() *Hub {
	return &Hub{subs: map[uuid.UUID]map[chan struct{}]struct{}{}}
}

// Run listens for job notifications until ctx is done.
func (h *Hub) Run(ctx context.Context, st *store.Store, log *slog.Logger) {
	st.Listen(ctx, []string{store.ChannelJobs, store.ChannelJobEvents},
		func(n store.Notification) {
			if n.Channel == "" {
				h.wakeAll()
				return
			}
			if id, err := uuid.Parse(n.Payload); err == nil {
				h.wake(id)
			}
		},
		func(err error) { log.Warn("job stream listener disconnected; reconnecting", "err", err) },
	)
}

// Subscribe returns a channel that receives a value whenever the job may
// have changed. Call the returned function to unsubscribe.
func (h *Hub) Subscribe(jobID uuid.UUID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[jobID] == nil {
		h.subs[jobID] = map[chan struct{}]struct{}{}
	}
	h.subs[jobID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[jobID], ch)
		if len(h.subs[jobID]) == 0 {
			delete(h.subs, jobID)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) wake(jobID uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[jobID] {
		signal(ch)
	}
}

func (h *Hub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subs := range h.subs {
		for ch := range subs {
			signal(ch)
		}
	}
}

// signal sends without blocking; a pending signal already covers this one.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
