package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

const validPatch = "--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n"

func TestPatchTool_MetadataAndValidation(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := &Provider{root: root}
	tool := patchTool{owner: owner}
	if tool.Definition().Name != "apply_patch" || tool.Concurrency() != appTool.ConcurrencyExclusive || !strings.Contains(tool.ApprovalReason(nil), "writes") {
		t.Fatal("patch tool metadata is invalid")
	}
	if err := owner.validatePatch(validPatch); err != nil {
		t.Fatalf("valid patch error = %v", err)
	}
	newFilePatch := "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+new\n"
	if err := owner.validatePatch(newFilePatch); err != nil {
		t.Fatalf("new-file patch error = %v", err)
	}

	for _, patch := range []string{
		"GIT binary patch\n--- a/a\n+++ b/a", "Binary files a and b differ\n--- a/a\n+++ b/a",
		"rename from a\nrename to b\n--- a/a\n+++ b/b", "copy from a\ncopy to b\n--- a/a\n+++ b/b",
		"old mode 100644\nnew mode 120000\n--- a/a\n+++ b/a", "no headers",
		"--- a/\"quoted\"\n+++ b/file", "--- a/../../escape\n+++ b/../../escape",
	} {
		if err := owner.validatePatch(patch); err == nil {
			t.Fatalf("accepted unsafe patch %q", patch)
		}
	}

	linkTarget := testRoot(t)
	if err := os.Symlink(linkTarget, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	linkPatch := "--- /dev/null\n+++ b/link/file\n@@ -0,0 +1 @@\n+data\n"
	if err := owner.validatePatch(linkPatch); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink error = %v", err)
	}

	workspaceLstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
	if err := owner.validatePatch(validPatch); err == nil || err.Error() != "lstat" {
		t.Fatalf("lstat error = %v", err)
	}
}

func TestPatchTool_RequiresApprovalAndRunsCheckBeforeApply(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	runner := &fakeRunner{}
	tool := patchTool{owner: &Provider{root: root, temp: filepath.Join(root, "tmp"), git: "/usr/bin/git", runner: runner}}

	if _, err := tool.Execute(context.Background(), execution(`{"patch":"x"}`)); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("approval error = %v", err)
	}
	for _, raw := range []string{`{`, `{"patch":""}`, `{"patch":"` + strings.Repeat("x", (2<<20)+1) + `"}`} {
		if _, err := tool.Execute(context.Background(), appTool.Execution{Arguments: json.RawMessage(raw), Elevated: true}); err == nil {
			t.Fatalf("accepted %d-byte arguments", len(raw))
		}
	}
	tool.owner.git = ""
	if _, err := tool.Execute(context.Background(), appTool.Execution{Arguments: json.RawMessage(`{"patch":"x"}`), Elevated: true}); err == nil || !strings.Contains(err.Error(), "git executable") {
		t.Fatalf("git error = %v", err)
	}
	tool.owner.git = "/usr/bin/git"
	if _, err := tool.Execute(context.Background(), appTool.Execution{Arguments: patchArguments("no headers"), Elevated: true}); err == nil || !strings.Contains(err.Error(), "no file headers") {
		t.Fatalf("patch validation error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner.results = []runnerResult{{result: platformProcess.Result{Output: "bad check"}, err: errors.New("check")}}
	if _, err := tool.Execute(context.Background(), appTool.Execution{Arguments: patchArguments(validPatch), Elevated: true}); err == nil || !strings.Contains(err.Error(), "patch check failed") || !strings.Contains(err.Error(), "bad check") {
		t.Fatalf("check error = %v", err)
	}
	runner.results = []runnerResult{{}, {result: platformProcess.Result{Output: "bad apply"}, err: errors.New("apply")}}
	if _, err := tool.Execute(context.Background(), appTool.Execution{Arguments: patchArguments(validPatch), Elevated: true}); err == nil || !strings.Contains(err.Error(), "patch apply failed") || !strings.Contains(err.Error(), "bad apply") {
		t.Fatalf("apply error = %v", err)
	}
	runner.results = []runnerResult{{}, {}}
	output, err := tool.Execute(context.Background(), appTool.Execution{Arguments: patchArguments(validPatch), Elevated: true})
	if err != nil || output != "patch applied" {
		t.Fatalf("success output = %q, error = %v", output, err)
	}
	if len(runner.requests) < 5 {
		t.Fatalf("requests = %#v", runner.requests)
	}
	check := runner.requests[len(runner.requests)-2]
	apply := runner.requests[len(runner.requests)-1]
	if check.Mode != platformProcess.ModeWorkspace || check.Timeout != time.Minute || check.Args[len(check.Args)-1] != "--check" || apply.Args[len(apply.Args)-1] == "--check" || string(apply.Stdin) != validPatch {
		t.Fatalf("check = %#v, apply = %#v", check, apply)
	}
}

func patchArguments(patch string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]string{"patch": patch})
	return encoded
}

func TestShellTool_MetadataApprovalModesAndResults(t *testing.T) {
	root := testRoot(t)
	runner := &fakeRunner{}
	tool := shellTool{owner: &Provider{root: root, temp: filepath.Join(root, "tmp"), runner: runner}}
	if tool.Definition().Name != "run_shell" || tool.Concurrency() != appTool.ConcurrencyExclusive {
		t.Fatal("shell tool metadata is invalid")
	}
	if reason := tool.ApprovalReason(json.RawMessage(`{"host":true}`)); !strings.Contains(reason, "host filesystem") {
		t.Fatalf("host approval = %q", reason)
	}
	if reason := tool.ApprovalReason(json.RawMessage(`{`)); !strings.Contains(reason, "workspace") {
		t.Fatalf("default approval = %q", reason)
	}

	for _, candidate := range []appTool.Execution{
		{Arguments: json.RawMessage(`{`), Elevated: true},
		{Arguments: json.RawMessage(`{"command":""}`), Elevated: true},
		{Arguments: shellArguments(strings.Repeat("x", (128<<10)+1), 0, false), Elevated: true},
		{Arguments: shellArguments("true", 99, false), Elevated: true},
		{Arguments: shellArguments("true", 600001, false), Elevated: true},
		{Arguments: shellArguments("true", 0, false)},
		{Arguments: shellArguments("true", 0, true), Elevated: true, Delegated: true},
	} {
		if _, err := tool.Execute(context.Background(), candidate); err == nil {
			t.Fatalf("accepted execution %+v", candidate)
		}
	}

	runner.results = []runnerResult{{}}
	output, err := tool.Execute(context.Background(), appTool.Execution{Arguments: shellArguments("true", 0, false), Elevated: true})
	if err != nil || output != "command completed with no output" {
		t.Fatalf("empty output = %q, error = %v", output, err)
	}
	request := runner.requests[len(runner.requests)-1]
	if request.Mode != platformProcess.ModeWorkspace || request.Timeout != 2*time.Minute || request.Path != "/bin/sh" {
		t.Fatalf("workspace request = %#v", request)
	}

	runner.results = []runnerResult{{result: platformProcess.Result{Output: "host output"}}}
	output, err = tool.Execute(context.Background(), appTool.Execution{Arguments: shellArguments("printf", 250, true), Elevated: true})
	if err != nil || output != "host output" {
		t.Fatalf("host output = %q, error = %v", output, err)
	}
	request = runner.requests[len(runner.requests)-1]
	if request.Mode != platformProcess.ModeHost || request.Timeout != 250*time.Millisecond || request.Args[1] != "printf" {
		t.Fatalf("host request = %#v", request)
	}

	runner.results = []runnerResult{{result: platformProcess.Result{Output: "partial"}, err: errors.New("exit")}}
	if _, err := tool.Execute(context.Background(), appTool.Execution{Arguments: shellArguments("false", 0, false), Elevated: true}); err == nil || !strings.Contains(err.Error(), "exit") || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("runner error = %v", err)
	}
}

func shellArguments(command string, timeout int64, host bool) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"command": command, "timeout_ms": timeout, "host": host})
	return encoded
}
