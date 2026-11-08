package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notification is a PostgreSQL NOTIFY message. A Notification with an empty
// Channel is sent after each (re)connect: notifications may have been
// missed, so listeners should re-check whatever they track.
type Notification struct {
	Channel string
	Payload string
}

// Notification channels.
const (
	// ChannelJobs carries a job ID when a job is created or its status or
	// cancel request changes (see the jobs_notify triggers).
	ChannelJobs = "dispatch_jobs"
	// ChannelJobEvents carries a job ID when events are appended to it.
	ChannelJobEvents = "dispatch_job_events"
)

// Listen LISTENs on channels over a dedicated connection and calls handle
// for each notification, reconnecting with backoff after errors (which it
// passes to onError). It returns when ctx is done.
func (s *Store) Listen(ctx context.Context, channels []string, handle func(Notification), onError func(error)) {
	backoff := time.Second
	for {
		connected, err := s.listenOnce(ctx, channels, handle)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		onError(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Store) listenOnce(ctx context.Context, channels []string, handle func(Notification)) (connected bool, err error) {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	// Take the connection out of the pool: it stays subscribed, so it must
	// not be handed to anyone else.
	conn := pooled.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))

	for _, ch := range channels {
		if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{ch}.Sanitize()); err != nil {
			return false, err
		}
	}
	handle(Notification{})
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		handle(Notification{Channel: n.Channel, Payload: n.Payload})
	}
}
