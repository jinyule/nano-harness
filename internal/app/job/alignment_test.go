package job

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutput_DecodesBeforeCountingRetention(t *testing.T) {
	service, _, _ := startService(t)
	id, output := launch(t, service, "root", newGate())
	raw := bytes.Repeat([]byte{0xff, 'a'}, liveRetainBytes/2)
	_, _ = output.Writer(Stdout).Write(raw)
	read, err := service.Read("root", id)
	if err != nil || !utf8.ValidString(read.Stdout) || len(read.Stdout) > liveRetainBytes || !read.Lossy {
		t.Fatalf("decoded retention: bytes=%d valid=%v lossy=%v err=%v", len(read.Stdout), utf8.ValidString(read.Stdout), read.Lossy, err)
	}
}

func TestOutput_DecodeIsIndependentOfWriteBoundaries(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"\xff\xff", "��"},
		{"\xe2\x82x", "�x"},
		{"\xe2\x82", "�"},
		{"\xe0\x80\x80", "���"},
		{"界�", "界�"},
	} {
		for _, split := range []bool{false, true} {
			service, _, _ := startService(t)
			producer := newGate()
			id, output := launch(t, service, "root", producer)
			writer := output.Writer(Stdout)
			if split {
				for index := range len(test.raw) {
					_, _ = writer.Write([]byte{test.raw[index]})
				}
			} else {
				_, _ = writer.Write([]byte(test.raw))
			}
			producer.release <- Outcome{Status: StatusCompleted}
			waitSettled(t, service, "root", id)
			read, _ := service.Read("root", id)
			if read.Stdout != test.want || output.record.ring.total != int64(len(test.want)) {
				t.Errorf("raw=%x split=%v: decoded=%q bytes=%d; want=%q", test.raw, split, read.Stdout, output.record.ring.total, test.want)
			}
		}
	}
}

func TestService_OmittedKillReasonPreservesPriorIntent(t *testing.T) {
	service, _, _ := startService(t)
	release := make(chan struct{})
	id, err := service.Launch(Spec{Kind: "bash", Label: "hold", Owner: "root", Run: func(ctx context.Context, _ *Output) Outcome {
		<-ctx.Done()
		<-release
		return Outcome{Status: StatusKilled}
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = service.Kill("root", id, new("keep"))
	_, _, _ = service.Kill("root", id, nil)
	close(release)
	if view := waitSettled(t, service, "root", id); !strings.Contains(view.Detail, "keep") {
		t.Fatalf("omitted reason replaced intent: %+v", view)
	}
}

func TestService_ExplicitEmptyKillReasonClearsPriorIntent(t *testing.T) {
	service, _, _ := startService(t)
	release := make(chan struct{})
	id, err := service.Launch(Spec{Kind: "bash", Label: "hold", Owner: "root", Run: func(ctx context.Context, _ *Output) Outcome {
		<-ctx.Done()
		<-release
		return Outcome{Status: StatusKilled, Detail: "signal: SIGTERM"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = service.Kill("root", id, new("stale"))
	_, _, _ = service.Kill("root", id, new(""))
	close(release)
	if view := waitSettled(t, service, "root", id); view.Detail != "signal: SIGTERM" {
		t.Fatalf("explicit empty reason preserved stale intent: %+v", view)
	}
}

func TestService_WaitChecksIdentityBeforeTimeout(t *testing.T) {
	service, _, _ := startService(t)
	id, _ := launch(t, service, "root", newGate())
	for _, test := range []struct {
		owner, id string
		want      error
	}{{"root", "missing", ErrUnknownJob}, {"child", id, ErrForeignJob}} {
		if _, err := service.Wait(t.Context(), test.owner, test.id, 0); !errors.Is(err, test.want) {
			t.Errorf("Wait(%s,%s,0) = %v; want %v", test.owner, test.id, err, test.want)
		}
	}
}
