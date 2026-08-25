// Package file stores provider credentials in an owner-only YAML document.
package file

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

const (
	documentVersion  = 1
	maxDocumentBytes = 2 << 20
)

var (
	credentialLockWait = 30 * time.Second
	randomRead         = rand.Read
	credentialAbs      = filepath.Abs
	credentialMkdirAll = os.MkdirAll
	credentialLstat    = os.Lstat
	credentialReadFile = os.ReadFile
	credentialMarshal  = yaml.Marshal
	credentialOpenFile = func(path string, flag int, permission os.FileMode) (credentialFile, error) {
		return os.OpenFile(path, flag, permission) //nolint:gosec // callers derive private sibling paths
	}
	credentialRename = os.Rename
	credentialRemove = os.Remove
)

type credentialFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

var (
	// ErrInvalidConfig identifies a credential-file configuration the store cannot honor.
	ErrInvalidConfig = errors.New("invalid credential-file configuration")
	// ErrUnsafeFile identifies a credential document whose type or permissions are unsafe.
	ErrUnsafeFile = errors.New("unsafe credential file")
)

type document struct {
	Version int                       `yaml:"version"`
	Records map[string]llm.Credential `yaml:"records"`
}

// Store is both the credential service provider and its lifecycle plugin.
type Store struct {
	path string

	mu      sync.RWMutex
	writeMu sync.Mutex
	started bool
	active  bool
	lookup  func(string) (string, bool)
}

// New validates the absolute store path without reading secrets.
func New(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrInvalidConfig
	}
	absolute, err := credentialAbs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve path", ErrInvalidConfig)
	}
	return &Store{path: absolute, lookup: os.LookupEnv}, nil
}

// ID returns the stable credential-provider plugin identity.
func (*Store) ID() string { return "credentials-file" }

// Path returns the resolved document path without exposing its contents.
func (store *Store) Path() string { return store.path }

// Start validates the existing document and registers contribution removal.
func (store *Store) Start(_ context.Context, scope *plugin.Scope) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.started {
		return ErrInvalidConfig
	}
	if _, err := store.readDocument(); err != nil {
		return err
	}
	if err := scope.Defer(func(context.Context) error {
		store.mu.Lock()
		store.active = false
		store.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	store.started, store.active = true, true
	return nil
}

// Resolve reads a stored provider record or the provider's configured environment reference.
func (store *Store) Resolve(ctx context.Context, provider, environment string) (llm.Credential, error) {
	if err := ctx.Err(); err != nil {
		return llm.Credential{}, err
	}
	if err := store.ensureActive(); err != nil {
		return llm.Credential{}, err
	}
	document, err := store.readDocument()
	if err != nil {
		return llm.Credential{}, err
	}
	if credential, exists := document.Records[provider]; exists {
		return cloneCredential(credential), nil
	}
	if value, exists := store.lookup(environment); exists && value != "" {
		credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: value}
		if err := llm.ValidateCredential(credential); err != nil {
			return llm.Credential{}, err
		}
		return credential, nil
	}
	return llm.Credential{}, fmt.Errorf("%w: %s", llm.ErrNoCredential, provider)
}

// Modify holds both an in-process and cross-process lock around read-decide-write.
func (store *Store) Modify(ctx context.Context, provider string, mutate func(*llm.Credential) (*llm.Credential, error)) (llm.Credential, error) {
	if !validProvider(provider) || mutate == nil {
		return llm.Credential{}, ErrInvalidConfig
	}
	if err := store.ensureActive(); err != nil {
		return llm.Credential{}, err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if err := credentialMkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return llm.Credential{}, fmt.Errorf("create credential directory: %w", err)
	}
	var committed llm.Credential
	err := withLock(ctx, store.path+".lock", func() error {
		doc, err := store.readDocument()
		if err != nil {
			return err
		}
		var current *llm.Credential
		if value, exists := doc.Records[provider]; exists {
			copyValue := cloneCredential(value)
			current = &copyValue
		}
		next, err := mutate(current)
		if err != nil {
			return err
		}
		if next == nil {
			delete(doc.Records, provider)
			committed = llm.Credential{}
		} else {
			if err := llm.ValidateCredential(*next); err != nil {
				return err
			}
			committed = cloneCredential(*next)
			doc.Records[provider] = committed
		}
		return store.writeDocument(doc)
	})
	return committed, err
}

// Delete removes one stored record. Environment fallback remains visible.
func (store *Store) Delete(ctx context.Context, provider string) error {
	_, err := store.Modify(ctx, provider, func(*llm.Credential) (*llm.Credential, error) { return nil, nil })
	return err
}

// List returns secret-free stored account metadata.
func (store *Store) List(ctx context.Context) ([]llm.AccountInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.ensureActive(); err != nil {
		return nil, err
	}
	doc, err := store.readDocument()
	if err != nil {
		return nil, err
	}
	accounts := make([]llm.AccountInfo, 0, len(doc.Records))
	for provider, credential := range doc.Records {
		accounts = append(accounts, llm.AccountInfo{Provider: provider, Kind: credential.Kind, Source: "file"})
	}
	sort.Slice(accounts, func(left, right int) bool { return accounts[left].Provider < accounts[right].Provider })
	return accounts, nil
}

func (store *Store) ensureActive() error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if !store.active {
		return llm.ErrNotRunning
	}
	return nil
}

func (store *Store) readDocument() (document, error) {
	info, err := credentialLstat(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return document{Version: documentVersion, Records: map[string]llm.Credential{}}, nil
	}
	if err != nil {
		return document{}, fmt.Errorf("inspect credential document: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxDocumentBytes || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return document{}, fmt.Errorf("%w: document must be a private regular file within %d bytes", ErrUnsafeFile, maxDocumentBytes)
	}
	encoded, err := credentialReadFile(store.path)
	if err != nil {
		return document{}, fmt.Errorf("read credential document: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var parsed document
	if err := decoder.Decode(&parsed); err != nil {
		return document{}, fmt.Errorf("decode credential YAML without exposing source: %T", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return document{}, errors.New("credential YAML contains multiple documents")
	}
	if parsed.Version != documentVersion || parsed.Records == nil {
		return document{}, errors.New("credential document has an unsupported shape")
	}
	for provider, credential := range parsed.Records {
		if !validProvider(provider) || llm.ValidateCredential(credential) != nil {
			return document{}, fmt.Errorf("credential record %q is invalid", provider)
		}
		parsed.Records[provider] = cloneCredential(credential)
	}
	return parsed, nil
}

func (store *Store) writeDocument(document document) error {
	if err := credentialMkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	if info, err := credentialLstat(store.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: target is a symbolic link", ErrUnsafeFile)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect credential target: %w", err)
	}
	encoded, err := credentialMarshal(document)
	if err != nil {
		return fmt.Errorf("encode credential document: %w", err)
	}
	if len(encoded) > maxDocumentBytes {
		return errors.New("credential document exceeds size limit")
	}
	var random [8]byte
	if _, err := randomRead(random[:]); err != nil {
		return fmt.Errorf("generate credential temp name: %w", err)
	}
	temporary := store.path + ".tmp-" + hex.EncodeToString(random[:])
	file, err := credentialOpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create credential temp: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = credentialRemove(temporary)
		}
	}()
	if written, err := file.Write(encoded); err != nil || written != len(encoded) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write credential temp: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync credential temp: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close credential temp: %w", err)
	}
	if err := credentialRename(temporary, store.path); err != nil {
		return fmt.Errorf("replace credential document: %w", err)
	}
	remove = false
	return nil
}

func withLock(ctx context.Context, path string, operation func() error) error {
	deadline := time.NewTimer(credentialLockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		file, err := credentialOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if closeErr := file.Close(); closeErr != nil {
				_ = credentialRemove(path)
				return closeErr
			}
			defer func() { _ = credentialRemove(path) }()
			return operation()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create credential lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("credential writer lock timed out")
		case <-ticker.C:
		}
	}
}

func cloneCredential(credential llm.Credential) llm.Credential {
	extra := credential.Extra
	credential.Extra = make(map[string]string, len(extra))
	maps.Copy(credential.Extra, extra)
	return credential
}

func validProvider(provider string) bool {
	return provider == "openai" || provider == "anthropic" || provider == "openrouter"
}
