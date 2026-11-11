package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"
)

type statusErr int

func (e statusErr) Error() string   { return fmt.Sprintf("HTTP %d", int(e)) }
func (e statusErr) HTTPStatus() int { return int(e) }

func TestUnavailable(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "http://opensearch:9200/_bulk", Err: errors.New("connection refused")}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{refused, true},
		{fmt.Errorf("idempotency lookup: %w", refused), true},
		{errors.Join(refused, refused), true}, // every node of a multi-node client failed
		{statusErr(429), true},
		{statusErr(503), true},
		{statusErr(504), true},
		{statusErr(502), true},
		{statusErr(400), false},
		{statusErr(500), false},
		{statusErr(409), false},
		{errors.New("decode bulk response: invalid character"), false},
		{ErrNotFound, false},
	} {
		if got := Unavailable(c.err); got != c.want {
			t.Errorf("Unavailable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestWaiterBacksOff(t *testing.T) {
	w := Waiter{Initial: 20 * time.Millisecond, Max: 60 * time.Millisecond}
	start := time.Now()
	for i := 0; i < 4; i++ { // 20 + 40 + 60 + 60 ms
		if err := w.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 170*time.Millisecond || took > time.Second || w.Waits != 4 {
		t.Errorf("backoff 20, 40, 60, 60 ms took %s (%d waits)", took, w.Waits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait must return the context error, got %v", err)
	}
}

func TestHeartbeatBeatsAtOnceAndUntilStopped(t *testing.T) {
	var mu sync.Mutex
	beats := 0
	stop := Heartbeat(20*time.Millisecond, func() { mu.Lock(); beats++; mu.Unlock() })
	time.Sleep(110 * time.Millisecond)
	stop()
	mu.Lock()
	n := beats
	mu.Unlock()
	if n < 4 { // at 0, 20, 40, 60, 80, 100 ms
		t.Errorf("want a beat at once and every interval, got %d", n)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if beats != n {
		t.Error("no beat may happen after stop returned")
	}
}
