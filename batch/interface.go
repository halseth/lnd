package batch

import "github.com/lightningnetwork/lnd/channeldb/kvdb"

// Request defines an operation that can be batched into a single bbolt
// transaction.
type Request struct {
	// Update is applied alongside other operations in the batch.
	//
	// NOTE: This method MUST NOT acquire any mutexes.
	Update func(tx kvdb.RwTx) error

	// OnSuccess is applied if the batch or a subset of the batch including
	// this request all succeeded without failure.
	//
	// NOTE: This field is optional.
	OnSuccess func(err error) error

	// Solo is applied if this request's Update failed, and was asked to
	// retry outside of the batch.
	Solo func() error
}

// Scheduler abstracts a generic batching engine that accumulates an incoming
// set of Requests, executes them, and returns the error from the operation.
type Scheduler interface {
	// Execute schedules a Request for execution with the next available
	// batch. This method blocks until the the underlying closure has been
	// run against the databse. The resulting error is returned to the
	// caller.
	Execute(req *Request) error
}
