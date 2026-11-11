package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
)

const (
	maxBodyBytes = 1 << 20 // 1 MiB (spec section 9.9 uses the same cap for ingest)
	defaultLimit = 50
	maxLimit     = 200
)

// decode reads a JSON body into dst, rejecting unknown fields and trailing data.
func decode(r *http.Request, dst any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err // MaxBytesError becomes 413 in toProblem
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return badRequest("a JSON request body is required")
	}
	if d := security.JSONDepth(b); d > security.MaxJSONDepth {
		return badRequest("JSON body nests %d levels deep; the limit is %d", d, security.MaxJSONDepth)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("invalid JSON body: %s", err.Error())
	}
	if dec.More() {
		return badRequest("invalid JSON body: unexpected data after the first value")
	}
	return nil
}

// pageBody is the list envelope used by every collection endpoint.
type pageBody[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// writeResource writes a single document with its ETag.
func writeResource(w http.ResponseWriter, status int, v any, ver store.Version) error {
	if !ver.IsZero() {
		w.Header().Set("ETag", ver.ETag())
	}
	return writeJSON(w, status, v)
}

func writePage[T any](w http.ResponseWriter, p store.Page[T]) error {
	items := p.Items
	if items == nil {
		items = []T{}
	}
	return writeJSON(w, http.StatusOK, pageBody[T]{Items: items, NextCursor: p.NextCursor})
}

// sortSpec maps a public sort name to the index field.
type sortSpec struct {
	fields      map[string]string
	defaultName string
	defaultDesc bool
}

// listQuery parses limit, cursor and sort and rejects parameters the endpoint does not know.
func listQuery(r *http.Request, ss sortSpec, filters ...string) (store.ListQuery, url.Values, error) {
	v := r.URL.Query()
	allowed := map[string]bool{"limit": true, "cursor": true, "sort": true}
	for _, f := range filters {
		allowed[f] = true
	}
	for k := range v {
		if !allowed[k] {
			return store.ListQuery{}, nil, badRequest("unknown query parameter %q", k)
		}
	}
	q := store.ListQuery{Limit: defaultLimit, After: v.Get("cursor")}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return q, v, badRequest("limit must be an integer from 1 to %d", maxLimit)
		}
		q.Limit = n
	}
	name, desc := ss.defaultName, ss.defaultDesc
	if s := v.Get("sort"); s != "" {
		name, desc = strings.TrimPrefix(s, "-"), strings.HasPrefix(s, "-")
	}
	field, ok := ss.fields[name]
	if !ok {
		names := make([]string, 0, len(ss.fields))
		for k := range ss.fields {
			names = append(names, k)
		}
		return q, v, badRequest("cannot sort by %q (allowed: %v; prefix with - for descending)", name, names)
	}
	q.Sort, q.Desc = field, desc
	return q, v, nil
}

// term adds an exact-match filter from a query parameter (comma-separated values mean "any of").
func term(q *store.ListQuery, v url.Values, param, field string) {
	raw := v.Get(param)
	if raw == "" {
		return
	}
	if q.Terms == nil {
		q.Terms = map[string][]string{}
	}
	q.Terms[field] = strings.Split(raw, ",")
}

// timeRange adds from/to filters on a date field and validates them.
func timeRange(q *store.ListQuery, v url.Values, field string) error {
	rg := store.Range{Field: field}
	for _, p := range []struct {
		param string
		dst   *any
	}{{"from", &rg.Gte}, {"to", &rg.Lte}} {
		s := v.Get(p.param)
		if s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return badRequest("%s must be an RFC 3339 timestamp, got %q", p.param, s)
		}
		*p.dst = t.UTC().Format(time.RFC3339Nano)
	}
	if rg.Gte != nil || rg.Lte != nil {
		q.Ranges = append(q.Ranges, rg)
	}
	return nil
}

func getOne[T any](w http.ResponseWriter, r *http.Request, repo store.Repo[T]) error {
	doc, ver, err := repo.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeResource(w, http.StatusOK, doc, ver)
}

// mutate applies fn to a document. Without If-Match it is a read-modify-write with
// bounded retries (last writer wins on disjoint fields). With If-Match it is a
// single compare-and-set that fails with 409 if the document changed.
func mutate[T any](r *http.Request, repo store.Repo[T], id string, fn func(*T) error) (T, store.Version, error) {
	ctx := r.Context()
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		return repo.Update(ctx, id, fn)
	}
	want, ok := store.ParseETag(strings.TrimSpace(ifMatch))
	var zero T
	if !ok {
		return zero, store.Version{}, badRequest("If-Match must be an ETag previously returned by the API")
	}
	doc, cur, err := repo.Get(ctx, id)
	if err != nil {
		return zero, store.Version{}, err
	}
	if cur.SeqNo != want.SeqNo || cur.PrimaryTerm != want.PrimaryTerm {
		return zero, store.Version{}, store.ErrVersionConflict
	}
	if err := fn(&doc); err != nil {
		return zero, store.Version{}, err
	}
	nv, err := repo.Save(ctx, doc, cur)
	return doc, nv, err
}

// ---- idempotent creates ----

var idemKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type idem struct {
	s      *Server
	id     string
	active bool
	replay string // id of the resource created by the first request, when this is a replay
}

// idemBegin implements the Idempotency-Key header for creates: the first request
// with a key proceeds, a repeat after success returns the original resource, a
// repeat while the first is still running gets 409.
func (s *Server) idemBegin(r *http.Request) (*idem, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return &idem{}, nil
	}
	if !idemKeyRe.MatchString(key) {
		return nil, badRequest("Idempotency-Key must be 1-128 characters from A-Z a-z 0-9 . _ : -")
	}
	sum := sha256.Sum256([]byte(r.Method + " " + r.URL.Path + "\x00" + key))
	id := "idm_" + hex.EncodeToString(sum[:16])
	ctx := r.Context()
	rec := domain.Idempotency{Meta: domain.Meta{ID: id}, Key: key, Status: "in_progress", ExpiresAt: s.now().Add(24 * time.Hour)}
	_, err := s.st.Idempotency.Create(ctx, rec)
	if err == nil {
		return &idem{s: s, id: id, active: true}, nil
	}
	if !errors.Is(err, store.ErrAlreadyExists) {
		return nil, err
	}
	cur, _, err := s.st.Idempotency.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case cur.Status == "done":
		return &idem{replay: cur.ResultRef}, nil
	case s.now().Sub(cur.UpdatedAt) > 2*time.Minute: // the first attempt died; take over
		if _, err := s.st.Idempotency.Put(ctx, rec); err != nil {
			return nil, err
		}
		return &idem{s: s, id: id, active: true}, nil
	default:
		return nil, conflict("idempotency-in-progress", "a request with this Idempotency-Key is still being processed")
	}
}

func (i *idem) done(r *http.Request, resourceID string) {
	if !i.active {
		return
	}
	_, _, err := i.s.st.Idempotency.Update(r.Context(), i.id, func(d *domain.Idempotency) error {
		d.Status, d.ResultRef = "done", resourceID
		return nil
	})
	if err != nil {
		i.s.log.WarnContext(r.Context(), "could not mark idempotency key done", "error", err.Error())
	}
}

func (i *idem) abort(r *http.Request) {
	if i.active {
		_ = i.s.st.Idempotency.Delete(r.Context(), i.id) // let the client retry with the same key
	}
}

// createResource implements POST for a collection with Idempotency-Key support.
// collection is the path of the collection for the Location header ("" for none).
func createResource[T any](s *Server, w http.ResponseWriter, r *http.Request, repo store.Repo[T], collection string,
	build func() (T, string, error),
) error {
	ik, err := s.idemBegin(r)
	if err != nil {
		return err
	}
	if ik.replay != "" {
		doc, ver, err := repo.Get(r.Context(), ik.replay)
		if err != nil {
			return err
		}
		w.Header().Set("Idempotent-Replayed", "true")
		if collection != "" {
			w.Header().Set("Location", Prefix+collection+"/"+ik.replay)
		}
		return writeResource(w, http.StatusCreated, doc, ver)
	}
	doc, id, err := build()
	if err != nil {
		ik.abort(r)
		return err
	}
	ver, err := repo.Create(r.Context(), doc)
	if err != nil {
		ik.abort(r)
		return err
	}
	ik.done(r, id)
	if collection != "" {
		w.Header().Set("Location", Prefix+collection+"/"+id)
	}
	// Re-read so the response carries server-stamped fields exactly as stored.
	stored, sv, gerr := repo.Get(r.Context(), id)
	if gerr != nil {
		return writeResource(w, http.StatusCreated, doc, ver)
	}
	return writeResource(w, http.StatusCreated, stored, sv)
}
