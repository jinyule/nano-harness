// Package file provides owner-only YAML settings with polling hot reload.
package file

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

const maxSettingsBytes = 2 << 20

var (
	// ErrInvalidConfig identifies a settings-file configuration the provider cannot honor.
	ErrInvalidConfig = errors.New("invalid settings-file configuration")
	// ErrUnsafeFile identifies a settings document whose type or permissions are unsafe.
	ErrUnsafeFile    = errors.New("unsafe settings file")
	settingsLockWait = 2 * time.Second
	randomRead       = rand.Read
	settingsAbs      = filepath.Abs
	settingsMarshal  = yaml.Marshal
	settingsMkdirAll = os.MkdirAll
	settingsLstat    = os.Lstat
	settingsReadFile = os.ReadFile
	settingsOpenFile = func(path string, flag int, permission os.FileMode) (settingsFile, error) {
		return os.OpenFile(path, flag, permission) //nolint:gosec // callers derive private sibling paths
	}
	settingsRemove = os.Remove
	settingsRename = os.Rename
)

type settingsFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// Config chooses the YAML document and polling cadence.
type Config struct {
	Path         string
	PollInterval time.Duration
}

// Provider implements the settings storage contract and plugin lifecycle.
type Provider struct {
	service *appsettings.Service
	path    string
	poll    time.Duration

	mu       sync.Mutex
	lastHash [sha256.Size]byte
	present  bool
}

// New validates the resolved path without reading it.
func New(service *appsettings.Service, config Config) (*Provider, error) {
	if service == nil || strings.TrimSpace(config.Path) == "" {
		return nil, ErrInvalidConfig
	}
	absolute, err := settingsAbs(config.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve path: %w", ErrInvalidConfig, err)
	}
	poll := config.PollInterval
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	if poll < 10*time.Millisecond || poll > time.Minute {
		return nil, fmt.Errorf("%w: poll interval must be 10ms-1m", ErrInvalidConfig)
	}
	return &Provider{service: service, path: absolute, poll: poll}, nil
}

// ID returns the stable settings-provider plugin identity.
func (*Provider) ID() string { return "settings-file" }

// Path returns the absolute user-editable settings document.
func (provider *Provider) Path() string { return provider.path }

// Start mounts this provider into the settings service.
func (provider *Provider) Start(ctx context.Context, scope *plugin.Scope) error {
	return provider.service.Mount(ctx, provider, scope)
}

// Load reads a strict document; absence means an empty user layer.
func (provider *Provider) Load(ctx context.Context) (appsettings.Document, error) {
	if err := ctx.Err(); err != nil {
		return appsettings.Document{}, err
	}
	document, encoded, present, err := provider.read()
	if err != nil {
		return appsettings.Document{}, err
	}
	provider.remember(encoded, present)
	return document, nil
}

// Persist replaces the document atomically under a cross-process lock.
func (provider *Provider) Persist(ctx context.Context, document appsettings.Document) error {
	if err := document.Validate(); err != nil {
		return err
	}
	encoded, err := settingsMarshal(document)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if len(encoded) > maxSettingsBytes {
		return fmt.Errorf("settings document exceeds %d bytes", maxSettingsBytes)
	}
	if err := settingsMkdirAll(filepath.Dir(provider.path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	return withLock(ctx, provider.path+".lock", func() error {
		if info, err := settingsLstat(provider.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: target is a symbolic link", ErrUnsafeFile)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect settings target: %w", err)
		}
		if err := atomicWrite(provider.path, encoded); err != nil {
			return err
		}
		provider.remember(encoded, true)
		return nil
	})
}

// Watch publishes external edits and keeps running after a rejected edit.
func (provider *Provider) Watch(ctx context.Context, publish func(appsettings.Document, error)) error {
	ticker := time.NewTicker(provider.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			document, encoded, present, err := provider.read()
			if err != nil {
				publish(appsettings.Document{}, err)
				continue
			}
			if provider.changed(encoded, present) {
				provider.remember(encoded, present)
				publish(document, nil)
			}
		}
	}
}

func (provider *Provider) read() (appsettings.Document, []byte, bool, error) {
	info, err := settingsLstat(provider.path)
	if errors.Is(err, os.ErrNotExist) {
		return appsettings.Document{}, nil, false, nil
	}
	if err != nil {
		return appsettings.Document{}, nil, false, fmt.Errorf("inspect settings: %w", err)
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 || info.Size() > maxSettingsBytes {
		return appsettings.Document{}, nil, false, fmt.Errorf("%w: document must be a private regular file within %d bytes", ErrUnsafeFile, maxSettingsBytes)
	}
	encoded, err := settingsReadFile(provider.path)
	if err != nil {
		return appsettings.Document{}, nil, false, fmt.Errorf("read settings: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	var document appsettings.Document
	if err := decoder.Decode(&document); err != nil && !errors.Is(err, io.EOF) {
		return appsettings.Document{}, nil, false, fmt.Errorf("decode settings YAML without exposing source: %T", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return appsettings.Document{}, nil, false, errors.New("settings YAML contains multiple documents")
	}
	if _, err := appsettings.Resolve(document); err != nil {
		return appsettings.Document{}, nil, false, err
	}
	return document, encoded, true, nil
}

func (provider *Provider) remember(encoded []byte, present bool) {
	provider.mu.Lock()
	provider.lastHash = sha256.Sum256(encoded)
	provider.present = present
	provider.mu.Unlock()
}

func (provider *Provider) changed(encoded []byte, present bool) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.present != present || provider.lastHash != sha256.Sum256(encoded)
}

func withLock(ctx context.Context, path string, operation func() error) error {
	deadline := time.NewTimer(settingsLockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		lock, err := settingsOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if closeErr := lock.Close(); closeErr != nil {
				_ = settingsRemove(path)
				return closeErr
			}
			defer func() { _ = settingsRemove(path) }()
			return operation()
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create settings lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("settings writer lock timed out")
		case <-ticker.C:
		}
	}
}

func atomicWrite(path string, encoded []byte) error {
	var random [8]byte
	if _, err := randomRead(random[:]); err != nil {
		return fmt.Errorf("generate settings temp name: %w", err)
	}
	temporary := path + ".tmp-" + hex.EncodeToString(random[:])
	file, err := settingsOpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create settings temp: %w", err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = settingsRemove(temporary)
		}
	}()
	if written, err := file.Write(encoded); err != nil || written != len(encoded) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write settings temp: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync settings temp: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close settings temp: %w", err)
	}
	if err := settingsRename(temporary, path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	cleanup = false
	return nil
}
