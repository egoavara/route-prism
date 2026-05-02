/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

const targetWeb = "web"

func TestBaggageMatchRegex(t *testing.T) {
	const routingKey = "default.web"
	type tc struct {
		name      string
		svc       string
		baggage   string
		wantMatch bool
	}
	cases := []tc{
		{name: "single-member-match", svc: "foo", baggage: routingKey + "=foo", wantMatch: true},
		{name: "first-of-many", svc: "foo", baggage: routingKey + "=foo,a=b", wantMatch: true},
		{name: "middle-of-many", svc: "foo", baggage: "a=b," + routingKey + "=foo,c=d", wantMatch: true},
		{name: "last-of-many", svc: "foo", baggage: "a=b," + routingKey + "=foo", wantMatch: true},
		{name: "with-property", svc: "foo", baggage: routingKey + "=foo;p=1,a=b", wantMatch: true},
		{name: "wrong-svc", svc: "foo", baggage: routingKey + "=bar", wantMatch: false},
		{name: "substring-only-no-match", svc: "foo", baggage: routingKey + "=foobar", wantMatch: false},
		{name: "different-key", svc: "foo", baggage: "other=foo", wantMatch: false},
		{name: "empty", svc: "foo", baggage: "", wantMatch: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pat := baggageMatchRegex(routingKey, c.svc)
			// Envoy/Cilium GAMMA enforces FULL-string match on the header
			// value, so anchor the pattern here to mirror what the data
			// plane actually evaluates. A regex that only worked under
			// Go's default partial match would silently regress in-cluster
			// even though the unit test passed.
			re, err := regexp.Compile(`\A(?:` + pat + `)\z`)
			if err != nil {
				t.Fatalf("regex compile failed for %q: %v", pat, err)
			}
			got := re.MatchString(c.baggage)
			if got != c.wantMatch {
				t.Errorf("regex %q against %q = %v, want %v", pat, c.baggage, got, c.wantMatch)
			}
		})
	}
}

func TestBaggageMatchRegex_EscapesMetaInBothArgs(t *testing.T) {
	// Both routingKey and svc name go through regexp.QuoteMeta — verify
	// metacharacters in either don't leak into the compiled regex.
	pat := baggageMatchRegex("ns.with.dots", "foo.bar")
	re := regexp.MustCompile(`\A(?:` + pat + `)\z`)
	if re.MatchString("nsxwithxdots=fooxbar") {
		t.Error("metacharacters leaked into regex")
	}
	if !re.MatchString("ns.with.dots=foo.bar") {
		t.Error("escaped values should match themselves literally")
	}
}

func TestRenderNginxConf_EmbedsRoutingKey(t *testing.T) {
	et := &routeprismv1alpha1.EdgeTransformation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo"},
		Spec:       routeprismv1alpha1.EdgeTransformationSpec{SourceCookie: "x-route-prism"},
	}
	out, err := renderNginxConf(et, "demo.web", "10.43.0.10", "10.0.0.1", 80, nil, "")
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	// The lua block consumes both values as raw strings (no regex), so
	// they should appear verbatim in the rendered config.
	if !strings.Contains(out, `"demo.web"`) {
		t.Errorf("routing key not embedded in rendered config")
	}
	if !strings.Contains(out, `"x-route-prism"`) {
		t.Errorf("source cookie not embedded in rendered config")
	}
}

func TestRenderNginxConf_DefaultPodIP(t *testing.T) {
	et := &routeprismv1alpha1.EdgeTransformation{
		Spec: routeprismv1alpha1.EdgeTransformationSpec{SourceCookie: "x"},
	}
	out, err := renderNginxConf(et, "ns.target", "", "10.0.0.7", 80, nil, "")
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(out, `"10.0.0.7:80"`) {
		t.Errorf("default Pod IP not baked into config")
	}
}

func TestRenderNginxConf_NoWidgetByDefault(t *testing.T) {
	et := &routeprismv1alpha1.EdgeTransformation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo"},
		Spec:       routeprismv1alpha1.EdgeTransformationSpec{SourceCookie: "x"},
	}
	out, err := renderNginxConf(et, "demo.web", "", "10.0.0.1", 80, nil, "")
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for _, marker := range []string{"body_filter_by_lua_block", "prism_widget_ip_match", "prism_operator_api"} {
		if strings.Contains(out, marker) {
			t.Errorf("widget-disabled config should not contain %q", marker)
		}
	}
}

func TestRenderNginxConf_WidgetEnabled(t *testing.T) {
	et := &routeprismv1alpha1.EdgeTransformation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo"},
		Spec: routeprismv1alpha1.EdgeTransformationSpec{
			SourceCookie: "x-route-prism",
			Target:       routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: targetWeb}},
			WidgetInjection: &routeprismv1alpha1.WidgetInjectionSpec{
				Enable: true,
				HTTP:   routeprismv1alpha1.WidgetHTTPSpec{PathPrefix: ".rp"},
				Target: routeprismv1alpha1.WidgetTargetSpec{Match: routeprismv1alpha1.WidgetMatchSpec{
					IPs:     []string{"10.0.0.0/8"},
					Methods: []string{"GET"},
					Headers: []routeprismv1alpha1.HeaderMatch{{Name: "X-Internal", Present: true}},
				}},
				Style: routeprismv1alpha1.WidgetStyleSpec{Mode: routeprismv1alpha1.WidgetStyleFloat},
			},
		},
	}
	out, err := renderNginxConf(et, "demo.web", "", "10.0.0.1", 80, nil, "ops.svc:8082")
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	wantContains := []string{
		"upstream prism_operator_api",
		"server ops.svc:8082;",
		"geo $prism_widget_ip_match",
		"10.0.0.0/8 1;",
		"location /.rp/widget/",
		"location /.rp/api/",
		"header_filter_by_lua_block",
		"body_filter_by_lua_block",
		`<script src="/.rp/widget/widget.js" data-route-prism-config=`,
		"X-Internal",
	}
	for _, m := range wantContains {
		if !strings.Contains(out, m) {
			t.Errorf("widget-enabled config missing %q", m)
		}
	}
	// JSON config blob is embedded as a Lua long-bracket string. Verify
	// it carries the expected fields and that single quotes are escaped
	// (so the runtime HTML attribute doesn't break out).
	if !strings.Contains(out, `"target":"web"`) {
		t.Errorf("widget config JSON missing target field")
	}
	if !strings.Contains(out, `"pathPrefix":".rp"`) {
		t.Errorf("widget config JSON missing pathPrefix field")
	}
}

func TestBuildWidgetTmplData_QuoteEscape(t *testing.T) {
	// Inputs containing a single quote should round-trip safely into
	// the JSON config blob (escaped to &#39;).
	et := &routeprismv1alpha1.EdgeTransformation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo"},
		Spec: routeprismv1alpha1.EdgeTransformationSpec{
			SourceCookie:    "x",
			Target:          routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: "ap'p"}},
			WidgetInjection: &routeprismv1alpha1.WidgetInjectionSpec{Enable: true},
		},
	}
	_, cfg, err := buildWidgetTmplData(et, "demo.app")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "'") {
		t.Errorf("config should not contain raw single quote: %q", cfg)
	}
	if !strings.Contains(cfg, "&#39;") {
		t.Errorf("expected &#39; escape, got %q", cfg)
	}
}

func TestHostnameGlobToLuaPattern(t *testing.T) {
	cases := map[string]string{
		"api.example.com": `^api%.example%.com$`,
		"*.example.com":   `^[^.]+%.example%.com$`,
	}
	for in, want := range cases {
		if got := hostnameGlobToLuaPattern(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

// renderCRHTTPRoute produces the Slate-style routing topology:
// ONE HTTPRoute on the target Service, with one rule per variant + catch-all.
func TestRenderCRHTTPRoute_Topology(t *testing.T) {
	cr := &routeprismv1alpha1.ContextRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-route", Namespace: "demo"},
		Spec: routeprismv1alpha1.ContextRouteSpec{
			Target: routeprismv1alpha1.TargetReference{
				Service: routeprismv1alpha1.ServiceReference{Name: targetWeb},
			},
			Variants: routeprismv1alpha1.VariantSelector{
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			},
		},
	}
	target := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: targetWeb, Namespace: "demo"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	variants := []corev1.Service{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-canary", Namespace: "demo"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-staged", Namespace: "demo"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
	}
	hr := renderCRHTTPRoute(cr, &target, variants, true)

	// ParentRef must be the target Service (group="").
	if len(hr.Spec.ParentRefs) != 1 {
		t.Fatalf("expected 1 parentRef, got %d", len(hr.Spec.ParentRefs))
	}
	pr := hr.Spec.ParentRefs[0]
	if pr.Group == nil || *pr.Group != "" || pr.Kind == nil || *pr.Kind != "Service" || pr.Name == nil || string(*pr.Name) != targetWeb {
		t.Errorf("parentRef should be Service web (core), got %+v", pr)
	}

	// Rules: one per variant (sorted by name) + final catch-all.
	if len(hr.Spec.Rules) != len(variants)+1 {
		t.Fatalf("expected %d rules, got %d", len(variants)+1, len(hr.Spec.Rules))
	}
	// First two rules must have a header match for the variant name; backendRef = that variant.
	wantOrder := []string{"web-canary", "web-staged"}
	for i, want := range wantOrder {
		rule := hr.Spec.Rules[i]
		if len(rule.Matches) != 1 || len(rule.Matches[0].Headers) != 1 {
			t.Fatalf("rule %d: expected 1 match w/ 1 header, got %+v", i, rule.Matches)
		}
		// Default routing key is "<ns>.<target-svc>" → "demo.web". Regex
		// is QuoteMeta-escaped so the literal `.` becomes `\.`.
		if rule.Matches[0].Headers[0].Value == nil || !strings.Contains(*rule.Matches[0].Headers[0].Value, `demo\.web=`+want) {
			t.Errorf("rule %d: header regex should reference %q, got %v", i, want, rule.Matches[0].Headers[0].Value)
		}
		if rule.BackendRefs[0].Name == nil || string(*rule.BackendRefs[0].Name) != want {
			t.Errorf("rule %d: backendRef should be %q, got %v", i, want, rule.BackendRefs[0].Name)
		}
	}
	// Catch-all (last rule): no Matches, backend → target.
	last := hr.Spec.Rules[len(hr.Spec.Rules)-1]
	if len(last.Matches) != 0 {
		t.Errorf("catch-all rule must have no matches, got %+v", last.Matches)
	}
	if last.BackendRefs[0].Name == nil || string(*last.BackendRefs[0].Name) != targetWeb {
		t.Errorf("catch-all backend should be target %q, got %v", targetWeb, last.BackendRefs[0].Name)
	}
}

func TestRenderCRHTTPRoute_NoVariants(t *testing.T) {
	cr := &routeprismv1alpha1.ContextRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "demo"},
	}
	target := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: targetWeb, Namespace: "demo"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	hr := renderCRHTTPRoute(cr, &target, nil, true)
	if len(hr.Spec.Rules) != 1 {
		t.Fatalf("expected only catch-all rule, got %d rules", len(hr.Spec.Rules))
	}
	if hr.Spec.Rules[0].BackendRefs[0].Name == nil || string(*hr.Spec.Rules[0].BackendRefs[0].Name) != targetWeb {
		t.Errorf("catch-all should backend the target")
	}
}

func TestRenderETHTTPRoute_SingleCatchAll(t *testing.T) {
	et := &routeprismv1alpha1.EdgeTransformation{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-edge", Namespace: "demo"},
	}
	target := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: targetWeb, Namespace: "demo"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	hr := renderETHTTPRoute(et, &target)
	if len(hr.Spec.Rules) != 1 {
		t.Fatalf("expected single catch-all rule, got %d", len(hr.Spec.Rules))
	}
	r := hr.Spec.Rules[0]
	if len(r.Matches) != 0 {
		// Note: CRD defaults inject path:/ but no header match. Tolerate that.
		for _, m := range r.Matches {
			if len(m.Headers) != 0 {
				t.Errorf("ET HTTPRoute rule should not have header matches; got %+v", m.Headers)
			}
		}
	}
	if r.BackendRefs[0].Name == nil || string(*r.BackendRefs[0].Name) != translatorName(et.Name) {
		t.Errorf("backend should be translator %q, got %v", translatorName(et.Name), r.BackendRefs[0].Name)
	}
}

// _ = gwv1 keeps the import warm if the test set evolves to reference gwv1 types.
var _ = gwv1.HTTPRoute{}
