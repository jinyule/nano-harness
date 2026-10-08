package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCalls_CancelReachesEveryAdmittedCallWithItsCause(t *testing.T) {
	var owner sync.Mutex
	calls := NewCalls(&owner)
	stopped := errors.New("stopped")
	owner.Lock()
	first, releaseFirst := calls.Admit(context.Background())
	second, releaseSecond := calls.Admit(context.Background())
	calls.Cancel(stopped)
	owner.Unlock()
	for _, call := range []context.Context{first, second} {
		if !errors.Is(call.Err(), context.Canceled) || !errors.Is(context.Cause(call), stopped) {
			t.Fatalf("call err=%v cause=%v", call.Err(), context.Cause(call))
		}
	}
	releaseFirst()
	releaseSecond()
	calls.Wait()

	owner.Lock()
	plain, release := calls.Admit(context.Background())
	calls.Cancel(nil)
	owner.Unlock()
	if !errors.Is(context.Cause(plain), context.Canceled) {
		t.Fatalf("nil cause = %v", context.Cause(plain))
	}
	release()
}

func TestCalls_ReleaseCancelsUnregistersAndUnblocksWait(t *testing.T) {
	var owner sync.Mutex
	calls := NewCalls(&owner)
	parent, cancelParent := context.WithCancel(context.Background())
	owner.Lock()
	call, release := calls.Admit(parent)
	owner.Unlock()
	if call.Err() != nil {
		t.Fatalf("admitted call is already done: %v", call.Err())
	}
	waited := make(chan struct{})
	go func() {
		calls.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned before the call was released")
	default:
	}
	release()
	<-waited
	if !errors.Is(call.Err(), context.Canceled) {
		t.Fatalf("released call err = %v", call.Err())
	}
	owner.Lock()
	registered := len(calls.cancel)
	owner.Unlock()
	if registered != 0 {
		t.Fatalf("registry retained %d released calls", registered)
	}
	cancelParent()
	owner.Lock()
	derived, releaseDerived := calls.Admit(parent)
	owner.Unlock()
	if !errors.Is(derived.Err(), context.Canceled) {
		t.Fatalf("call ignored its parent's cancellation: %v", derived.Err())
	}
	releaseDerived()
}

func TestCalls_ConcurrentCallsReleaseUnderTheOwnersLock(t *testing.T) {
	var owner sync.Mutex
	calls := NewCalls(&owner)
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			owner.Lock()
			_, release := calls.Admit(context.Background())
			owner.Unlock()
			release()
		})
	}
	group.Wait()
	calls.Wait()
	if len(calls.cancel) != 0 {
		t.Fatalf("registry retained %d calls", len(calls.cancel))
	}
}
