package file

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

type syncBarrierFile struct {
	*os.File
	entered chan<- struct{}
	release <-chan struct{}
}

func (file *syncBarrierFile) Sync() error {
	err := file.File.Sync()
	file.entered <- struct{}{}
	<-file.release
	return err
}

func TestWrite_CancellationDuringSyncDoesNotPublish(t *testing.T) {
	for _, present := range []bool{true, false} {
		name := "create"
		if present {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			testCanceledPublication(t, "write", present)
		})
	}
}

func TestEdit_CancellationDuringSyncDoesNotPublish(t *testing.T) {
	testCanceledPublication(t, "edit", true)
}

func testCanceledPublication(t *testing.T, tool string, present bool) {
	t.Helper()
	restoreHooks(t)
	h := newHarness(t)
	if present {
		writeFixture(t, h.path("picked.txt"), "before")
		h.read(t, "picked.txt")
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	createTemp = func(directory, pattern string) (stagedFile, error) {
		file, err := os.CreateTemp(directory, pattern)
		return &syncBarrierFile{File: file, entered: entered, release: release}, err
	}
	publications := 0
	renameFile = func(from, to string) error {
		publications++
		return os.Rename(from, to)
	}
	linkFile = func(from, to string) error {
		publications++
		return os.Link(from, to)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	finished, exited := make(chan error, 1), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() {
		cancel()
		unblock.Do(func() { close(release) })
		<-exited
	})
	go func() {
		defer close(exited)
		invocation := appTool.Invocation{SessionID: "session", Approved: true}
		var err error
		if tool == "write" {
			_, err = h.provider.write(ctx, invocation, writeArgs{FilePath: "picked.txt", Content: "after"})
		} else {
			_, err = h.provider.edit(ctx, invocation, editArgs{FilePath: "picked.txt", OldString: "before", NewString: "after"})
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("write did not reach the Sync barrier")
	}
	cancel()
	unblock.Do(func() { close(release) })
	err := <-finished
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%s after cancellation = %v, want context.Canceled", tool, err)
	}
	if publications != 0 {
		t.Errorf("%s published %d times after cancellation", tool, publications)
	}
	entries, readErr := os.ReadDir(h.root.Path())
	wantEntries := 0
	if present {
		wantEntries = 1
		if got := readFixture(t, h.path("picked.txt")); got != "before" {
			t.Errorf("target = %q, want before", got)
		}
	}
	if readErr != nil || len(entries) != wantEntries {
		t.Errorf("staging or target residue = %v, %v", entries, readErr)
	}
	prior, _ := h.provider.observed.lookup("session", h.path("picked.txt"))
	if prior.present != present || present && prior.version != digest([]byte("before")) {
		t.Errorf("cancellation changed observation: %+v", prior)
	}
}
