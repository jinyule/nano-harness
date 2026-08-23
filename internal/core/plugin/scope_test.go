package plugin

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestScope_Close(t *testing.T) {
	t.Parallel()

	firstError := errors.New("first failed")
	secondError := errors.New("second failed")
	var order []string
	scope := &Scope{}
	if err := scope.Defer(func(context.Context) error {
		order = append(order, "first")
		return firstError
	}); err != nil {
		t.Fatalf("Defer(first) error = %v", err)
	}
	if err := scope.Defer(func(context.Context) error {
		order = append(order, "second")
		return secondError
	}); err != nil {
		t.Fatalf("Defer(second) error = %v", err)
	}

	err := scope.Close(context.Background())
	if !errors.Is(err, firstError) || !errors.Is(err, secondError) {
		t.Fatalf("Close() error = %v, want both cleanup errors", err)
	}
	if want := []string{"second", "first"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := scope.Defer(func(context.Context) error { return nil }); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("Defer() after close error = %v, want ErrScopeClosed", err)
	}
}

func TestScope_DeferRejectsNil(t *testing.T) {
	t.Parallel()

	if err := (&Scope{}).Defer(nil); !errors.Is(err, ErrNilCleanup) {
		t.Fatalf("Defer(nil) error = %v, want ErrNilCleanup", err)
	}
}

func TestScope_CloseWithoutCleanups(t *testing.T) {
	t.Parallel()

	if err := (&Scope{}).Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
