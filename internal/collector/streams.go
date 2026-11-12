package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/adapters"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/security"
)

// streamLease is how long an exclusive stream's lease lasts; it is renewed every
// third of that while the stream runs.
const streamLease = 60 * time.Second

// runStream runs a stream source (Step 10: syslog, Kafka, file/object) until ctx ends,
// restarting it with a back-off when it stops.
func (c *Collector) runStream(ctx context.Context, rt *ingest.Runtime, spec adapters.StreamSpec) {
	log := c.Log.With("source", rt.Source.Name, "adapter", rt.Source.Adapter)
	fail := 0
	for ctx.Err() == nil {
		err := c.streamOnce(ctx, rt, spec, log)
		if ctx.Err() != nil {
			return
		}
		wait := 5 * time.Second
		switch {
		case errors.Is(err, errNoLease):
			fail = 0 // another replica runs it
		case err != nil:
			fail++
			wait = min(time.Duration(1<<min(fail, 7))*time.Second, 2*time.Minute)
			log.Warn("stream stopped; restarting", "error", err.Error(), "retry_in", wait.String())
		default:
			fail = 0
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

func (c *Collector) streamOnce(ctx context.Context, rt *ingest.Runtime, spec adapters.StreamSpec, log *slog.Logger) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	deps := adapters.StreamDeps{Log: log, Now: c.now, LocalDirs: c.Dirs}
	if name, _ := rt.Source.Config["connection"].(string); name != "" || spec.NeedsConnection {
		conn, err := c.connection(ctx, rt)
		if err != nil {
			return err
		}
		deps.Conn = &conn
		deps.Resolve = func(ref string) (string, error) {
			return c.Secrets(ref, security.UseConnection, security.ConnectionConsumers(conn)...)
		}
	}
	if spec.Exclusive {
		cur, err := c.acquire(ctx, rt.Source.Name, streamLease)
		if err != nil {
			return err
		}
		deps.Cursor = &leaseCursor{c: c, name: rt.Source.Name, value: cur.Cursor}
		go func() { // keep the lease; losing it stops the stream
			tick := time.NewTicker(streamLease / 3)
			defer tick.Stop()
			for {
				select {
				case <-sctx.Done():
					return
				case <-tick.C:
					if err := c.renew(sctx, rt.Source.Name, streamLease); err != nil && sctx.Err() == nil {
						log.Warn("lost the source lease; stopping this replica's stream", "error", err.Error())
						cancel()
						return
					}
				}
			}
		}()
	}
	st, err := spec.New(rt.Source, deps)
	if err != nil {
		return err
	}
	log.Info("stream started")
	return st.Run(sctx, c.emitter(rt))
}

// renew extends this replica's lease on a source without moving its cursor.
func (c *Collector) renew(ctx context.Context, name string, ttl time.Duration) error {
	exp := c.now().Add(ttl)
	_, _, err := c.St.Cursors.Update(ctx, cursorID(name), func(d *domain.Cursor) error {
		if d.LeaseOwner != c.Owner {
			return errNoLease
		}
		d.LeaseExpiresAt = &exp
		return nil
	})
	return err
}

// leaseCursor is an exclusive stream's position, saved only while the lease is held.
type leaseCursor struct {
	c     *Collector
	name  string
	mu    sync.Mutex
	value string
}

func (l *leaseCursor) Load(context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.value, nil
}

func (l *leaseCursor) Save(ctx context.Context, v string) error {
	if err := l.c.advance(ctx, l.name, v, streamLease); err != nil {
		return err
	}
	l.mu.Lock()
	l.value = v
	l.mu.Unlock()
	return nil
}

// emitter parses a stream batch with the source's parser, maps and filters it, and
// publishes the events. It returns an error unless every event is queued, so the
// stream keeps the batch; messages with a transport identity are re-sent under the
// same queue message id, which deduplicates them.
func (c *Collector) emitter(rt *ingest.Runtime) adapters.Emit {
	name := rt.Source.Name
	return func(ctx context.Context, batch []adapters.Message) error {
		var msgs []queue.Msg
		var failures []ingest.Failure
		filtered := 0
		for _, m := range batch {
			recs, err := rt.Pipeline.Parser.Parse(m.Body)
			if err != nil {
				failures = append(failures, ingest.Failure{Record: map[string]any{"body": clip(m.Body), "_transport": m.Meta}, Stage: "parse", Err: err.Error()})
				continue
			}
			for i, r := range recs {
				if m.Meta != nil {
					r["_transport"] = m.Meta
				}
				out := rt.Pipeline.ProcessRecords([]map[string]any{r})
				filtered += out.Filtered
				failures = append(failures, out.Failures...)
				for j, ev := range out.Events {
					key := ""
					if m.ID != "" {
						key = "stream:" + name + ":" + m.ID + "#" + strconv.Itoa(i) + "." + strconv.Itoa(j)
					}
					msg, err := ingest.EventMessage(ev, rt.Pipeline.System, key)
					if err != nil {
						failures = append(failures, ingest.Failure{Record: ev.Alert.Ext, Stage: "publish", Err: err.Error()})
						continue
					}
					msgs = append(msgs, msg)
				}
			}
		}
		for _, perr := range c.Q.PublishMany(ctx, msgs) {
			if perr != nil {
				return fmt.Errorf("publish: %w", perr)
			}
		}
		ingest.SendFailures(ctx, c.Q, c.St, c.Log, name, rt.Source.Adapter, failures)
		obs.SourceEvents.WithLabelValues(name, "accepted").Add(float64(len(msgs)))
		if filtered > 0 {
			obs.SourceEvents.WithLabelValues(name, "filtered").Add(float64(filtered))
		}
		if len(failures) > 0 {
			obs.SourceEvents.WithLabelValues(name, "failed").Add(float64(len(failures)))
		}
		return nil
	}
}

func clip(b []byte) string {
	if len(b) > 4096 {
		return string(b[:4096]) + "..."
	}
	return string(b)
}
