package dbworker

import (
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/derision-test/glock"
	"github.com/inconshreveable/log15"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sourcegraph/sourcegraph/internal/workerutil/dbworker/store"
)

// Resetter periodically moves all unlocked records that have been in the processing state
// for a while back to queued.
//
// An unlocked record signifies that it is not actively being processed and records in this
// state for more than a few seconds are very likely to be stuck after the worker processing
// them has crashed.
type Resetter struct {
	store    store.Store
	options  ResetterOptions
	clock    glock.Clock
	ctx      context.Context // root context passed to the database
	cancel   func()          // cancels the root context
	finished chan struct{}   // signals that Start has finished
}

type ResetterOptions struct {
	Name     string
	Interval time.Duration
	Metrics  ResetterMetrics
}

type ResetterMetrics struct {
	RecordResets        prometheus.Counter
	RecordResetFailures prometheus.Counter
	Errors              prometheus.Counter
}

func NewResetter(store store.Store, options ResetterOptions) *Resetter {
	return newResetter(store, options, glock.NewRealClock())
}

func newResetter(store store.Store, options ResetterOptions, clock glock.Clock) *Resetter {
	if options.Name == "" {
		panic("no name supplied to github.com/sourcegraph/sourcegraph/internal/dbworker/newResetter")
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Resetter{
		store:    store,
		options:  options,
		clock:    clock,
		ctx:      ctx,
		cancel:   cancel,
		finished: make(chan struct{}),
	}
}

// Start begins periodically calling reset stalled on the underlying store.
func (r *Resetter) Start() {
	defer close(r.finished)

loop:
	for {
		resetLastHeartbeatsByIDs, failedLastHeartbeatsByIDs, err := r.store.ResetStalled(r.ctx)
		if err != nil {
			if r.ctx.Err() != nil && errors.Is(err, r.ctx.Err()) {
				// If the error is due to the loop being shut down, just break
				break loop
			}

			r.options.Metrics.Errors.Inc()
			log15.Error("Failed to reset stalled records", "name", r.options.Name, "error", err)
		}

		for id, lastHeartbeatAge := range resetLastHeartbeatsByIDs {
			log15.Warn("Reset stalled record back to 'queued' state", "name", r.options.Name, "id", id, "timeSinceLastHeartbeat", lastHeartbeatAge)
		}
		for id, lastHeartbeatAge := range failedLastHeartbeatsByIDs {
			log15.Warn("Reset stalled record to 'failed' state", "name", r.options.Name, "id", id, "timeSinceLastHeartbeat", lastHeartbeatAge)
		}

		r.options.Metrics.RecordResets.Add(float64(len(resetLastHeartbeatsByIDs)))
		r.options.Metrics.RecordResetFailures.Add(float64(len(failedLastHeartbeatsByIDs)))

		select {
		case <-r.clock.After(r.options.Interval):
		case <-r.ctx.Done():
			return
		}
	}
}

// Stop will cause the resetter loop to exit after the current iteration.
func (r *Resetter) Stop() {
	r.cancel()
	<-r.finished
}
