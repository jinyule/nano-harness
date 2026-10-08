package job

import (
	"context"
	"errors"
	"testing"
)

func TestService_ReleaseCancelsWaitsAndDropsOnlyTheOwner(t *testing.T) {
	service, notifier, scope := startService(t)
	live, other := newGate(), newGate()
	liveID, err := service.Launch(Spec{Kind: "subagent", Label: "child work", Owner: "child", Run: live.run})
	if err != nil {
		t.Fatal(err)
	}
	settledID, err := service.Launch(Spec{Kind: "subagent", Label: "done", Owner: "child", Run: func(context.Context, *Output) Outcome {
		return Outcome{Status: StatusCompleted, Result: "report"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := service.Launch(Spec{Kind: "subagent", Label: "sibling", Owner: "root", Run: other.run})
	if err != nil {
		t.Fatal(err)
	}
	<-live.started
	<-other.started
	// The settled job notified its owner; wait so it cannot race the release.
	<-notifier.sent
	if err := service.Release(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	// Release returned after the producer settled; both child records are gone.
	for _, id := range []string{liveID, settledID} {
		if _, err := service.Get("child", id); !errors.Is(err, ErrUnknownJob) {
			t.Fatalf("Get(%s) after release = %v", id, err)
		}
	}
	if views := service.List("child"); len(views) != 0 {
		t.Fatalf("child jobs after release = %#v", views)
	}
	if view, err := service.Get("root", otherID); err != nil || view.Status != StatusRunning {
		t.Fatalf("other owner's job = %#v, %v", view, err)
	}
	// The release settlement sent no notice: only the completed job did.
	if texts := notifier.texts(); len(texts) != 1 {
		t.Fatalf("notices = %q", texts)
	}
	other.release <- Outcome{Status: StatusCompleted}
	<-notifier.sent
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Release(context.Background(), "root"); err != nil {
		t.Fatalf("Release after stop = %v", err)
	}
}

func TestService_ReleaseStopsWaitingWhenContextEnds(t *testing.T) {
	service, _, _ := startService(t)
	stuck, unblock := make(chan struct{}), make(chan struct{})
	id, err := service.Launch(Spec{Kind: "subagent", Label: "stuck", Owner: "child", Run: func(context.Context, *Output) Outcome {
		close(stuck)
		<-unblock
		return Outcome{Status: StatusKilled}
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-stuck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Release(ctx, "child"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Release = %v", err)
	}
	if view, err := service.Get("child", id); err != nil || view.Status != StatusStopping {
		t.Fatalf("unsettled job = %#v, %v", view, err)
	}
	close(unblock)
	if view, err := service.Wait(context.Background(), "child", id, 1<<62); err != nil || view.Status != StatusKilled {
		t.Fatalf("settled job = %#v, %v", view, err)
	}
}
