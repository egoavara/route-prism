/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package apiserver

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

// snapshot is an immutable, read-only materialized view of the routing
// surface. The dataset is small but the API is read-heavy, so we trade a
// little memory for lock-free O(log N) lookups by:
//   - keeping targets pre-sorted so equals/startswith become binary searches
//   - keeping a parallel lowercased slice so fuzzy/query never re-allocate
//   - resolving the routingKey-conflict winner once per rebuild (older CR
//     wins, mirroring the controller) so /alternative is just a map hit
type snapshot struct {
	targets      []string            // sorted unique routing keys
	targetsLower []string            // parallel slice, len(targets), lowercase
	altByTarget  map[string]altEntry // routingKey → alternatives
	metaByTarget map[string]targetMeta
	// remoteByAlt is a sidecar lookup keyed by "<routingKey>:<alt>". When
	// the alternative is backed by a RemoteRoute, the entry carries the
	// proxy reachability (mirrored from RR's UpstreamReachable condition).
	// Used by /service, /alternative, and /tuple responses to flag remote
	// variants without re-listing RemoteRoute on every request.
	remoteByAlt map[string]remoteFlag
	// hasRemoteByTarget is true when at least one alternative on that
	// routingKey is backed by a RemoteRoute. Surfaced on /service so
	// the dashboard can flag targets that have a remote variant without
	// fanning out to /alternative per row.
	hasRemoteByTarget map[string]bool
	// tuples is the flattened (target × alternative) pair list, one entry
	// per (routing-target, alternative). Sorted by Tuple ascending. Used
	// by /api/v1/tuple so the widget can search across both axes in a
	// single round-trip. tuplesLower is the parallel lowercase slice for
	// case-insensitive substring filtering.
	tuples      []TupleEntry
	tuplesLower []string
}

// remoteFlag is the sidecar value for remoteByAlt. Reachable is tristate:
// true (healthy), false (host PC offline), nil (status not yet reported).
type remoteFlag struct {
	reachable *bool
}

// TupleEntry is one (target × alternative) pair as exposed on the wire by
// /api/v1/tuple. The Tuple field is the user-visible search/display key.
type TupleEntry struct {
	// Service is "<namespace>/<service-name>" of the target Service.
	Service string `json:"service"`
	// Alternative is the variant Service name, or "." (SelfAlternative)
	// to mean "no override / route to target itself". Clients should
	// prefer the Self flag over a string compare against "."; the
	// sentinel value is an internal convention and may change.
	Alternative string `json:"alternative"`
	// Self is true when this row represents the unmarked / default path
	// (the target Service itself). Independent discriminator so callers
	// don't depend on the "." sentinel.
	Self bool `json:"self,omitempty"`
	// Tuple is "<service>:<alternative>" — both human-display and
	// fuzzy-search input.
	Tuple string `json:"tuple"`
	// RoutingKey is the Baggage member key clients should set this entry
	// against (also the multi-tier cookie's per-entry key).
	RoutingKey string `json:"routingKey"`
	// SourceCookie is the cookie name an attached EdgeTransformation
	// lifts into Baggage. Empty when no EdgeTransformation is attached
	// (the tuple is still routable manually via the Baggage header).
	SourceCookie string `json:"sourceCookie,omitempty"`
	// Remote is true when this alternative is backed by a RemoteRoute —
	// traffic to it leaves the cluster for a developer's PC. Lets the
	// widget render the entry with an explicit "remote" affordance and
	// gate it on Reachable.
	Remote bool `json:"remote,omitempty"`
	// Reachable, when non-nil, mirrors the RemoteRoute's
	// UpstreamReachable condition. nil means "unknown" (status not yet
	// reported); false means the developer's PC is unreachable and
	// selecting this variant will return 5xx; true means traffic flows.
	// Always nil for non-Remote tuples.
	Reachable *bool `json:"reachable,omitempty"`
}

type altEntry struct {
	list  []string // sorted; "." (self) is included as the first sorted entry
	lower []string // parallel slice, len(list), lowercase
}

// targetMeta carries the routing knobs a client needs to actually steer
// traffic toward an alternative: the routingKey (Baggage member key /
// cookie multi-tier sub-key) and the sourceCookie (cookie name an
// EdgeTransformation lifts into Baggage). sourceCookie is empty when no
// EdgeTransformation is attached to this target.
type targetMeta struct {
	routingKey   string
	sourceCookie string
}

// emptySnapshot is the zero-value served before the first rebuild.
var emptySnapshot = &snapshot{
	altByTarget:       map[string]altEntry{},
	metaByTarget:      map[string]targetMeta{},
	remoteByAlt:       map[string]remoteFlag{},
	hasRemoteByTarget: map[string]bool{},
}

// Index keeps the latest snapshot accessible to handlers via atomic
// pointer swap. Readers never block; writers (Rebuild) take only the
// rebuild lock to serialize concurrent rebuilds.
type Index struct {
	cli  client.Client
	snap atomic.Pointer[snapshot]

	rebuildMu sync.Mutex
}

// NewIndex builds an empty Index that reads from the given (cached)
// client. Call Rebuild before serving requests, or wire it up via Server
// which does the initial rebuild plus informer-driven incremental refresh.
func NewIndex(c client.Client) *Index {
	idx := &Index{cli: c}
	idx.snap.Store(emptySnapshot)
	return idx
}

// Snapshot returns the current immutable view. Safe to call concurrently;
// the returned pointer must not be mutated.
func (i *Index) Snapshot() *snapshot {
	return i.snap.Load()
}

// Rebuild materializes a fresh snapshot from cache and atomically swaps
// it in. Concurrent rebuilds are serialized — only the latest result is
// observable. Cheap on small datasets; meant to be called from informer
// event handlers (debounced) and once at startup.
func (i *Index) Rebuild(ctx context.Context) error {
	i.rebuildMu.Lock()
	defer i.rebuildMu.Unlock()

	var crList routeprismv1alpha1.ContextRouteList
	if err := i.cli.List(ctx, &crList); err != nil {
		return err
	}
	var etList routeprismv1alpha1.EdgeTransformationList
	if err := i.cli.List(ctx, &etList); err != nil {
		return err
	}
	// (namespace, target Service name) → sourceCookie. Used to decorate
	// each owner ContextRoute with the cookie name a client should set
	// to drive traffic toward an alternative on this target.
	cookieByService := make(map[string]string, len(etList.Items))
	for j := range etList.Items {
		et := &etList.Items[j]
		if !et.DeletionTimestamp.IsZero() || et.Spec.SourceCookie == "" {
			continue
		}
		cookieByService[et.Namespace+"/"+et.Spec.Target.Service.Name] = et.Spec.SourceCookie
	}

	// (namespace/rrName) → reachable tristate. RemoteRoute's metadata.name
	// equals the variant Service name (the controller invariant), so we
	// can decorate any tuple whose alternative matches an RR in the same
	// namespace.
	var rrList routeprismv1alpha1.RemoteRouteList
	if err := i.cli.List(ctx, &rrList); err != nil {
		return err
	}
	type rrInfo struct{ reachable *bool }
	rrByNsName := make(map[string]rrInfo, len(rrList.Items))
	for j := range rrList.Items {
		rr := &rrList.Items[j]
		if !rr.DeletionTimestamp.IsZero() {
			continue
		}
		var reachable *bool
		for _, c := range rr.Status.Conditions {
			if c.Type != "UpstreamReachable" {
				continue
			}
			v := c.Status == metav1.ConditionTrue
			reachable = &v
			break
		}
		rrByNsName[rr.Namespace+"/"+rr.Name] = rrInfo{reachable: reachable}
	}

	// Resolve owner per routingKey: older creationTimestamp wins; ties by name.
	owners := make(map[string]*routeprismv1alpha1.ContextRoute, len(crList.Items))
	for j := range crList.Items {
		cr := &crList.Items[j]
		if !cr.DeletionTimestamp.IsZero() {
			continue
		}
		key := cr.EffectiveRoutingKey()
		cur, ok := owners[key]
		if !ok ||
			cr.CreationTimestamp.Before(&cur.CreationTimestamp) ||
			(cr.CreationTimestamp.Equal(&cur.CreationTimestamp) && cr.Name < cur.Name) {
			owners[key] = cr
		}
	}

	targets := make([]string, 0, len(owners))
	for k := range owners {
		targets = append(targets, k)
	}
	sort.Strings(targets)
	targetsLower := make([]string, len(targets))
	for j, t := range targets {
		targetsLower[j] = strings.ToLower(t)
	}

	altMap := make(map[string]altEntry, len(owners))
	metaMap := make(map[string]targetMeta, len(owners))
	remoteByAlt := map[string]remoteFlag{}
	hasRemoteByTarget := map[string]bool{}
	var tuples []TupleEntry
	for key, owner := range owners {
		entry, err := i.buildAltEntry(ctx, owner)
		if err != nil {
			return err
		}
		altMap[key] = entry
		cookie := cookieByService[owner.Namespace+"/"+owner.Spec.Target.Service.Name]
		metaMap[key] = targetMeta{
			routingKey:   key,
			sourceCookie: cookie,
		}
		service := owner.Namespace + "/" + owner.Spec.Target.Service.Name
		for _, alt := range entry.list {
			te := TupleEntry{
				Service:      service,
				Alternative:  alt,
				Tuple:        service + ":" + alt,
				RoutingKey:   key,
				SourceCookie: cookie,
			}
			if alt == SelfAlternative {
				te.Self = true
			} else {
				if info, ok := rrByNsName[owner.Namespace+"/"+alt]; ok {
					te.Remote = true
					te.Reachable = info.reachable
					remoteByAlt[key+":"+alt] = remoteFlag{reachable: info.reachable}
					hasRemoteByTarget[key] = true
				}
			}
			tuples = append(tuples, te)
		}
	}
	sort.Slice(tuples, func(i, j int) bool { return tuples[i].Tuple < tuples[j].Tuple })
	tuplesLower := make([]string, len(tuples))
	for j, t := range tuples {
		tuplesLower[j] = strings.ToLower(t.Tuple)
	}

	i.snap.Store(&snapshot{
		targets:           targets,
		targetsLower:      targetsLower,
		altByTarget:       altMap,
		metaByTarget:      metaMap,
		remoteByAlt:       remoteByAlt,
		hasRemoteByTarget: hasRemoteByTarget,
		tuples:            tuples,
		tuplesLower:       tuplesLower,
	})
	return nil
}

func (i *Index) buildAltEntry(ctx context.Context, owner *routeprismv1alpha1.ContextRoute) (altEntry, error) {
	sel, err := metav1.LabelSelectorAsSelector(&owner.Spec.Variants.Selector)
	if err != nil {
		// Invalid selector — surface as "self only" rather than failing the
		// entire snapshot. The controller will mark the CR Ready=False
		// independently; the API simply has nothing to suggest.
		return altEntry{
			list:  []string{SelfAlternative},
			lower: []string{SelfAlternative},
		}, nil
	}
	var svcList corev1.ServiceList
	if err := i.cli.List(ctx, &svcList,
		client.InNamespace(owner.Namespace),
		client.MatchingLabelsSelector{Selector: sel},
	); err != nil {
		return altEntry{}, err
	}
	list := make([]string, 0, len(svcList.Items)+1)
	list = append(list, SelfAlternative)
	for _, svc := range svcList.Items {
		if svc.Name == owner.Spec.Target.Service.Name {
			continue
		}
		list = append(list, svc.Name)
	}
	sort.Strings(list)
	lower := make([]string, len(list))
	for j, x := range list {
		lower[j] = strings.ToLower(x)
	}
	return altEntry{list: list, lower: lower}, nil
}

// debouncer coalesces a burst of triggers into a single delayed call.
// Used to avoid rebuilding the snapshot once per object during the
// initial informer sync, when controller-runtime fires Add for every
// pre-existing object in the cache.
type debouncer struct {
	delay time.Duration
	fn    func()

	mu    sync.Mutex
	timer *time.Timer
}

func newDebouncer(delay time.Duration, fn func()) *debouncer {
	return &debouncer{delay: delay, fn: fn}
}

func (d *debouncer) trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.delay, d.fn)
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}
