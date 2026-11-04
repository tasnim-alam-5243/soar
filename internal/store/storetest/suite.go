// Package storetest is the behaviour contract every store.Repo implementation must
// satisfy. internal/store/memory runs it in unit tests; the OpenSearch repository
// runs the same suite in integration tests, so the fast fake can never drift.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

func newCase(title, status string, sev int, created time.Time) domain.Case {
	m := domain.NewMeta(domain.PrefixCase)
	m.CreatedAt = created
	return domain.Case{Meta: m, Title: title, Status: status, Severity: sev}
}

// RunStateSuite checks a repository for a state index (read by ID, deletable).
// newRepo must return an empty repository whose Update retries conflicts maxRetries times.
func RunStateSuite(t *testing.T, newRepo func(t *testing.T, maxRetries int) store.Repo[domain.Case]) {
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("create_get", func(t *testing.T) {
		r := newRepo(t, 8)
		c := newCase("phish", "open", 3, base)
		v, err := r.Create(ctx, c)
		if err != nil || v.IsZero() {
			t.Fatalf("create: %v %+v", err, v)
		}
		got, gv, err := r.Get(ctx, c.ID)
		if err != nil || got.Title != "phish" || got.UpdatedAt.IsZero() || got.SchemaVersion != domain.SchemaVersion {
			t.Fatalf("get: %v %+v", err, got)
		}
		if gv.SeqNo != v.SeqNo || gv.PrimaryTerm != v.PrimaryTerm {
			t.Errorf("version from Get %+v != from Create %+v", gv, v)
		}
		if !got.CreatedAt.Equal(base) {
			t.Errorf("explicit CreatedAt must be kept, got %v", got.CreatedAt)
		}
	})

	t.Run("create_duplicate_and_missing", func(t *testing.T) {
		r := newRepo(t, 8)
		c := newCase("a", "open", 1, base)
		if _, err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Create(ctx, c); !errors.Is(err, store.ErrAlreadyExists) {
			t.Errorf("duplicate create: want ErrAlreadyExists, got %v", err)
		}
		if _, _, err := r.Get(ctx, "cas_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("get missing: want ErrNotFound, got %v", err)
		}
		if _, err := r.Create(ctx, domain.Case{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("create without id: want ErrInvalid, got %v", err)
		}
	})

	t.Run("save_is_optimistic", func(t *testing.T) {
		r := newRepo(t, 8)
		c := newCase("a", "open", 1, base)
		if _, err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		doc1, v1, _ := r.Get(ctx, c.ID)
		doc2, v2, _ := r.Get(ctx, c.ID) // two writers read the same revision
		doc1.Title = "writer one"
		nv, err := r.Save(ctx, doc1, v1)
		if err != nil || nv.SeqNo == v1.SeqNo {
			t.Fatalf("first save: %v %+v", err, nv)
		}
		doc2.Title = "writer two"
		if _, err := r.Save(ctx, doc2, v2); !errors.Is(err, store.ErrVersionConflict) {
			t.Fatalf("stale save: want ErrVersionConflict, got %v", err)
		}
		got, _, _ := r.Get(ctx, c.ID)
		if got.Title != "writer one" {
			t.Errorf("stale writer must not overwrite, title=%q", got.Title)
		}
		if _, err := r.Save(ctx, doc1, store.Version{}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("save without version: want ErrInvalid, got %v", err)
		}
	})

	t.Run("put_upserts", func(t *testing.T) {
		r := newRepo(t, 8)
		c := newCase("first", "open", 1, base)
		if _, err := r.Put(ctx, c); err != nil {
			t.Fatal(err)
		}
		c.Title = "second"
		if _, err := r.Put(ctx, c); err != nil {
			t.Fatal(err)
		}
		got, _, _ := r.Get(ctx, c.ID)
		if got.Title != "second" {
			t.Errorf("title=%q", got.Title)
		}
	})

	t.Run("update_concurrent_loses_nothing", func(t *testing.T) {
		const n = 12
		r := newRepo(t, 200)
		c := newCase("race", "open", 1, base)
		if _, err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tag := fmt.Sprintf("t%02d", i)
				_, _, err := r.Update(ctx, c.ID, func(d *domain.Case) error {
					d.Tags = append(d.Tags, tag)
					return nil
				})
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("update: %v", err)
			}
		}
		got, _, _ := r.Get(ctx, c.ID)
		sort.Strings(got.Tags)
		if len(got.Tags) != n {
			t.Fatalf("lost updates: want %d tags, got %d: %v", n, len(got.Tags), got.Tags)
		}
	})

	t.Run("update_gives_up_after_bounded_retries", func(t *testing.T) {
		r := newRepo(t, 2)
		c := newCase("contended", "open", 1, base)
		if _, err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		calls := 0
		_, _, err := r.Update(ctx, c.ID, func(d *domain.Case) error {
			calls++
			// An interfering writer sneaks in on every attempt.
			other, _, _ := r.Get(ctx, c.ID)
			other.Summary = fmt.Sprint("interference ", calls)
			_, _ = r.Put(ctx, other)
			d.Title = "never lands"
			return nil
		})
		if !errors.Is(err, store.ErrConflictExhausted) {
			t.Fatalf("want ErrConflictExhausted, got %v", err)
		}
		if calls != 3 { // first try + 2 retries
			t.Errorf("mutate ran %d times, want 3", calls)
		}
	})

	t.Run("update_errors", func(t *testing.T) {
		r := newRepo(t, 8)
		c := newCase("a", "open", 1, base)
		if _, err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		if _, _, err := r.Update(ctx, c.ID, func(*domain.Case) error { return boom }); !errors.Is(err, boom) {
			t.Errorf("mutate error must be returned, got %v", err)
		}
		if _, _, err := r.Update(ctx, "cas_missing", func(*domain.Case) error { return nil }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("update missing: want ErrNotFound, got %v", err)
		}
		if _, _, err := r.Update(ctx, c.ID, func(d *domain.Case) error { d.ID = "cas_other"; return nil }); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("changing id: want ErrInvalid, got %v", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		r := newRepo(t, 8)
		c := newCase("a", "open", 1, base)
		if _, err := r.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.Get(ctx, c.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("after delete: %v", err)
		}
		if err := r.Delete(ctx, c.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second delete: want ErrNotFound, got %v", err)
		}
	})

	t.Run("list_filter_sort_paginate", func(t *testing.T) {
		r := newRepo(t, 8)
		sevs := []int{3, 1, 3, 5, 2, 3, 4}
		for i, s := range sevs {
			status := "open"
			if i%3 == 2 {
				status = "closed"
			}
			if _, err := r.Create(ctx, newCase(fmt.Sprint("case ", i), status, s, base.Add(time.Duration(i)*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}

		all, err := r.List(ctx, store.ListQuery{Limit: 100}) // default: created_at ascending
		if err != nil || len(all.Items) != 7 || all.NextCursor != "" {
			t.Fatalf("list all: %v n=%d next=%q", err, len(all.Items), all.NextCursor)
		}
		desc, _ := r.List(ctx, store.ListQuery{Desc: true, Limit: 100})
		if desc.Items[0].Title != "case 6" || desc.Items[6].Title != "case 0" {
			t.Errorf("created_at desc order wrong: %s ... %s", desc.Items[0].Title, desc.Items[6].Title)
		}

		open, _ := r.List(ctx, store.ListQuery{Terms: map[string][]string{"status": {"open"}}, Limit: 100})
		if len(open.Items) != 5 {
			t.Errorf("status=open: want 5, got %d", len(open.Items))
		}
		either, _ := r.List(ctx, store.ListQuery{Terms: map[string][]string{"severity": {"1", "5"}}, Limit: 100})
		if len(either.Items) != 2 {
			t.Errorf("severity in (1,5): want 2, got %d", len(either.Items))
		}
		rng, _ := r.List(ctx, store.ListQuery{Ranges: []store.Range{{Field: "severity", Gte: 3}}, Limit: 100})
		if len(rng.Items) != 5 {
			t.Errorf("severity>=3: want 5, got %d", len(rng.Items))
		}
		when, _ := r.List(ctx, store.ListQuery{Ranges: []store.Range{{Field: "created_at", Gte: base.Add(2 * time.Hour).Format(time.RFC3339), Lte: base.Add(4 * time.Hour).Format(time.RFC3339)}}, Limit: 100})
		if len(when.Items) != 3 {
			t.Errorf("created_at range: want 3, got %d", len(when.Items))
		}

		// Walk every page sorted by severity (many ties) and require a stable total order.
		var seen []string
		cursor := ""
		pages := 0
		for {
			p, err := r.List(ctx, store.ListQuery{Sort: "severity", Desc: true, Limit: 3, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			for _, c := range p.Items {
				seen = append(seen, c.ID)
			}
			if p.NextCursor == "" {
				break
			}
			cursor = p.NextCursor
		}
		uniq := map[string]bool{}
		for _, id := range seen {
			uniq[id] = true
		}
		if pages != 3 || len(seen) != 7 || len(uniq) != 7 {
			t.Errorf("pagination: pages=%d items=%d unique=%d (want 3/7/7)", pages, len(seen), len(uniq))
		}

		if _, err := r.List(ctx, store.ListQuery{After: "not-a-cursor"}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad cursor: want ErrInvalid, got %v", err)
		}
	})
}

// RunTimeSeriesSuite checks a repository for a time-series alias (read through search).
func RunTimeSeriesSuite(t *testing.T, newRepo func(t *testing.T) store.Repo[domain.Alert]) {
	ctx := context.Background()
	newAlert := func(source string, sev int) domain.Alert {
		return domain.Alert{Meta: domain.NewMeta(domain.PrefixAlert), Kind: "alert", SourceID: source, Title: "t " + source,
			Severity: sev, ReceivedAt: time.Now().UTC(), OccurrenceCount: 1}
	}

	t.Run("create_get_save", func(t *testing.T) {
		r := newRepo(t)
		a := newAlert("wazuh", 3)
		if _, err := r.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Create(ctx, a); !errors.Is(err, store.ErrAlreadyExists) {
			t.Errorf("duplicate: want ErrAlreadyExists, got %v", err)
		}
		got, v, err := r.Get(ctx, a.ID)
		if err != nil || got.SourceID != "wazuh" || v.IsZero() {
			t.Fatalf("get: %v %+v %+v", err, got, v)
		}
		got.OccurrenceCount = 2
		nv, err := r.Save(ctx, got, v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Save(ctx, got, v); !errors.Is(err, store.ErrVersionConflict) {
			t.Errorf("stale save: want ErrVersionConflict, got %v", err)
		}
		again, v2, _ := r.Get(ctx, a.ID)
		if again.OccurrenceCount != 2 || v2.SeqNo != nv.SeqNo {
			t.Errorf("after save: %+v %+v vs %+v", again.OccurrenceCount, v2, nv)
		}
		if _, _, err := r.Get(ctx, "alt_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("missing: want ErrNotFound, got %v", err)
		}
	})

	t.Run("update_and_list", func(t *testing.T) {
		r := newRepo(t)
		var ids []string
		for i := 0; i < 5; i++ {
			src := "a"
			if i >= 3 {
				src = "b"
			}
			a := newAlert(src, i+1)
			ids = append(ids, a.ID)
			if _, err := r.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
		got, _, err := r.Update(ctx, ids[0], func(a *domain.Alert) error { a.Verdict = "benign"; return nil })
		if err != nil || got.Verdict != "benign" {
			t.Fatalf("update: %v %+v", err, got)
		}
		p, err := r.List(ctx, store.ListQuery{Terms: map[string][]string{"source_id": {"b"}}, Sort: "received_at", Limit: 10})
		if err != nil || len(p.Items) != 2 {
			t.Fatalf("list source b: %v n=%d", err, len(p.Items))
		}
		p, _ = r.List(ctx, store.ListQuery{Terms: map[string][]string{"verdict": {"benign"}}, Limit: 10})
		if len(p.Items) != 1 || p.Items[0].ID != ids[0] {
			t.Errorf("verdict filter returned %d items", len(p.Items))
		}
	})
}
