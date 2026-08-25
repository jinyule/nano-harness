package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

type faultCredentialFile struct {
	writeN   int
	writeErr error
	syncErr  error
	closeErr error
}

func (file *faultCredentialFile) Write([]byte) (int, error) { return file.writeN, file.writeErr }
func (file *faultCredentialFile) Sync() error               { return file.syncErr }
func (file *faultCredentialFile) Close() error              { return file.closeErr }

func activeStore(t *testing.T, path string) (*Store, *plugin.Scope) {
	t.Helper()
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if store.ID() != "credentials-file" || store.Start(context.Background(), scope) != nil {
		t.Fatal("start")
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return store, scope
}

func TestStoreRoundTripEnvironmentAndLifecycle(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty path=%v", err)
	}
	path := filepath.Join(t.TempDir(), "nested", "credentials.yaml")
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	closedStore, _ := New(path)
	if err := closedStore.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed start scope=%v", err)
	}
	store, scope := activeStore(t, path)
	if store.Path() != path {
		t.Fatalf("path=%q", store.Path())
	}
	if store.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start")
	}
	store.lookup = func(name string) (string, bool) { return "env-key", name == "KEY" }
	credential, err := store.Resolve(context.Background(), "openai", "KEY")
	if err != nil || credential.APIKey != "env-key" {
		t.Fatalf("env=%#v err=%v", credential, err)
	}
	stored := llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "access", RefreshToken: "refresh", ExpiresUnixMS: 1, AccountID: "account", Extra: map[string]string{"source": "test"}}
	credential, err = store.Modify(context.Background(), "openai", func(current *llm.Credential) (*llm.Credential, error) {
		if current != nil {
			t.Fatal("unexpected current")
		}
		return &stored, nil
	})
	if err != nil || credential.AccessToken != "access" {
		t.Fatalf("modify=%#v err=%v", credential, err)
	}
	stored.Extra["source"] = "changed"
	credential, err = store.Resolve(context.Background(), "openai", "KEY")
	if err != nil || credential.Extra["source"] != "test" {
		t.Fatal("credential aliases caller")
	}
	credential.Extra["source"] = "again"
	again, _ := store.Resolve(context.Background(), "openai", "KEY")
	if again.Extra["source"] != "test" {
		t.Fatal("resolve aliases store")
	}
	accounts, err := store.List(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Provider != "openai" || accounts[0].Source != "file" {
		t.Fatalf("accounts=%#v err=%v", accounts, err)
	}
	second := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "second"}
	if _, err := store.Modify(context.Background(), "anthropic", func(*llm.Credential) (*llm.Credential, error) { return &second, nil }); err != nil {
		t.Fatal(err)
	}
	accounts, err = store.List(context.Background())
	if err != nil || len(accounts) != 2 || accounts[0].Provider != "anthropic" || accounts[1].Provider != "openai" {
		t.Fatalf("sorted accounts=%#v err=%v", accounts, err)
	}
	if err := store.Delete(context.Background(), "anthropic"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	if err := store.Delete(context.Background(), "openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), "openai", "MISSING"); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("missing=%v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), "openai", "KEY"); !errors.Is(err, llm.ErrNotRunning) {
		t.Fatalf("closed=%v", err)
	}
	if _, err := store.List(context.Background()); !errors.Is(err, llm.ErrNotRunning) {
		t.Fatalf("list closed=%v", err)
	}
	resumed, resumedScope := activeStore(t, path)
	if accounts, err := resumed.List(context.Background()); err != nil || len(accounts) != 0 {
		t.Fatalf("resumed=%#v err=%v", accounts, err)
	}
	_ = resumedScope.Close(context.Background())
}

func TestStoreValidationAndFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.yaml")
	store, _ := activeStore(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Resolve(ctx, "openai", "KEY"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve cancel=%v", err)
	}
	if _, err := store.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("list cancel=%v", err)
	}
	if _, err := store.Modify(context.Background(), "bad", func(*llm.Credential) (*llm.Credential, error) { return nil, nil }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad provider=%v", err)
	}
	if _, err := store.Modify(context.Background(), "openai", nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil mutate=%v", err)
	}
	inactive, _ := New(filepath.Join(t.TempDir(), "inactive.yaml"))
	if _, err := inactive.Modify(context.Background(), "openai", func(*llm.Credential) (*llm.Credential, error) { return nil, nil }); !errors.Is(err, llm.ErrNotRunning) {
		t.Fatalf("inactive modify=%v", err)
	}
	failure := errors.New("mutate")
	if _, err := store.Modify(context.Background(), "openai", func(*llm.Credential) (*llm.Credential, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatalf("mutate=%v", err)
	}
	if _, err := store.Modify(context.Background(), "openai", func(*llm.Credential) (*llm.Credential, error) { return &llm.Credential{}, nil }); err == nil {
		t.Fatal("invalid credential accepted")
	}
	if validProvider("bad") || !validProvider("anthropic") || !validProvider("openrouter") {
		t.Fatal("validProvider")
	}
	store.lookup = func(string) (string, bool) { return "bad\n", true }
	if _, err := store.Resolve(context.Background(), "openrouter", "KEY"); err == nil {
		t.Fatal("invalid environment credential accepted")
	}
}

func TestStoreRejectsUnsafeDocuments(t *testing.T) {
	for _, test := range []struct {
		name, content string
		mode          os.FileMode
	}{
		{"syntax", "not: [yaml", 0o600},
		{"version", "version: 2\nrecords: {}\n", 0o600},
		{"unknown", "version: 1\nrecords: {}\nextra: true\n", 0o600},
		{"provider", "version: 1\nrecords:\n  bad:\n    kind: api-key\n    api_key: x\n", 0o600},
		{"permissions", "version: 1\nrecords: {}\n", 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.yaml")
			if err := os.WriteFile(path, []byte(test.content), test.mode); err != nil {
				t.Fatal(err)
			}
			store, _ := New(path)
			if err := store.Start(context.Background(), &plugin.Scope{}); err == nil {
				t.Fatal("unsafe document accepted")
			}
		})
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "link")
	if err := os.Symlink(filepath.Join(directory, "missing"), path); err != nil {
		t.Fatal(err)
	}
	store, _ := New(path)
	if err := store.Start(context.Background(), &plugin.Scope{}); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestLockAndRandomFailure(t *testing.T) {
	originalWait, originalRandom := credentialLockWait, randomRead
	t.Cleanup(func() { credentialLockWait, randomRead = originalWait, originalRandom })
	root := t.TempDir()
	lock := filepath.Join(root, "lock")
	if err := os.WriteFile(lock, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialLockWait = time.Millisecond
	if err := withLock(context.Background(), lock, func() error { return nil }); err == nil {
		t.Fatal("lock timeout missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withLock(ctx, lock, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock cancel=%v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("operation")
	if err := withLock(context.Background(), lock, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("operation=%v", err)
	}
	store, _ := activeStore(t, filepath.Join(root, "credentials.yaml"))
	randomRead = func([]byte) (int, error) { return 0, errors.New("random") }
	_, err := store.Modify(context.Background(), "openai", func(*llm.Credential) (*llm.Credential, error) {
		return &llm.Credential{Kind: llm.CredentialAPIKey, APIKey: strings.Repeat("x", 1)}, nil
	})
	if err == nil {
		t.Fatal("random error lost")
	}
}

func TestFilesystemAndAtomicWriteFailures(t *testing.T) {
	originalAbs, originalMkdir := credentialAbs, credentialMkdirAll
	originalLstat, originalRead := credentialLstat, credentialReadFile
	originalMarshal, originalOpen := credentialMarshal, credentialOpenFile
	originalRename, originalRemove := credentialRename, credentialRemove
	t.Cleanup(func() {
		credentialAbs, credentialMkdirAll = originalAbs, originalMkdir
		credentialLstat, credentialReadFile = originalLstat, originalRead
		credentialMarshal, credentialOpenFile = originalMarshal, originalOpen
		credentialRename, credentialRemove = originalRename, originalRemove
	})
	credentialAbs = func(string) (string, error) { return "", errors.New("absolute") }
	if _, err := New("x"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("absolute path error=%v", err)
	}
	credentialAbs = originalAbs

	path := filepath.Join(t.TempDir(), "nested", "credentials.yaml")
	store, _ := activeStore(t, path)
	credentialMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
	if _, err := store.Modify(context.Background(), "openai", func(*llm.Credential) (*llm.Credential, error) {
		return &llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, nil
	}); err == nil {
		t.Fatal("modify mkdir error missing")
	}
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("write mkdir error missing")
	}
	credentialMkdirAll = originalMkdir

	credentialLstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
	if _, err := store.readDocument(); err == nil {
		t.Fatal("read lstat error missing")
	}
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("write lstat error missing")
	}
	credentialLstat = originalLstat
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\nrecords: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }
	if _, err := store.readDocument(); err == nil {
		t.Fatal("read file error missing")
	}
	credentialReadFile = originalRead
	if err := os.WriteFile(path, []byte("version: 1\nrecords: {}\n---\nversion: 1\nrecords: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readDocument(); err == nil {
		t.Fatal("multiple YAML documents accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("write symlink=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	credentialMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("marshal error missing")
	}
	credentialMarshal = originalMarshal
	large := document{Version: documentVersion, Records: map[string]llm.Credential{"openai": {Kind: llm.CredentialAPIKey, APIKey: strings.Repeat("x", maxDocumentBytes)}}}
	if err := store.writeDocument(large); err == nil {
		t.Fatal("document size error missing")
	}

	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) { return nil, errors.New("open") }
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("temp open error missing")
	}
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) { return &faultCredentialFile{writeN: 1}, nil }
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("short write error missing")
	}
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) {
		return &faultCredentialFile{writeErr: errors.New("write")}, nil
	}
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("write error missing")
	}
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) {
		return &faultCredentialFile{writeN: len("version: 1\nrecords: {}\n"), syncErr: errors.New("sync")}, nil
	}
	credentialMarshal = func(any) ([]byte, error) { return []byte("version: 1\nrecords: {}\n"), nil }
	if err := store.writeDocument(document{}); err == nil {
		t.Fatal("sync error missing")
	}
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) {
		return &faultCredentialFile{writeN: len("version: 1\nrecords: {}\n"), closeErr: errors.New("close")}, nil
	}
	if err := store.writeDocument(document{}); err == nil {
		t.Fatal("close error missing")
	}
	credentialOpenFile = originalOpen
	credentialMarshal = originalMarshal
	credentialRename = func(string, string) error { return errors.New("rename") }
	if err := store.writeDocument(document{Version: documentVersion, Records: map[string]llm.Credential{}}); err == nil {
		t.Fatal("rename error missing")
	}
}

func TestReadErrorsReachResolveModifyAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.yaml")
	store, _ := activeStore(t, path)
	if err := os.WriteFile(path, []byte("bad: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(context.Background(), "openai", "KEY"); err == nil {
		t.Fatal("resolve read error missing")
	}
	if _, err := store.Modify(context.Background(), "openai", func(*llm.Credential) (*llm.Credential, error) { return nil, nil }); err == nil {
		t.Fatal("modify read error missing")
	}
	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("list read error missing")
	}
}

func TestLockCloseCreateAndRetryTickerPaths(t *testing.T) {
	originalOpen, originalWait := credentialOpenFile, credentialLockWait
	t.Cleanup(func() { credentialOpenFile, credentialLockWait = originalOpen, originalWait })
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) {
		return &faultCredentialFile{closeErr: errors.New("close")}, nil
	}
	if err := withLock(context.Background(), filepath.Join(t.TempDir(), "lock"), func() error { return nil }); err == nil {
		t.Fatal("lock close error missing")
	}
	credentialOpenFile = func(string, int, os.FileMode) (credentialFile, error) { return nil, errors.New("create") }
	if err := withLock(context.Background(), filepath.Join(t.TempDir(), "lock"), func() error { return nil }); err == nil {
		t.Fatal("lock create error missing")
	}
	credentialOpenFile = originalOpen
	lock := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	credentialLockWait = time.Second
	timer := time.AfterFunc(35*time.Millisecond, func() { _ = os.Remove(lock) })
	defer timer.Stop()
	if err := withLock(context.Background(), lock, func() error { return nil }); err != nil {
		t.Fatalf("lock retry=%v", err)
	}
}
