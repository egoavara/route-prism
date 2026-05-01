/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

func newTestAPI(t *testing.T, objs ...runtime.Object) *API {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := routeprismv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	idx := NewIndex(c)
	if err := idx.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	return NewAPI(idx)
}

func mustGet(t *testing.T, api *API, path string) ListResponse {
	t.Helper()
	mux := chi.NewRouter()
	api.Register(mux)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func mkCR(name, ns, target, routingKey string, created time.Time) *routeprismv1alpha1.ContextRoute {
	return &routeprismv1alpha1.ContextRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: routeprismv1alpha1.ContextRouteSpec{
			Target:     routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: target}},
			RoutingKey: routingKey,
			Variants: routeprismv1alpha1.VariantSelector{
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": target}},
			},
		},
	}
}

func mkSvc(name, ns string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
	}
}

func TestListServices(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkCR("b", "demo", "api", "demo.api", now),
		mkCR("c", "other", "web", "", now),
	)

	resp := mustGet(t, api, "/api/v1/service")
	if got, want := len(resp.Items), 3; got != want {
		t.Fatalf("items=%d want %d", got, want)
	}
	expected := []string{"demo.api", "demo.web", "other.web"}
	for i, want := range expected {
		if resp.Items[i].Target != want {
			t.Errorf("items[%d]=%q want %q", i, resp.Items[i].Target, want)
		}
	}
}

func TestListServices_FilterEquals(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkCR("b", "demo", "api", "", now),
	)
	resp := mustGet(t, api, "/api/v1/service?target.equals=demo.web")
	if len(resp.Items) != 1 || resp.Items[0].Target != "demo.web" {
		t.Fatalf("unexpected: %+v", resp.Items)
	}
}

func TestListServices_FilterStartswith(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkCR("b", "demo", "api", "", now),
		mkCR("c", "other", "web", "", now),
	)
	resp := mustGet(t, api, "/api/v1/service?target.startswith=demo.")
	got := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		got = append(got, it.Target)
	}
	want := []string{"demo.api", "demo.web"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListServices_FilterFuzzy(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkCR("b", "demo", "api", "", now),
		mkCR("c", "other", "moon", "", now),
	)
	// "dwb" is a subsequence of "demo.web" only.
	resp := mustGet(t, api, "/api/v1/service?target.fuzzy=dwb")
	if len(resp.Items) != 1 || resp.Items[0].Target != "demo.web" {
		t.Fatalf("fuzzy unexpected: %+v", resp.Items)
	}
	// Case insensitive.
	resp = mustGet(t, api, "/api/v1/service?target.fuzzy=DWB")
	if len(resp.Items) != 1 || resp.Items[0].Target != "demo.web" {
		t.Fatalf("fuzzy case-insensitive failed: %+v", resp.Items)
	}
}

func TestListServices_FilterCombined(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkCR("b", "demo", "api", "", now),
		mkCR("c", "other", "web-api", "", now),
	)
	// startswith narrows to demo.*, fuzzy "ai" hits demo.api only.
	resp := mustGet(t, api, "/api/v1/service?target.startswith=demo.&target.fuzzy=ai")
	if len(resp.Items) != 1 || resp.Items[0].Target != "demo.api" {
		t.Fatalf("combined: %+v", resp.Items)
	}
}

func TestListServices_Pagination(t *testing.T) {
	now := time.Now()
	objs := make([]runtime.Object, 0, 5)
	for i := range 5 {
		objs = append(objs, mkCR(fmt.Sprintf("cr-%d", i), "ns", fmt.Sprintf("svc-%d", i), "", now))
	}
	api := newTestAPI(t, objs...)

	page1 := mustGet(t, api, "/api/v1/service?limit=2")
	if len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1 unexpected: %+v", page1)
	}
	if page1.Items[0].Target != "ns.svc-0" || page1.Items[1].Target != "ns.svc-1" {
		t.Fatalf("page1 wrong order: %+v", page1.Items)
	}

	page2 := mustGet(t, api, "/api/v1/service?limit=2&cursor="+page1.NextCursor)
	if len(page2.Items) != 2 || page2.NextCursor == "" {
		t.Fatalf("page2 unexpected: %+v", page2)
	}
	if page2.Items[0].Target != "ns.svc-2" {
		t.Fatalf("page2 wrong start: %+v", page2.Items)
	}

	page3 := mustGet(t, api, "/api/v1/service?limit=2&cursor="+page2.NextCursor)
	if len(page3.Items) != 1 || page3.NextCursor != "" {
		t.Fatalf("page3 unexpected: %+v", page3)
	}
}

func TestListAlternatives(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkSvc("web", "demo", map[string]string{"app": "web"}),
		mkSvc("canary", "demo", map[string]string{"app": "web"}),
		mkSvc("blue", "demo", map[string]string{"app": "web"}),
		mkSvc("unrelated", "demo", map[string]string{"app": "other"}),
	)

	resp := mustGet(t, api, "/api/v1/service/demo.web/alternative")
	got := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		got = append(got, it.Target)
	}
	want := []string{".", "blue", "canary"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListAlternatives_FilterStartswith(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkSvc("web", "demo", map[string]string{"app": "web"}),
		mkSvc("canary-1", "demo", map[string]string{"app": "web"}),
		mkSvc("canary-2", "demo", map[string]string{"app": "web"}),
		mkSvc("blue", "demo", map[string]string{"app": "web"}),
	)
	resp := mustGet(t, api, "/api/v1/service/demo.web/alternative?target.startswith=canary")
	got := make([]string, 0, len(resp.Items))
	for _, it := range resp.Items {
		got = append(got, it.Target)
	}
	want := []string{"canary-1", "canary-2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListAlternatives_FilterFuzzy(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkSvc("web", "demo", map[string]string{"app": "web"}),
		mkSvc("canary-blue", "demo", map[string]string{"app": "web"}),
		mkSvc("blue", "demo", map[string]string{"app": "web"}),
	)
	resp := mustGet(t, api, "/api/v1/service/demo.web/alternative?target.fuzzy=cb")
	if len(resp.Items) != 1 || resp.Items[0].Target != "canary-blue" {
		t.Fatalf("fuzzy: %+v", resp.Items)
	}
}

func TestListAlternatives_NotFound(t *testing.T) {
	api := newTestAPI(t)
	mux := chi.NewRouter()
	api.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/service/missing.svc/alternative", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestListAlternatives_RoutingKeyConflictPicksOlder(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	api := newTestAPI(t,
		mkCR("new-cr", "ns", "newer-target", "shared.key", newer),
		mkCR("old-cr", "ns", "older-target", "shared.key", older),
		mkSvc("older-target", "ns", map[string]string{"app": "older-target"}),
		mkSvc("alt-old", "ns", map[string]string{"app": "older-target"}),
	)
	resp := mustGet(t, api, "/api/v1/service/shared.key/alternative")
	got := map[string]bool{}
	for _, it := range resp.Items {
		got[it.Target] = true
	}
	if !got["."] || !got["alt-old"] {
		t.Fatalf("expected alternatives from older CR, got %+v", resp.Items)
	}
}

func TestListTuples(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkSvc("web", "demo", map[string]string{"app": "web"}),
		mkSvc("canary", "demo", map[string]string{"app": "web"}),
		mkSvc("blue", "demo", map[string]string{"app": "web"}),
	)
	mux := chi.NewRouter()
	api.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tuple", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp TupleListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// One tuple per (target × alternative). 1 target, 3 alternatives ("." + 2 variants).
	if len(resp.Items) != 3 {
		t.Fatalf("expected 3 tuples, got %d: %+v", len(resp.Items), resp.Items)
	}
	// Sorted by Tuple. service is "demo/web", alternatives ".", "blue", "canary".
	wantTuples := []string{"demo/web:.", "demo/web:blue", "demo/web:canary"}
	for i, want := range wantTuples {
		if resp.Items[i].Tuple != want {
			t.Errorf("items[%d].Tuple=%q want %q", i, resp.Items[i].Tuple, want)
		}
		if resp.Items[i].Service != "demo/web" {
			t.Errorf("items[%d].Service=%q want demo/web", i, resp.Items[i].Service)
		}
		if resp.Items[i].RoutingKey != "demo.web" {
			t.Errorf("items[%d].RoutingKey=%q want demo.web", i, resp.Items[i].RoutingKey)
		}
	}
}

func TestListTuples_FilterFuzzy(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkSvc("web", "demo", map[string]string{"app": "web"}),
		mkSvc("canary", "demo", map[string]string{"app": "web"}),
		mkSvc("blue", "demo", map[string]string{"app": "web"}),
	)
	mux := chi.NewRouter()
	api.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tuple?fuzzy=can", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp TupleListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Alternative != "canary" {
		t.Fatalf("fuzzy=can unexpected: %+v", resp.Items)
	}
}

func TestListTuples_SourceCookieFromET(t *testing.T) {
	now := time.Now()
	api := newTestAPI(t,
		mkCR("a", "demo", "web", "", now),
		mkSvc("web", "demo", map[string]string{"app": "web"}),
		&routeprismv1alpha1.EdgeTransformation{
			ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "demo"},
			Spec: routeprismv1alpha1.EdgeTransformationSpec{
				Mode:         routeprismv1alpha1.EdgeModeRouter,
				SourceCookie: "x-route-prism",
				Target:       routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: "web"}},
			},
		},
	)
	mux := chi.NewRouter()
	api.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tuple", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp TupleListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) == 0 || resp.Items[0].SourceCookie != "x-route-prism" {
		t.Fatalf("expected sourceCookie populated from ET, got: %+v", resp.Items)
	}
}

func TestSubseqMatch(t *testing.T) {
	cases := []struct {
		s, p string
		want bool
	}{
		{"demo.web", "dwb", true},
		{"demo.web", "ow", true},
		{"demo.web", "wd", false},
		{"demo.web", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := subseqMatch(c.s, c.p); got != c.want {
			t.Errorf("subseqMatch(%q,%q)=%v want %v", c.s, c.p, got, c.want)
		}
	}
}

func TestPaginate_Cursor(t *testing.T) {
	in := []string{"a", "b", "c", "d"}
	page, next := paginate(in, "", 2)
	if fmt.Sprint(page) != "[a b]" {
		t.Fatalf("page=%v", page)
	}
	page2, next2 := paginate(in, decodeCursor(next), 2)
	if fmt.Sprint(page2) != "[c d]" || next2 != "" {
		t.Fatalf("page2=%v next2=%q", page2, next2)
	}
}

func TestApply_StartswithBoundary(t *testing.T) {
	sorted := []string{"alpha", "beta", "betas", "gamma"}
	lower := []string{"alpha", "beta", "betas", "gamma"}
	f := filter{startswith: "beta"}
	got := f.apply(sorted, lower)
	want := []string{"beta", "betas"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Prefix that matches nothing should return empty.
	got = filter{startswith: "zzz"}.apply(sorted, lower)
	if len(got) != 0 {
		t.Fatalf("zzz prefix non-empty: %v", got)
	}
}
