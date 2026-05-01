/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package apiserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/egoavara/route-prism/internal/dashboard"
	"github.com/egoavara/route-prism/internal/widget"
)

const (
	defaultLimit = 50
	maxLimit     = 500
	// SelfAlternative is the synthetic alternative name representing the
	// target Service itself ("no Baggage stamp" / unmarked traffic).
	SelfAlternative = "."
)

// API serves read-only routing queries from a precomputed Index.
// Decoupled from net/http so handlers can be unit-tested without a
// listener; the Server in this package wires the HTTP layer.
type API struct {
	idx *Index
}

// NewAPI wraps an Index with HTTP handlers.
func NewAPI(idx *Index) *API {
	return &API{idx: idx}
}

// Register wires API routes onto the provided chi router.
func (a *API) Register(r chi.Router) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/api/v1/service", a.handleListServices)
	r.Get("/api/v1/service/{target}/alternative", a.handleListAlternatives)
	r.Get("/api/v1/tuple", a.handleListTuples)
	a.registerDashboard(r)
	a.registerWidget(r)
}

// registerDashboard mounts the embedded web UI at /dashboard/ and
// redirects /dashboard → /dashboard/ so relative asset URLs resolve.
// Defined as a separate method to keep the dashboard import out of the
// hot read-API path and easy to disable in tests.
func (a *API) registerDashboard(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/dashboard/", http.StatusFound)
	})
	r.Get("/dashboard", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/dashboard/", http.StatusMovedPermanently)
	})
	r.Mount("/dashboard/", http.StripPrefix("/dashboard", dashboard.Handler()))
}

// registerWidget mounts the embedded in-page widget bundle at /widget/.
// The translator (with widgetInjection enabled) proxies its configured
// pathPrefix to this same mount, so the widget JS is always served from
// the host page's origin (no CORS).
func (a *API) registerWidget(r chi.Router) {
	r.Mount("/widget/", http.StripPrefix("/widget", widget.Handler()))
}

// ServiceItem is a single entry in list responses.
type ServiceItem struct {
	Target string `json:"target"`
	// Translator, when non-empty, is the cookie name an attached
	// EdgeTransformation lifts into the Baggage header. Surfaced on the
	// /service list so callers can flag targets that have a translator
	// without a second round-trip. Empty means no EdgeTransformation.
	Translator string `json:"translator,omitempty"`
}

// ListResponse is the wire shape for paginated list endpoints.
type ListResponse struct {
	Items      []ServiceItem `json:"items"`
	NextCursor string        `json:"nextCursor,omitempty"`
	// Alternative-specific routing context. Populated only by the
	// /alternative endpoint; omitted from /service list responses.
	RoutingKey   string `json:"routingKey,omitempty"`
	SourceCookie string `json:"sourceCookie,omitempty"`
}

// TupleListResponse is the wire shape for /api/v1/tuple. Items carry
// fully-populated TupleEntry rows so the widget needs only one round-trip
// to drive routing.
type TupleListResponse struct {
	Items      []TupleEntry `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

func (a *API) handleListServices(w http.ResponseWriter, r *http.Request) {
	snap := a.idx.Snapshot()
	q := r.URL.Query()
	f := parseFilter(q)
	limit := parseLimit(q.Get("limit"))
	cursor := decodeCursor(q.Get("cursor"))

	matched := f.apply(snap.targets, snap.targetsLower)
	page, next := paginate(matched, cursor, limit)
	writeJSON(w, http.StatusOK, toServiceListResponse(snap, page, next))
}

// handleListTuples serves the flattened (target × alternative) view used
// by the in-page widget. fuzzy is a case-insensitive substring filter on
// the Tuple field — the widget further refines/highlights with microfuzz
// client-side, so the server stays simple and deterministic. Pagination
// uses the same opaque cursor format as /service.
func (a *API) handleListTuples(w http.ResponseWriter, r *http.Request) {
	snap := a.idx.Snapshot()
	q := r.URL.Query()
	fuzzy := strings.ToLower(q.Get("fuzzy"))
	limit := parseLimit(q.Get("limit"))
	cursor := decodeCursor(q.Get("cursor"))

	// Filter (substring) into a fresh slice when fuzzy is set; otherwise
	// page directly off the snapshot slice.
	var filtered []TupleEntry
	if fuzzy == "" {
		filtered = snap.tuples
	} else {
		filtered = make([]TupleEntry, 0, len(snap.tuples))
		for j, low := range snap.tuplesLower {
			if strings.Contains(low, fuzzy) {
				filtered = append(filtered, snap.tuples[j])
			}
		}
	}

	page, next := paginateTuples(filtered, cursor, limit)
	if page == nil {
		page = []TupleEntry{}
	}
	writeJSON(w, http.StatusOK, TupleListResponse{Items: page, NextCursor: next})
}

func (a *API) handleListAlternatives(w http.ResponseWriter, r *http.Request) {
	target := chi.URLParam(r, "target")
	if target == "" {
		writeError(w, http.StatusBadRequest, "target is required")
		return
	}
	snap := a.idx.Snapshot()
	entry, ok := snap.altByTarget[target]
	if !ok {
		writeError(w, http.StatusNotFound, "target not found")
		return
	}
	q := r.URL.Query()
	f := parseFilter(q)
	limit := parseLimit(q.Get("limit"))
	cursor := decodeCursor(q.Get("cursor"))

	matched := f.apply(entry.list, entry.lower)
	page, next := paginate(matched, cursor, limit)
	resp := toAlternativeListResponse(page, next)
	if meta, ok := snap.metaByTarget[target]; ok {
		resp.RoutingKey = meta.routingKey
		resp.SourceCookie = meta.sourceCookie
	} else {
		resp.RoutingKey = target
	}
	writeJSON(w, http.StatusOK, resp)
}

// filter holds the per-request query parameters. All filters are AND-ed.
// A zero-value filter passes everything.
type filter struct {
	equals     string
	startswith string
	fuzzy      string
}

func parseFilter(q url.Values) filter {
	return filter{
		equals:     q.Get("target.equals"),
		startswith: q.Get("target.startswith"),
		fuzzy:      q.Get("target.fuzzy"),
	}
}

// apply runs the filter chain against a sorted slice (and its parallel
// lowercased twin) and returns the matching entries — still sorted.
//
// Performance contract:
//   - equals     — O(log N), one binary search
//   - startswith — O(log N), two binary searches narrowing to a contiguous range
//   - fuzzy      — O(K · L), where K = remaining candidates after equals/startswith
//     narrowing and L = avg target length. With small K this is
//     effectively constant work per request.
//
// The returned slice is either a sub-slice of the snapshot (when no
// fuzzy filter is applied) or a fresh allocation. Either way it is safe
// to read but must not be mutated.
func (f filter) apply(sorted, sortedLower []string) []string {
	lo, hi := 0, len(sorted)

	// Narrow by prefix using two binary searches. The matched range is
	// contiguous because `sorted` is sorted lexicographically.
	if f.startswith != "" {
		lo = sort.SearchStrings(sorted, f.startswith)
		// Find the first index in sorted[lo:] that no longer has the prefix.
		hi = lo + sort.Search(len(sorted)-lo, func(j int) bool {
			return !strings.HasPrefix(sorted[lo+j], f.startswith)
		})
	}

	// Equals is the most restrictive — apply within the [lo, hi) window.
	if f.equals != "" {
		idx := sort.SearchStrings(sorted, f.equals)
		if idx >= lo && idx < hi && idx < len(sorted) && sorted[idx] == f.equals {
			return sorted[idx : idx+1]
		}
		return nil
	}

	if f.fuzzy == "" {
		return sorted[lo:hi]
	}

	fuzzy := strings.ToLower(f.fuzzy)
	out := make([]string, 0, hi-lo)
	for j := lo; j < hi; j++ {
		if !subseqMatch(sortedLower[j], fuzzy) {
			continue
		}
		out = append(out, sorted[j])
	}
	return out
}

// subseqMatch returns true when every byte of pattern appears in s in
// order (not necessarily contiguously). Both inputs are expected to be
// already lowercased. Service names are DNS labels (ASCII), so byte-wise
// comparison is correct.
func subseqMatch(s, pattern string) bool {
	if pattern == "" {
		return true
	}
	i := 0
	for j := 0; i < len(pattern) && j < len(s); j++ {
		if s[j] == pattern[i] {
			i++
		}
	}
	return i == len(pattern)
}

func parseLimit(s string) int {
	if s == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	return min(n, maxLimit)
}

// paginate slices the sorted input given an opaque cursor (last item of
// the previous page) and a positive limit, returning the page plus the
// next cursor (empty string when the page is the last one).
func paginate(sorted []string, cursor string, limit int) ([]string, string) {
	startIdx := 0
	if cursor != "" {
		startIdx = sort.SearchStrings(sorted, cursor)
		if startIdx < len(sorted) && sorted[startIdx] == cursor {
			startIdx++
		}
	}
	if startIdx >= len(sorted) {
		return nil, ""
	}
	end := min(startIdx+limit, len(sorted))
	page := sorted[startIdx:end]
	var next string
	if end < len(sorted) && len(page) > 0 {
		next = encodeCursor(page[len(page)-1])
	}
	return page, next
}

// toServiceListResponse decorates each entry with its translator cookie
// (if any) so the dashboard can flag ET-backed targets at a glance
// without a per-row /alternative call.
func toServiceListResponse(snap *snapshot, page []string, nextCursor string) ListResponse {
	items := make([]ServiceItem, len(page))
	for i, t := range page {
		item := ServiceItem{Target: t}
		if meta, ok := snap.metaByTarget[t]; ok {
			item.Translator = meta.sourceCookie
		}
		items[i] = item
	}
	return ListResponse{Items: items, NextCursor: nextCursor}
}

// toAlternativeListResponse renders raw alternative names without
// per-item metadata — alternatives don't carry routing context of their
// own; the surrounding response carries the routingKey/sourceCookie.
func toAlternativeListResponse(page []string, nextCursor string) ListResponse {
	items := make([]ServiceItem, len(page))
	for i, t := range page {
		items[i] = ServiceItem{Target: t}
	}
	return ListResponse{Items: items, NextCursor: nextCursor}
}

// paginateTuples mirrors paginate() but works against a sorted []TupleEntry.
// Cursor is the Tuple string of the last item on the previous page.
func paginateTuples(sorted []TupleEntry, cursor string, limit int) ([]TupleEntry, string) {
	startIdx := 0
	if cursor != "" {
		startIdx = sort.Search(len(sorted), func(i int) bool { return sorted[i].Tuple >= cursor })
		if startIdx < len(sorted) && sorted[startIdx].Tuple == cursor {
			startIdx++
		}
	}
	if startIdx >= len(sorted) {
		return nil, ""
	}
	end := min(startIdx+limit, len(sorted))
	page := sorted[startIdx:end]
	var next string
	if end < len(sorted) && len(page) > 0 {
		next = encodeCursor(page[len(page)-1].Tuple)
	}
	return page, next
}

func encodeCursor(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func decodeCursor(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
