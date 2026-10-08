package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

type dialResult struct {
	connection net.Conn
	err        error
}

// dialPinned staggers attempts over the validated set, alternating families.
// A failure advances immediately; a pending attempt cannot starve later ones.
// The owner cancels and joins every attempt and closes all unused connections.
func (client *Client) dialPinned(ctx context.Context, network string, addresses []netip.Addr, port uint16, fallback <-chan time.Time) (winner net.Conn, err error) {
	dialContext, cancel := context.WithCancel(ctx)
	results := make(chan dialResult, len(addresses))
	var group sync.WaitGroup
	defer func() {
		cancel()
		group.Wait()
		close(results)
		for result := range results {
			if result.connection != nil {
				_ = result.connection.Close()
			}
		}
		// Cancellation during join also prevents publishing a connection.
		if winner != nil && ctx.Err() != nil {
			_ = winner.Close()
			winner, err = nil, fmt.Errorf("dial validated addresses: %w", ctx.Err())
		}
	}()
	pending := interleaveAddresses(addresses)
	active := 0
	start := func() {
		address := pending[0]
		pending = pending[1:]
		active++
		group.Go(func() {
			connection, err := client.dial(dialContext, network, netip.AddrPortFrom(address, port).String())
			results <- dialResult{connection: connection, err: err}
		})
	}
	var failures []error
	for ctx.Err() == nil && (len(pending) > 0 || active > 0) {
		if active == 0 {
			start()
		}
		select {
		case <-ctx.Done():
		case <-fallback:
			if len(pending) > 0 {
				start()
			}
		case result := <-results:
			active--
			if result.err == nil {
				return result.connection, nil
			}
			failures = append(failures, result.err)
			if len(pending) > 0 {
				start()
			}
		}
	}
	return nil, fmt.Errorf("dial validated addresses: %w", errors.Join(append(failures, ctx.Err())...))
}

// interleaveAddresses keeps resolver order within each family and gives the
// other family the second attempt, even after several same-family answers.
func interleaveAddresses(addresses []netip.Addr) []netip.Addr {
	var primary, secondary []netip.Addr
	for _, address := range addresses {
		if address.Is4() == addresses[0].Is4() {
			primary = append(primary, address)
		} else {
			secondary = append(secondary, address)
		}
	}
	ordered := make([]netip.Addr, 0, len(addresses))
	for index := 0; index < len(primary) || index < len(secondary); index++ {
		if index < len(primary) {
			ordered = append(ordered, primary[index])
		}
		if index < len(secondary) {
			ordered = append(ordered, secondary[index])
		}
	}
	return ordered
}
