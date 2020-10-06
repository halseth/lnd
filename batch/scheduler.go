package batch

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
)

// TimeScheduler is a batching engine that executes requests within a fixed
// horizon. When the first request is received, a TimeScheduler waits a
// configurable duration for other concurrent requests to join the batch. Once
// this time has elapsed, the batch is closed and executed. Subsequent requests
// are then added to a new batch which undergoes the same process.
type TimeScheduler struct {
	db       kvdb.Backend
	locker   sync.Locker
	duration time.Duration

	mu sync.Mutex
	b  *batch
}

// NewTimeScheduler initializes a new TimeScheduler with a fixed duration at
// which to schedule batches. If the operation needs to modify a higher-level
// cache, the cache's lock should be provided so that external consistency can
// be maintained, as successful db operations will cause a request's OnSuccess
// method to be executed while holding this lock.
func NewTimeScheduler(db kvdb.Backend, locker sync.Locker,
	duration time.Duration) *TimeScheduler {

	return &TimeScheduler{
		db:       db,
		locker:   locker,
		duration: duration,
	}
}

// Execute schedules the provided request for batch execution along with other
// concurrent requests. The request will be executed within a fixed horizon,
// parameterized by the duration of the scheduler. The error from the underlying
// operation is returned to the caller.
func (s *TimeScheduler) Execute(r *Request) error {
	req := request{
		Request: r,
		errChan: make(chan error, 1),
	}

	// Add the request to the current batch. If the batch has been cleared
	// or no batch exists, create a new one.
	s.mu.Lock()
	if s.b == nil {
		s.b = &batch{
			db:     s.db,
			clear:  s.clear,
			locker: s.locker,
		}
		time.AfterFunc(s.duration, s.b.trigger)
	}
	s.b.reqs = append(s.b.reqs, &req)
	s.mu.Unlock()

	// Wait for the batch to process the request. If the batch didn't
	// ask us to execute the request individually, simply return the error.
	err := <-req.errChan
	if err != errSolo {
		return err
	}

	// Otherwise, run the request on its own.
	return req.Solo()
}

// clear resets the scheduler's batch to nil so that no more requests can be
// added.
func (s *TimeScheduler) clear(b *batch) {
	s.mu.Lock()
	if s.b == b {
		s.b = nil
	}
	s.mu.Unlock()
}
