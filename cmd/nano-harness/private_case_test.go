package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateLayout returns a resolved base with an existing workspace and a
// separate directory for the private paths a test does not vary.
func privateLayout(t *testing.T, workspace string) (base string, config func() applicationConfig) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(base, workspace), outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return base, func() applicationConfig {
		return applicationConfig{
			workspaceRoot: filepath.Join(base, workspace), sessionRoot: filepath.Join(outside, "sessions"), spillRoot: filepath.Join(outside, "spill"),
			attachmentRoot: filepath.Join(outside, "attachments"), settingsPath: filepath.Join(outside, "settings.yaml"),
			credentialPath: filepath.Join(outside, "credentials.yaml"), skillsDir: filepath.Join(outside, "skills"),
			agentsSkillsDir: filepath.Join(outside, "agents"), sessionID: "session", maxSteps: 1,
		}
	}
}

// sameDirectory reports whether two spellings name one existing directory
// on the file system under test.
func sameDirectory(t *testing.T, left, right string) bool {
	t.Helper()
	leftInfo, err := os.Stat(left)
	if err != nil {
		t.Fatal(err)
	}
	rightInfo, err := os.Stat(right)
	return err == nil && os.SameFile(leftInfo, rightInfo)
}

// TestNormalizeConfig_RejectsCaseAliasesOfTheWorkspace runs on the real
// file system: a case-insensitive one, as macOS uses by default, resolves a
// differently cased spelling to the workspace itself.
func TestNormalizeConfig_RejectsCaseAliasesOfTheWorkspace(t *testing.T) {
	restoreMainHooks(t)
	base, valid := privateLayout(t, filepath.Join("store", "workspace"))
	if !sameDirectory(t, filepath.Join(base, "store"), filepath.Join(base, "STORE")) {
		t.Skip("the temporary file system is case-sensitive, so differently cased spellings are different directories; TestNormalizeConfig_JudgesContainmentByFileIdentity covers the identity logic there")
	}
	for _, test := range []struct {
		name, flag string
		set        func(*applicationConfig)
	}{
		{"credentials", "--credentials", func(c *applicationConfig) {
			c.credentialPath = filepath.Join(base, "Store", "WorkSpace", "credentials.yaml")
		}},
		{"settings", "--settings", func(c *applicationConfig) {
			c.settingsPath = filepath.Join(base, "STORE", "WORKSPACE", "settings.yaml")
		}},
		{"session root inside", "--session-root", func(c *applicationConfig) { c.sessionRoot = filepath.Join(base, "store", "Workspace", "sessions") }},
		{"session root containing", "--session-root", func(c *applicationConfig) { c.sessionRoot = filepath.Join(base, "STORE") }},
		{"aliased workspace", "--credentials", func(c *applicationConfig) {
			c.workspaceRoot = filepath.Join(base, "Store", "WORKSPACE")
			c.credentialPath = filepath.Join(base, "store", "workspace", "credentials.yaml")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid()
			test.set(&config)
			if _, err := normalizeConfig(config); err == nil || !strings.Contains(err.Error(), "choose another "+test.flag) {
				t.Fatalf("normalizeConfig = %v", err)
			}
		})
	}
}

// TestNormalizeConfig_RejectsNormalizationAliasesOfTheWorkspace covers a
// file system that treats NFC and NFD spellings as one name, as APFS does.
func TestNormalizeConfig_RejectsNormalizationAliasesOfTheWorkspace(t *testing.T) {
	restoreMainHooks(t)
	const composed, decomposed = "café", "café"
	base, valid := privateLayout(t, composed)
	if !sameDirectory(t, filepath.Join(base, composed), filepath.Join(base, decomposed)) {
		t.Skip("the temporary file system distinguishes NFC from NFD names, so they are different directories")
	}
	config := valid()
	config.credentialPath = filepath.Join(base, decomposed, "credentials.yaml")
	if _, err := normalizeConfig(config); err == nil || !strings.Contains(err.Error(), "choose another --credentials") {
		t.Fatalf("normalizeConfig = %v", err)
	}
}

// TestNormalizeConfig_RejectsFirmlinkAliasesOfTheWorkspace covers macOS
// firmlinks: /System/Volumes/Data/<path> names the same directory as <path>
// on the data volume, and link resolution keeps either spelling.
func TestNormalizeConfig_RejectsFirmlinkAliasesOfTheWorkspace(t *testing.T) {
	restoreMainHooks(t)
	base, valid := privateLayout(t, "workspace")
	firmlinked := filepath.Join("/System/Volumes/Data", base)
	if info, err := os.Stat(firmlinked); err != nil || !info.IsDir() || !sameDirectory(t, base, firmlinked) {
		t.Skip("no macOS /System/Volumes/Data firmlink reaches the temporary directory on this platform")
	}
	for _, test := range []struct {
		name, flag string
		set        func(*applicationConfig)
	}{
		{"credentials through the firmlink", "--credentials", func(c *applicationConfig) {
			c.credentialPath = filepath.Join(firmlinked, "workspace", "credentials.yaml")
		}},
		{"workspace through the firmlink", "--settings", func(c *applicationConfig) {
			c.workspaceRoot = filepath.Join(firmlinked, "workspace")
			c.settingsPath = filepath.Join(base, "workspace", "settings.yaml")
		}},
		{"session root containing through the firmlink", "--session-root", func(c *applicationConfig) { c.sessionRoot = firmlinked }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid()
			test.set(&config)
			if _, err := normalizeConfig(config); err == nil || !strings.Contains(err.Error(), "choose another "+test.flag) {
				t.Fatalf("normalizeConfig = %v", err)
			}
		})
	}
}

// TestNormalizeConfig_JudgesContainmentByFileIdentity injects a
// case-insensitive identity, so the comparison of existing ancestors runs on
// every platform, including spellings link resolution did not canonicalize.
func TestNormalizeConfig_JudgesContainmentByFileIdentity(t *testing.T) {
	restoreMainHooks(t)
	base, valid := privateLayout(t, filepath.Join("store", "workspace"))
	failure := errors.New("identify failure")
	identifyPath = func(path string) (fs.FileInfo, error) {
		if strings.HasPrefix(path, filepath.Join(base, "blocked")) {
			return nil, failure
		}
		if relative, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(relative, "..") {
			path = filepath.Join(base, strings.ToLower(relative))
		}
		return os.Stat(path)
	}
	for _, test := range []struct {
		name, flag string
		set        func(*applicationConfig)
		allowed    bool
	}{
		{"file below an aliased workspace", "--credentials", func(c *applicationConfig) {
			c.credentialPath = filepath.Join(base, "Store", "WorkSpace", "nested", "credentials.yaml")
		}, false},
		{"store root that is the workspace", "--session-root", func(c *applicationConfig) { c.sessionRoot = filepath.Join(base, "STORE", "WORKSPACE") }, false},
		{"store root containing the workspace", "--spill-root", func(c *applicationConfig) { c.spillRoot = filepath.Join(base, "Store") }, false},
		{"missing store root beside the workspace", "", func(c *applicationConfig) { c.attachmentRoot = filepath.Join(base, "Store", "attachments") }, true},
		{"file in a sibling with a shared prefix", "", func(c *applicationConfig) {
			c.settingsPath = filepath.Join(base, "Store", "WorkSpace-settings", "settings.yaml")
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid()
			test.set(&config)
			_, err := normalizeConfig(config)
			if test.allowed != (err == nil) || !test.allowed && !strings.Contains(err.Error(), "choose another "+test.flag) {
				t.Fatalf("normalizeConfig = %v", err)
			}
		})
	}
	config := valid()
	config.credentialPath = filepath.Join(base, "blocked", "credentials.yaml")
	if _, err := normalizeConfig(config); !errors.Is(err, failure) || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("identity failure = %v", err)
	}
	identifyPath = func(path string) (fs.FileInfo, error) {
		if path == valid().workspaceRoot {
			return nil, failure
		}
		return os.Stat(path)
	}
	if _, err := normalizeConfig(valid()); !errors.Is(err, failure) {
		t.Fatalf("workspace identity failure = %v", err)
	}
}
