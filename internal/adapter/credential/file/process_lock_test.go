package file

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
)

func TestStore_ProcessLockHolder(t *testing.T) {
	path := os.Getenv("NANO_CREDENTIAL_LOCK_HELPER")
	if path == "" {
		return
	}
	store, _ := activeStore(t, path)
	_, err := store.Modify(t.Context(), "openai", func(current *llm.Credential) (*llm.Credential, error) {
		if _, err := fmt.Fprintln(os.Stdout, "held"); err != nil {
			return nil, err
		}
		var release [1]byte
		if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
			return nil, err
		}
		return current, nil
	})
	if err != nil {
		t.Fatal("lock holder failed")
	}
}

func TestStore_CrossProcessCancellationPreservesHolder(t *testing.T) {
	store, original := seededCredentialStore(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestStore_ProcessLockHolder$") //nolint:gosec // executable is this test binary from os.Executable, with fixed arguments
	command.Env = []string{"NANO_CREDENTIAL_LOCK_HELPER=" + store.path, "TMPDIR=" + t.TempDir()}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			_ = command.Wait()
		}
	})
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "held\n" {
		t.Fatal("child did not acquire credential lock")
	}
	originalOpen := credentialOpenFile
	t.Cleanup(func() { credentialOpenFile = originalOpen })
	attempted := make(chan struct{}, 1)
	credentialOpenFile = func(path string, flag int, mode os.FileMode) (credentialFile, error) {
		file, err := originalOpen(path, flag, mode)
		if errors.Is(err, os.ErrExist) {
			select {
			case attempted <- struct{}{}:
			default:
			}
		}
		return file, err
	}
	waitContext, stopWaiting := context.WithCancel(t.Context())
	defer stopWaiting()
	finished := make(chan error, 1)
	go func() { finished <- store.Delete(waitContext, "openai") }()
	<-attempted
	stopWaiting()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("cross-process canceled waiter = %v", err)
	}
	if _, err := os.Stat(store.path + ".lock"); err != nil {
		t.Errorf("waiter removed holder's lock, err=%v", err)
	}
	current, err := os.ReadFile(store.path)
	if err != nil || !bytes.Equal(current, original) {
		t.Errorf("waiter changed credential file, err=%v", err)
	}
	if _, err := input.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		waited = true
		t.Fatalf("credential holder failed: %v", err)
	}
	waited = true
	credentialOpenFile = originalOpen
	assertCredentialFileUnchanged(t, store.path, original)
	if err := store.Delete(t.Context(), "openai"); err != nil {
		t.Fatalf("lock was not reusable after holder exit: %v", err)
	}
}
