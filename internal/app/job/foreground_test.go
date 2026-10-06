package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestService_ForegroundSettlementBeforeWaitDoesNotNotify(t *testing.T) {
	for _, status := range []Status{StatusCompleted, StatusFailed, StatusKilled} {
		t.Run(string(status), func(t *testing.T) {
			service, notifier, _ := startService(t)
			producer := newGate()
			id, err := service.Launch(Spec{Kind: "bash", Label: "fast", Owner: "root", Foreground: true, Run: producer.run})
			if err != nil {
				t.Fatal(err)
			}
			<-producer.started
			producer.release <- Outcome{Status: status}
			// Join includes settle and Notify, forcing completion before Wait.
			service.group.Wait()
			if view, err := service.Wait(t.Context(), "root", id, time.Minute); err != nil || view.Status != status {
				t.Fatalf("foreground wait = %+v, %v", view, err)
			}
			if err := service.Remove("root", id); err != nil {
				t.Fatal(err)
			}
			if texts := notifier.texts(); len(texts) != 0 {
				t.Fatalf("foreground completion notified an undisclosed job: %q", texts)
			}
		})
	}
}

func TestService_ForegroundNoticeTransfersAtRead(t *testing.T) {
	for _, settledBeforeRead := range []bool{true, false} {
		name := "settlement before handoff"
		if !settledBeforeRead {
			name = "settlement after handoff"
		}
		t.Run(name, func(t *testing.T) {
			service, notifier, _ := startService(t)
			producer := newGate()
			id, err := service.Launch(Spec{Kind: "bash", Label: "hold", Owner: "root", Foreground: true, Run: producer.run})
			if err != nil {
				t.Fatal(err)
			}
			output := <-producer.started
			_, _ = output.Writer(Stdout).Write([]byte("partial"))
			if view, err := service.Wait(t.Context(), "root", id, time.Nanosecond); err != nil || view.Status != StatusRunning {
				t.Fatalf("timeout = %+v, %v", view, err)
			}
			if settledBeforeRead {
				producer.release <- Outcome{Status: StatusCompleted}
				service.group.Wait()
			}
			read, err := service.Read("root", id)
			if err != nil || read.Stdout != "partial" || (read.Job.Status == StatusCompleted) != settledBeforeRead {
				t.Fatalf("handoff = %+v, %v", read, err)
			}
			if !settledBeforeRead {
				producer.release <- Outcome{Status: StatusCompleted}
				service.group.Wait()
			}
			if read, err := service.Read("root", id); err != nil || read.Stdout != "" {
				t.Fatalf("repeated read = %+v, %v", read, err)
			}
			want := 1
			if settledBeforeRead {
				want = 0
			}
			if texts := notifier.texts(); len(texts) != want {
				t.Fatalf("completion notices = %q, want %d", texts, want)
			}
		})
	}
}

func TestService_ForegroundCancellationBeforeKillDoesNotNotify(t *testing.T) {
	service, notifier, _ := startService(t)
	producer := newGate()
	id, err := service.Launch(Spec{Kind: "bash", Label: "hold", Owner: "root", Foreground: true, Run: producer.run})
	if err != nil {
		t.Fatal(err)
	}
	<-producer.started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Wait(ctx, "root", id, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	producer.release <- Outcome{Status: StatusCompleted}
	service.group.Wait()
	_, _, _ = service.Kill("root", id, "tool call aborted")
	if texts := notifier.texts(); len(texts) != 0 {
		t.Fatalf("completion between cancelled Wait and Kill notified: %q", texts)
	}
}
