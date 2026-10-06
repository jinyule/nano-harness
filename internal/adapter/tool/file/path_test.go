package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/adapter/tool/search"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	"github.com/jinyule/nano-harness/internal/platform/process"
)

func TestFileTools_PhysicalParentTraversalMatrix(t *testing.T) {
	for _, tool := range []string{"read", "write", "edit", "glob", "grep", "read_image"} {
		t.Run(tool, func(t *testing.T) {
			for _, test := range []struct {
				name, prefix string
				physical     bool
				denied       bool
				symlink      bool
			}{
				{name: "root"},
				{name: "real parent", prefix: "a/nested/../", physical: true},
				{name: "linked parent", prefix: "alias/../", physical: true, symlink: true},
				{name: "missing parent", prefix: "missing/../", denied: true},
				{name: "file parent", prefix: "picked.txt/../", denied: true},
				{name: "physical escape", prefix: "rootlink/../", denied: true, symlink: true},
				{name: "outside link", prefix: "escape/../", denied: true, symlink: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					h := newHarness(t)
					writeFixture(t, h.path("picked.txt"), "root before")
					writeFixture(t, h.path("a", "picked.txt"), "physical before")
					writeFixture(t, h.path("picked.png"), pngBytes+"root")
					writeFixture(t, h.path("a", "picked.png"), pngBytes+"physical")
					if err := os.Mkdir(h.path("a", "nested"), 0o700); err != nil {
						t.Fatal(err)
					}
					for link, target := range map[string]string{"alias": h.path("a", "nested"), "rootlink": h.root.Path(), "escape": t.TempDir()} {
						if err := os.Symlink(target, h.path(link)); err != nil {
							t.Fatal(err)
						}
					}
					if tool == "glob" || tool == "grep" {
						provider, err := search.New(h.runtime, process.New(), h.root)
						if err != nil {
							t.Fatal(err)
						}
						scope := &plugin.Scope{}
						t.Cleanup(func() { _ = scope.Close(context.Background()) })
						if err := provider.Start(context.Background(), scope); err != nil {
							t.Fatal(err)
						}
					}
					arguments := map[string]any{"file_path": test.prefix + "picked.txt"}
					if tool == "write" || tool == "edit" {
						h.read(t, "picked.txt")
						h.read(t, "a/picked.txt")
						arguments["content"] = "after"
						if tool == "edit" {
							delete(arguments, "content")
							arguments["old_string"], arguments["new_string"] = "before", "after"
						}
					}
					if tool == "glob" {
						arguments = map[string]any{"path": test.prefix + ".", "pattern": "picked.txt"}
					}
					if tool == "grep" {
						arguments = map[string]any{"path": test.prefix + "picked.txt", "pattern": "before"}
					}
					var result session.ToolResult
					if tool == "read_image" {
						result = h.readImage(t, visionRoute, test.prefix+"picked.png")[0]
					} else {
						result = h.call(t, tool, arguments)
					}
					denied := test.denied || test.symlink && (tool == "write" || tool == "edit")
					if result.IsError != denied {
						t.Fatalf("%s(%s) = %s; want denied=%v", tool, test.prefix, result.Output, denied)
					}
					if denied {
						if len(h.approver.reasons) != 0 || readFixture(t, h.path("picked.txt")) != "root before" || readFixture(t, h.path("a", "picked.txt")) != "physical before" || h.images.calls() != 0 {
							t.Error("denied path reached approval, attachment storage, or mutation")
						}
						return
					}
					identity, target, untouched := "root", h.path("picked.txt"), h.path("a", "picked.txt")
					if test.physical {
						identity, target, untouched = "physical", untouched, target
					}
					switch tool {
					case "read", "grep":
						if !strings.Contains(result.Output, identity+" before") {
							t.Errorf("wrong file identity: %s", result.Output)
						}
					case "glob":
						if test.physical && result.Output != "a/picked.txt" {
							t.Errorf("wrong search root: %s", result.Output)
						}
					case "write", "edit":
						want := "after"
						if tool == "edit" {
							want = identity + " after"
						}
						if readFixture(t, target) != want || !strings.HasSuffix(readFixture(t, untouched), " before") {
							t.Error("mutation changed the wrong file")
						}
					case "read_image":
						data := pngBytes + identity
						if h.images.data[len(h.images.data)-1] != data {
							t.Error("stored the wrong source image")
						}
						imagePath := h.path("picked.png")
						if test.physical {
							imagePath = h.path("a", "picked.png")
						}
						if prior, ok := h.provider.observed.lookup("session", imagePath); !ok || !prior.present || prior.version != digest([]byte(data)) {
							t.Errorf("image observation lost its physical identity: %+v, %v", prior, ok)
						}
						if result.Image == nil || result.Image.ID == "" || result.Image.Name != "picked.png" || result.Image.Bytes != 4 || !strings.Contains(result.Output, "<path>"+imagePath+"</path>") {
							t.Errorf("image reference or display path = %+v", result)
						}
					}
				})
			}
		})
	}
}

func TestFileTools_MissingPhysicalTargetKeepsObservationIdentity(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.path("a", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := "a/nested/../missing.txt"
	result := h.call(t, "read", map[string]any{"file_path": path})
	want := fmt.Sprintf("Error: cannot read %q: not found", h.path("a", "missing.txt"))
	if !result.IsError || result.Output != want {
		t.Errorf("missing read = %q, want %q", result.Output, want)
	}
	prior, ok := h.provider.observed.lookup("session", h.path("a", "missing.txt"))
	if !ok || prior.present {
		t.Errorf("missing target lost its physical observation identity: %+v, %v", prior, ok)
	}
	result = h.call(t, "edit", map[string]any{"file_path": path, "old_string": "before", "new_string": "after"})
	want = fmt.Sprintf("Error: cannot edit %q: not found", h.path("a", "missing.txt"))
	if !result.IsError || result.Output != want || len(h.approver.reasons) != 0 {
		t.Errorf("edit after missing read = %q, want %q, approvals=%v", result.Output, want, h.approver.reasons)
	}
}

func TestFileTools_PhysicalPathsAreRecheckedAtExecution(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("picked.txt"), "before")
	if err := os.Mkdir(h.path("nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.read(t, "picked.txt")
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			h.approver.during = func() {
				if err := os.Remove(h.path("nested")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(h.root.Path(), h.path("nested")); err != nil {
					t.Fatal(err)
				}
			}
			arguments := map[string]any{"file_path": "nested/../picked.txt", "content": "after"}
			if tool == "edit" {
				delete(arguments, "content")
				arguments["old_string"], arguments["new_string"] = "before", "after"
			}
			result := h.call(t, tool, arguments)
			if !result.IsError || !strings.Contains(result.Output, "path crosses a symbolic link") || readFixture(t, h.path("picked.txt")) != "before" {
				t.Fatalf("approval-time replacement = %+v", result)
			}
			if err := os.Remove(h.path("nested")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(h.path("nested"), 0o700); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Calling the execution function directly cannot bypass missing-directory denial.
	invocation := appTool.Invocation{SessionID: "session", Approved: true}
	if _, err := h.provider.write(context.Background(), invocation, writeArgs{FilePath: "missing/../picked.txt", Content: "after"}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("direct write = %v", err)
	}
	if _, err := h.provider.edit(context.Background(), invocation, editArgs{FilePath: "missing/../picked.txt", OldString: "before", NewString: "after"}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("direct edit = %v", err)
	}
	if readFixture(t, h.path("picked.txt")) != "before" {
		t.Fatal("direct execution mutated a denied target")
	}
	if len(h.approver.reasons) != 2 {
		t.Fatalf("approval requests = %v, want one for each replaced directory", h.approver.reasons)
	}
}
