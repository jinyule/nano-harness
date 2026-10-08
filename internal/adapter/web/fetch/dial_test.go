package fetch

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// The timer is a deadlock watchdog, never the source of test ordering.
func receiveDial[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	select {
	case value := <-channel:
		return value
	case <-watchdog.C:
		t.Fatal("dial barrier was not reached")
		var zero T
		return zero
	}
}

type trackedConnection struct {
	net.Conn
	closed atomic.Bool
}

func (connection *trackedConnection) Close() error {
	connection.closed.Store(true)
	return connection.Conn.Close()
}

func trackedPipe(t *testing.T) *trackedConnection {
	t.Helper()
	connection, peer := net.Pipe()
	tracked := &trackedConnection{Conn: connection}
	t.Cleanup(func() {
		_ = tracked.Close()
		_ = peer.Close()
	})
	return tracked
}

func TestDialPinned_InterleavesAndJoinsLateConnections(t *testing.T) {
	for _, cancelDuringJoin := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cancel-during-join"}[cancelDuringJoin], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			addresses := []netip.Addr{netip.MustParseAddr("2606:4700::1111"), netip.MustParseAddr("2606:4700::2222"), netip.MustParseAddr(publicIP)}
			winner, late := trackedPipe(t), trackedPipe(t)
			entered := make(chan string, 2)
			cancelled, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			ticks := make(chan time.Time)
			client := New(Config{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" {
					t.Errorf("network=%q", network)
				}
				entered <- address
				if address == "[2606:4700::1111]:443" {
					<-ctx.Done()
					close(cancelled)
					<-release
					close(returned)
					return late, nil
				}
				return winner, nil
			}})
			result := make(chan dialResult, 1)
			ownerDone := make(chan struct{})
			go func() {
				defer close(ownerDone)
				connection, err := client.dialPinned(ctx, "tcp", addresses, 443, ticks)
				result <- dialResult{connection, err}
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-release:
				default:
					close(release)
				}
				// On an assertion failure, still join the owner.
				if result != nil {
					<-result
				}
				<-ownerDone
			})
			if address := receiveDial(t, entered); address != "[2606:4700::1111]:443" {
				t.Fatalf("first=%q", address)
			}
			ticks <- time.Time{}
			if address := receiveDial(t, entered); address != publicIP+":443" {
				t.Fatalf("second=%q, want other family", address)
			}
			receiveDial(t, cancelled)
			select {
			case <-result:
				result = nil
				t.Fatal("returned while a losing dial was still running")
			default:
			}
			if cancelDuringJoin {
				cancel()
			}
			close(release)
			got := receiveDial(t, result)
			result = nil
			if cancelDuringJoin {
				if got.connection != nil || !errors.Is(got.err, context.Canceled) || !winner.closed.Load() {
					t.Fatalf("cancelled winner=%v error=%v closed=%v", got.connection, got.err, winner.closed.Load())
				}
			} else if got.connection != winner || got.err != nil || winner.closed.Load() {
				t.Fatalf("winner=%v err=%v closed=%v", got.connection, got.err, winner.closed.Load())
			}
			if !late.closed.Load() {
				t.Fatal("late successful connection leaked")
			}
			<-returned
		})
	}
}

func TestDialPinned_CancellationWaitsForEveryAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addresses := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(publicIP)}
	entered, cancelled := make(chan struct{}, 2), make(chan struct{}, 2)
	release := make(chan struct{})
	var exited atomic.Int32
	client := New(Config{Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		entered <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release
		exited.Add(1)
		return nil, ctx.Err()
	}})
	ticks := make(chan time.Time)
	result := make(chan error, 1)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, err := client.dialPinned(ctx, "tcp", addresses, 80, ticks)
		result <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		if result != nil {
			<-result
		}
		<-ownerDone
	})
	receiveDial(t, entered)
	ticks <- time.Time{}
	receiveDial(t, entered)
	// A tick after the final candidate must not start another connection.
	ticks <- time.Time{}
	cancel()
	receiveDial(t, cancelled)
	receiveDial(t, cancelled)
	close(release)
	err := receiveDial(t, result)
	result = nil
	if !errors.Is(err, context.Canceled) || exited.Load() != 2 {
		t.Fatalf("err=%v exited=%d", err, exited.Load())
	}
}

func TestDialPinned_RejectsPriorCancellationAndPreservesAllFailures(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(publicIP)}
	first, second := errors.New("first refused"), errors.New("second refused")
	var attempts []string
	client := New(Config{Dial: func(_ context.Context, _, address string) (net.Conn, error) {
		attempts = append(attempts, address)
		if address == "8.8.8.8:80" {
			return nil, first
		}
		return nil, second
	}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.dialPinned(ctx, "tcp", addresses, 80, nil); !errors.Is(err, context.Canceled) || len(attempts) != 0 {
		t.Fatalf("err=%v attempts=%v", err, attempts)
	}
	_, err := client.dialPinned(t.Context(), "tcp", addresses, 80, nil)
	if !errors.Is(err, first) || !errors.Is(err, second) || !reflect.DeepEqual(attempts, []string{"8.8.8.8:80", publicIP + ":80"}) {
		t.Fatalf("err=%v attempts=%v", err, attempts)
	}
}

func TestInterleaveAddresses_PreservesOrderWithinEachFamily(t *testing.T) {
	v4a, v4b := netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(publicIP)
	v6a, v6b := netip.MustParseAddr("2606:4700::1111"), netip.MustParseAddr("2606:4700::2222")
	for _, test := range []struct{ input, want []netip.Addr }{
		{[]netip.Addr{v6a, v6b, v4a, v4b}, []netip.Addr{v6a, v4a, v6b, v4b}},
		{[]netip.Addr{v4a, v4b, v6a}, []netip.Addr{v4a, v6a, v4b}},
		{[]netip.Addr{v6a, v6b}, []netip.Addr{v6a, v6b}},
	} {
		if got := interleaveAddresses(test.input); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("order=%v want=%v", got, test.want)
		}
	}
}
