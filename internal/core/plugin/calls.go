package plugin

import (
	"context"
	"sync"
)

// Calls tracks the cancellable calls a plugin admits while it runs, so its
// cleanup can cancel them and wait until each has returned.
//
// Admission stays the owner's decision: the owner holds its own lock, checks
// its running state, then calls Admit; its cleanup clears that state and calls
// Cancel under the same lock, and calls Wait after releasing it. Admit and
// Cancel therefore require the owner's lock, while the release function Admit
// returns takes it itself.
type Calls struct {
	owner  sync.Locker
	next   uint64
	cancel map[uint64]context.CancelCauseFunc
	group  sync.WaitGroup
}

// NewCalls tracks calls admitted under owner, the lock that guards the
// owning plugin's running state.
func NewCalls(owner sync.Locker) *Calls {
	return &Calls{owner: owner, cancel: map[uint64]context.CancelCauseFunc{}}
}

// Admit registers one call derived from ctx. The returned release function
// cancels the call's context and must run exactly once, after the call has
// returned and without the owner's lock held.
func (calls *Calls) Admit(ctx context.Context) (context.Context, func()) {
	call, cancel := context.WithCancelCause(ctx)
	calls.next++
	key := calls.next
	calls.cancel[key] = cancel
	calls.group.Add(1)
	return call, func() {
		cancel(nil)
		calls.owner.Lock()
		delete(calls.cancel, key)
		calls.owner.Unlock()
		calls.group.Done()
	}
}

// Cancel cancels every admitted call with cause; a nil cause reports
// context.Canceled.
func (calls *Calls) Cancel(cause error) {
	for _, cancel := range calls.cancel {
		cancel(cause)
	}
}

// Wait blocks until every admitted call has been released. The owner calls
// it after Cancel, without its lock held.
func (calls *Calls) Wait() { calls.group.Wait() }
