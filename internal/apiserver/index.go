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
	// tuples is the flattened (target × alternative) pair list, one entry
	// per (routing-target, alternative). Sorted by Tuple ascending. Used
	// by /api/v1/tuple so the widget can search across both axes in a
	// single round-trip. tuplesLower is the parallel lowercase slice for
	// case-insensitive substring filtering.
	tuples      []TupleEntry
	tuplesLower []string
}

// TupleEntry is one (target × alternative) pair as exposed on the wire by
// /api/v1/tuple. The Tuple field is the user-visible search/display key.
type TupleEntry struct {
	// Service is "<namespace>/<service-name>" of the target Service.
	Service string `json:"service"`
	// Alternative is the variant Service name, or "." (SelfAlternative)
	// to mean "no override / route to target itself".
	Alternative string `json:"alternative"`
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
	altByTarget:  map[string]altEntry{},
	metaByTarget: map[string]targetMeta{},
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
			tuples = append(tuples, TupleEntry{
				Service:      service,
				Alternative:  alt,
				Tuple:        service + ":" + alt,
				RoutingKey:   key,
				SourceCookie: cookie,
			})
		}
	}
	sort.Slice(tuples, func(i, j int) bool { return tuples[i].Tuple < tuples[j].Tuple })
	tuplesLower := make([]string, len(tuples))
	for j, t := range tuples {
		tuplesLower[j] = strings.ToLower(t.Tuple)
	}

	i.snap.Store(&snapshot{
		targets:      targets,
		targetsLower: targetsLower,
		altByTarget:  altMap,
		metaByTarget: metaMap,
		tuples:       tuples,
		tuplesLower:  tuplesLower,
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
