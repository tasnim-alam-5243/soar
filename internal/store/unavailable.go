package store

import (
	"context"
	"errors"
	"net/url"
	"time"
)

// Unavailable reports an error that means the data store cannot serve requests right
// now: unreachable (connection refused, DNS, timeout) or overloaded or starting (HTTP
// 429, 502, 503, 504). Such a failure says nothing about the message being handled, so
// queue consumers hold the message and retry in place instead of spending its
// JetStream deliveries, which would move it to the DLQ during a long outage
// (spec 8.7 and 15: "OpenSearch down: stop acking; JetStream retains").
//
// Errors without an HTTP status that are not transport errors (an undecodable answer,
// a bad request) are not unavailability: retrying in place would never end.
func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var hs interface{ HTTPStatus() int }
	if errors.As(err, &hs) {
		return UnavailableStatus(hs.HTTPStatus())
	}
	return false
}

// UnavailableStatus is Unavailable for an HTTP status, for example of one bulk item.
func UnavailableStatus(status int) bool {
	return status == 429 || status == 502 || status == 503 || status == 504
}

// Heartbeat calls beat now and then every interval until stop is called; stop waits
// for the goroutine to end. Held messages need it for the whole hold, not only between
// attempts: one attempt against a hung OpenSearch blocks until the client's 60 s
// timeout, which is as long as an AckWait.
func Heartbeat(every time.Duration, beat func()) (stop func()) {
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			beat()
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(done); <-exited }
}

// Waiter paces in-place retries while the store is unavailable: 500 ms, doubling up
// to Max. Keep the held messages alive separately with Heartbeat. The zero value is
// ready to use.
type Waiter struct {
	Initial time.Duration // first pause (default 500 ms)
	Max     time.Duration // longest pause between attempts (default 15 s)

	Waits int // completed waits so far
	delay time.Duration
}

// Wait pauses before the next attempt and returns ctx.Err() if ctx ends first.
func (w *Waiter) Wait(ctx context.Context) error {
	if w.Initial <= 0 {
		w.Initial = 500 * time.Millisecond
	}
	if w.Max <= 0 {
		w.Max = 15 * time.Second
	}
	switch {
	case w.delay == 0:
		w.delay = min(w.Initial, w.Max)
	case w.delay < w.Max:
		w.delay = min(2*w.delay, w.Max)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(w.delay):
		w.Waits++
		return nil
	}
}
