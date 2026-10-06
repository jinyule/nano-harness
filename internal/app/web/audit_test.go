package web

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type journalFunc func(context.Context, session.Record) (session.Event, error)

func (fn journalFunc) Append(ctx context.Context, record session.Record) (session.Event, error) {
	return fn(ctx, record)
}

func TestService_SearchAuditFailurePreventsDispatchAndPreservesCause(t *testing.T) {
	current := newFixture(t, configured, webStore{}, nil)
	var dispatched atomic.Int32
	current.model.search = func(context.Context, llm.SearchRequest) (llm.SearchResult, error) {
		dispatched.Add(1)
		return llm.SearchResult{}, nil
	}
	if _, err := current.service.Search(t.Context(), []string{"go"}, SearchInvocation{}); err == nil || !strings.Contains(err.Error(), "owning agent session") {
		t.Fatalf("no journal=%v", err)
	}
	failure := errors.New("disk failed: private-secret")
	invocation := searchOwner()
	invocation.Journal = journalFunc(func(context.Context, session.Record) (session.Event, error) { return session.Event{}, failure })
	for _, queries := range [][]string{{"go"}, {"go", "rust", "swift", "zig"}} {
		_, err := current.service.Search(t.Context(), queries, invocation)
		message := expectCode(t, err, CodeRequestRecordFailed)
		if !errors.Is(err, failure) || strings.Contains(message, "private-secret") || dispatched.Load() != 0 {
			t.Fatalf("failure=%v dispatched=%d", err, dispatched.Load())
		}
	}
}

func TestService_OrdersAuditsWhileQueriesRunConcurrently(t *testing.T) {
	for count := 1; count <= 4; count++ {
		current := newFixture(t, configured, webStore{}, nil)
		var mu sync.Mutex
		var records []session.Record
		invocation := searchOwner()
		invocation.Journal = journalFunc(func(_ context.Context, record session.Record) (session.Event, error) {
			if err := record.Validate(); err != nil {
				return session.Event{}, err
			}
			mu.Lock()
			defer mu.Unlock()
			records = append(records, record)
			return session.Event{Sequence: uint64(len(records)), Record: record}, nil
		})
		var arrived sync.WaitGroup
		arrived.Add(count)
		current.model.search = func(_ context.Context, request llm.SearchRequest) (llm.SearchResult, error) {
			mu.Lock()
			found := false
			for _, record := range records {
				found = found || record.Search.Query == request.Query
			}
			mu.Unlock()
			if !found {
				t.Error("dispatched query has no committed audit")
			}
			arrived.Done()
			arrived.Wait()
			return llm.SearchResult{}, nil
		}
		queries := []string{"go", "rust", "swift", "zig"}[:count]
		inputs := append([]string(nil), queries...)
		if count < 4 {
			inputs = append(inputs, queries[0])
		}
		if _, err := current.service.Search(t.Context(), inputs, invocation); err != nil {
			t.Fatal(err)
		}

		var audited []string
		for i, record := range records {
			if record.Turn != invocation.Turn || record.Step != invocation.Step || record.Search.CallID != invocation.CallID || record.Search.Index != i+1 {
				t.Fatalf("causality=%+v", record)
			}
			audited = append(audited, record.Search.Query)
		}
		if !reflect.DeepEqual(audited, queries) {
			t.Fatalf("order=%v want=%v", audited, queries)
		}
	}
}
