package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestModelSend_StoresAttachmentsBeforeSubmitting(t *testing.T) {
	fixture, current := modelFixture(t)
	submittedAtCommit := -1
	fixture.images.onCommit = func() { submittedAtCommit = len(fixture.controller.submitted) }
	current.images = []pendingImage{{ref: session.Image{ID: "sha256:a", Name: "one.png"}, data: []byte("jpeg")}}
	current.input.SetValue("inspect")
	next, command := current.submit()
	current = next.(model)
	if result := command().(turnMessage).result; result.Err != nil {
		t.Fatal(result.Err)
	}
	if submittedAtCommit != 0 || len(fixture.images.committed) != 1 || fixture.images.committed[0].Name != "one.png" || len(fixture.controller.submitted) != 1 {
		t.Fatalf("commit before submit: at=%d committed=%v submitted=%d", submittedAtCommit, fixture.images.committed, len(fixture.controller.submitted))
	}

	// A failed store keeps the message from being submitted.
	fixture.images.commitErr = errors.New("disk full")
	current.images = []pendingImage{{ref: session.Image{Name: "two.png"}}}
	current.input.SetValue("again")
	next, command = current.submit()
	current = next.(model)
	result := command().(turnMessage).result
	if !errors.Is(result.Err, fixture.images.commitErr) || !strings.Contains(result.Err.Error(), "store attachment two.png") || len(fixture.controller.submitted) != 1 {
		t.Fatalf("commit failure = %+v submitted=%d", result, len(fixture.controller.submitted))
	}
	if steerResult := current.deliverCommand(session.Message{}, []pendingImage{{ref: session.Image{Name: "three.png"}}})(); steerResult.(operationMessage).err == nil || len(fixture.controller.steered) != 0 {
		t.Fatalf("deliver commit failure = %+v steered=%d", steerResult, len(fixture.controller.steered))
	}
	fixture.images.commitErr = nil
	fixture.controller.steerErr = agent.ErrAgentIdle
	if delivered := current.deliverCommand(session.Message{}, []pendingImage{{ref: session.Image{Name: "four.png"}}})(); delivered.(turnMessage).result.Err != nil || len(fixture.images.committed) != 2 {
		t.Fatalf("idle delivery = %+v committed=%v", delivered, fixture.images.committed)
	}
}

func TestModel_ReportsEachUnavailableImageOnce(t *testing.T) {
	_, current := modelFixture(t)
	image := session.Image{ID: "sha256:0123456789abcdef0123", Name: "shot.png"}
	current, command := update(t, current, unavailableImageMessage{image: image, missing: true})
	current, _ = update(t, current, unavailableImageMessage{image: image})
	other := session.Image{ID: "sha256:fedcba9876543210", Name: "old.png"}
	current, _ = update(t, current, unavailableImageMessage{image: other})
	joined := strings.Join(current.lines, "\n")
	if command == nil || strings.Count(joined, "shot.png") != 1 ||
		!strings.Contains(joined, "attachment> image shot.png (sha256:0123456789ab) is missing from the attachment store; the model sees a placeholder instead") ||
		!strings.Contains(joined, "attachment> image old.png (sha256:fedcba987654) failed verification in the attachment store") {
		t.Fatalf("lines = %s", joined)
	}
}

func TestAppImageUnavailable_NeverBlocksTheReader(t *testing.T) {
	fixture, config := newAppFixture()
	app, _ := New(config)
	scope := &plugin.Scope{}
	if err := app.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	image := session.Image{ID: "sha256:1", Name: "shot.png"}
	fixture.images.observer(image, session.ErrAttachmentMissing)
	if notice := (<-app.events).(unavailableImageMessage); notice.image != image || !notice.missing {
		t.Fatalf("notice = %+v", notice)
	}
	for range cap(app.events) {
		app.events <- tea.KeyPressMsg{}
	}
	fixture.images.observer(image, session.ErrAttachmentCorrupt) // dropped, not blocked
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	app.imageUnavailable(image, session.ErrAttachmentCorrupt) // stopped
	if fixture.images.observer != nil {
		t.Fatal("observer outlived the scope")
	}
}
