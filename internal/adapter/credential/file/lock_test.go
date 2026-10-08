package file

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
)

func assertCredentialFileUnchanged(t *testing.T, path string, original []byte) {
	t.Helper()
	current, err := os.ReadFile(path) //nolint:gosec // this test owns the private credential path
	if err != nil || !bytes.Equal(current, original) {
		t.Errorf("canceled transaction changed credential file, err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Errorf("transaction left lock or staging files, err=%v", err)
	}
}

func seededCredentialStore(t *testing.T) (*Store, []byte) {
	t.Helper()
	store, _ := activeStore(t, filepath.Join(t.TempDir(), "credentials.yaml"))
	credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "fixture-original"}
	if _, err := store.Modify(t.Context(), "openai", func(*llm.Credential) (*llm.Credential, error) {
		return &credential, nil
	}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	return store, original
}

func TestStore_CanceledBeforeTransaction(t *testing.T) {
	for _, operation := range []string{"modify", "delete"} {
		t.Run(operation, func(t *testing.T) {
			store, original := seededCredentialStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var err error
			if operation == "delete" {
				err = store.Delete(ctx, "openai")
			} else {
				_, err = store.Modify(ctx, "openai", func(*llm.Credential) (*llm.Credential, error) {
					t.Error("pre-canceled transaction invoked mutation")
					return &llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "fixture-replacement"}, nil
				})
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("pre-canceled %s = %v", operation, err)
			}
			assertCredentialFileUnchanged(t, store.path, original)
		})
	}
}

func TestStore_CanceledDuringAcquisitionAndRead(t *testing.T) {
	for _, phase := range []string{"acquire", "read"} {
		t.Run(phase, func(t *testing.T) {
			store, original := seededCredentialStore(t)
			originalOpen, originalRead := credentialOpenFile, credentialReadFile
			t.Cleanup(func() { credentialOpenFile, credentialReadFile = originalOpen, originalRead })
			entered, release := make(chan struct{}), make(chan struct{})
			if phase == "acquire" {
				credentialReadFile = func(path string) ([]byte, error) {
					t.Error("canceled acquisition read the credential document")
					return originalRead(path)
				}
				credentialOpenFile = func(path string, flag int, mode os.FileMode) (credentialFile, error) {
					file, err := originalOpen(path, flag, mode)
					if path == store.path+".lock" {
						close(entered)
						<-release
					}
					return file, err
				}
			} else {
				credentialReadFile = func(path string) ([]byte, error) {
					encoded, err := originalRead(path)
					close(entered)
					<-release
					return encoded, err
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := store.Modify(ctx, "openai", func(*llm.Credential) (*llm.Credential, error) {
					t.Error("canceled transaction invoked mutation")
					return nil, nil
				})
				finished <- err
			}()
			<-entered
			cancel()
			close(release)
			if err := <-finished; !errors.Is(err, context.Canceled) {
				t.Errorf("canceled during %s = %v", phase, err)
			}
			credentialOpenFile, credentialReadFile = originalOpen, originalRead
			assertCredentialFileUnchanged(t, store.path, original)
		})
	}
}

func TestStore_CanceledWaiterReturnsBeforeHolder(t *testing.T) {
	if os.Getenv("NANO_CREDENTIAL_QUEUE_TEST") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestStore_CanceledWaiterReturnsBeforeHolder$") //nolint:gosec // executable is this test binary from os.Executable, with fixed arguments
		command.Env = []string{"NANO_CREDENTIAL_QUEUE_TEST=1", "TMPDIR=" + t.TempDir()}
		// Mutex waits are not durably blocked in synctest. Isolate a regressed
		// deadlock so the parent reports a named failure and removes child files.
		if err := command.Run(); err != nil {
			t.Fatalf("canceled waiter remained blocked in helper: %v", err)
		}
		return
	}
	synctest.Test(t, func(t *testing.T) {
		store, original := seededCredentialStore(t)
		entered, release := make(chan struct{}), make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			_, err := store.Modify(t.Context(), "openai", func(*llm.Credential) (*llm.Credential, error) {
				close(entered)
				<-release
				return nil, errors.New("fixture holder abort")
			})
			holderDone <- err
		}()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waiterDone := make(chan error, 1)
		go func() { waiterDone <- store.Delete(ctx, "openai") }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		var waiterErr error
		returned := false
		select {
		case waiterErr = <-waiterDone:
			returned = true
		default:
			t.Error("canceled waiter remained blocked behind holder")
		}
		close(release)
		if err := <-holderDone; err == nil {
			t.Error("holder abort missing")
		}
		if !returned {
			waiterErr = <-waiterDone
		}
		if !errors.Is(waiterErr, context.Canceled) {
			t.Errorf("canceled waiter = %v", waiterErr)
		}
		assertCredentialFileUnchanged(t, store.path, original)
	})
}

func TestWithLock_CancellationWinsExpiredWait(t *testing.T) {
	originalOpen, originalWait := credentialOpenFile, credentialLockWait
	t.Cleanup(func() { credentialOpenFile, credentialLockWait = originalOpen, originalWait })
	credentialLockWait = time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.lock")
		for range 32 {
			entered, release := make(chan struct{}), make(chan struct{})
			credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) {
				close(entered)
				<-release
				return nil, os.ErrExist
			}
			ctx, cancel := context.WithCancel(t.Context())
			finished := make(chan error, 1)
			go func() {
				finished <- withLock(ctx, path, func() error {
					t.Error("canceled waiter invoked operation")
					return nil
				})
			}()
			<-entered
			// Both cancellation and the lock deadline are ready before open returns.
			deadline := time.NewTimer(credentialLockWait)
			<-deadline.C
			cancel()
			close(release)
			if err := <-finished; !errors.Is(err, context.Canceled) {
				t.Errorf("expired wait masked cancellation: %v", err)
			}
		}
	})
}

func TestWithLock_PreCanceledDoesNotCreateLock(t *testing.T) {
	originalOpen := credentialOpenFile
	t.Cleanup(func() { credentialOpenFile = originalOpen })
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) {
		t.Error("pre-canceled waiter attempted lock creation")
		return nil, errors.New("unexpected lock attempt")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := withLock(ctx, filepath.Join(t.TempDir(), "credentials.lock"), func() error {
		t.Error("pre-canceled waiter invoked operation")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("pre-canceled lock = %v", err)
	}
}

func TestStore_RefreshCommitBoundary(t *testing.T) {
	for _, outcome := range []string{"success", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			store, original := seededCredentialStore(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			finished := make(chan error, 1)
			refreshed := llm.Credential{
				Kind: llm.CredentialOAuth, AccessToken: "fixture-rotated-access",
				RefreshToken: "fixture-rotated-refresh", ExpiresUnixMS: 1,
			}
			go func() {
				_, err := store.Modify(ctx, "openai", func(*llm.Credential) (*llm.Credential, error) {
					close(entered)
					<-release
					if outcome == "cancel" {
						return nil, ctx.Err()
					}
					return &refreshed, nil
				})
				finished <- err
			}()
			<-entered
			cancel()
			close(release)
			err := <-finished
			if outcome == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("canceled refresh = %v", err)
				}
				assertCredentialFileUnchanged(t, store.path, original)
				return
			}
			if err != nil {
				t.Fatalf("successful refresh was discarded after cancellation: %v", err)
			}
			loaded, err := store.Resolve(t.Context(), "openai", "UNUSED")
			if err != nil || loaded.AccessToken != refreshed.AccessToken || loaded.RefreshToken != refreshed.RefreshToken {
				t.Errorf("rotated grant was not persisted, err=%v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(store.path))
			if err != nil || len(entries) != 1 {
				t.Errorf("refresh left lock or staging files, err=%v", err)
			}
		})
	}
}
