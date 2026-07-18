// Package eventstream connects the operator to the Clavex Admin API's live
// audit event stream so that out-of-band mutations of managed resources trigger
// an immediate reconcile, instead of waiting for the periodic drift poll.
//
// The Admin API exposes one org-scoped WebSocket per org slug
// (GET /:org_slug/events, see internal/handler/stream.go). The operator holds
// org-scoped API keys (one per authSecretRef) and cannot open a single
// cross-org stream, so the Manager keeps one connection per distinct org slug
// found across all managed CRs — a small pool that grows and shrinks as CRs are
// added and removed.
//
// On a relevant event the Manager enqueues a reconcile for every CR of the
// matching Kind belonging to that org. Mapping is by (org, Kind) rather than by
// the event's resource_id: a reconcile is idempotent and the per-CR controller
// already does the precise Admin-API-entity → CR lookup, so a coarse enqueue is
// correct and avoids brittle key matching.
//
// The 5-minute (now 15-minute) controller poll is intentionally retained as a
// backstop for events missed beyond the stream's replay window — see
// requeueInterval in internal/controller.
package eventstream

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
)

// Resource types carried by the Admin API audit stream for operator-managed
// entities. They MUST match the server-side taxonomy in
// internal/handler/audit_helpers.go — they are the wire contract.
const (
	ResourceOIDCClient       = "oidc_client"
	ResourceRole             = "role"
	ResourceGroup            = "group"
	ResourceAuthPolicy       = "auth_policy"
	ResourceOrg              = "org"
	ResourceWebhook          = "webhook"
	ResourceIdentityProvider = "identity_provider"
)

// managedResourceTypes is the client-side filter: events whose resource_type is
// not in this set are ignored without touching the API server.
var managedResourceTypes = map[string]struct{}{
	ResourceOIDCClient:       {},
	ResourceRole:             {},
	ResourceGroup:            {},
	ResourceAuthPolicy:       {},
	ResourceOrg:              {},
	ResourceWebhook:          {},
	ResourceIdentityProvider: {},
}

// isManagedResource reports whether the operator reconciles this resource_type.
func isManagedResource(resourceType string) bool {
	_, ok := managedResourceTypes[resourceType]
	return ok
}

// Item is one CR discovered by a Lister: the object to enqueue plus the org
// coordinates the Manager needs to open and scope its stream connection.
type Item struct {
	Object    client.Object
	OrgSlug   string
	SecretRef clavexv1alpha1.SecretRef
	Namespace string
}

// Lister returns every CR of one Kind. The concrete type stays inside the
// closure (registered by each controller) so the Manager remains Kind-agnostic.
type Lister func(ctx context.Context) ([]Item, error)

// KeyResolver resolves the org-scoped Admin API key for a CR's authSecretRef.
// It is injected from main so the package does not depend on the secret plumbing.
type KeyResolver func(ctx context.Context, ref clavexv1alpha1.SecretRef, namespace string) (apiKey string, err error)

type registration struct {
	resourceType string
	ch           chan event.GenericEvent
	lister       Lister
}

// Manager owns the pool of per-org stream connections and the registry mapping
// each resource_type to the controller channel that enqueues its reconciles.
// It implements manager.Runnable and manager.LeaderElectionRunnable.
type Manager struct {
	serverURL     string
	resolveKey    KeyResolver
	log           logr.Logger
	discoverEvery time.Duration
	replayCount   int

	mu    sync.Mutex
	regs  map[string]*registration
	conns map[string]*conn // key: org slug

	// debounce collapses repeated enqueues of the same object within a short
	// window. It bounds the reconcile rate an event storm can drive — most
	// importantly the operator's own corrective write echoing back through the
	// stream — so a drift-comparison that fails to converge degrades to the
	// periodic poll's cadence instead of hammering the Admin API. Safe because
	// each reconcile re-reads the full live state, so collapsing N events into
	// one loses nothing.
	debounceMu  sync.Mutex
	lastEnqueue map[string]time.Time
}

// enqueueDebounce is the minimum interval between stream-driven reconciles of
// the same object. Echoes arrive within milliseconds, so this collapses them
// while keeping drift detection effectively instant.
const enqueueDebounce = 2 * time.Second

// NewManager creates a Manager targeting the given Admin API base URL.
func NewManager(serverURL string, resolveKey KeyResolver, log logr.Logger) *Manager {
	return &Manager{
		serverURL:     strings.TrimRight(serverURL, "/"),
		resolveKey:    resolveKey,
		log:           log.WithName("eventstream"),
		discoverEvery: 60 * time.Second,
		replayCount:   50,
		regs:          map[string]*registration{},
		conns:         map[string]*conn{},
		lastEnqueue:   map[string]time.Time{},
	}
}

// shouldEnqueue reports whether an object may be enqueued now, recording the
// time when it may. It returns false while the object is within its debounce
// window, collapsing echo storms (including the operator's own writes).
func (m *Manager) shouldEnqueue(key string) bool {
	m.debounceMu.Lock()
	defer m.debounceMu.Unlock()
	if last, ok := m.lastEnqueue[key]; ok && time.Since(last) < enqueueDebounce {
		return false
	}
	m.lastEnqueue[key] = time.Now()
	return true
}

// Register wires a Kind into the stream and returns the channel the controller
// must feed into its watch:
//
//	src := source.Channel(mgr.EventStream.Register(rt, lister), &handler.EnqueueRequestForObject{})
//	ctrl.NewControllerManagedBy(m).For(&T{}).WatchesRawSource(src).Complete(r)
func (m *Manager) Register(resourceType string, lister Lister) <-chan event.GenericEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &registration{
		resourceType: resourceType,
		ch:           make(chan event.GenericEvent, 256),
		lister:       lister,
	}
	m.regs[resourceType] = r
	return r.ch
}

// NeedLeaderElection ties the stream to leadership: only the leader reconciles,
// so only the leader should hold connections and enqueue work.
func (m *Manager) NeedLeaderElection() bool { return true }

// Start runs the connection-reconcile loop until ctx is cancelled. It satisfies
// manager.Runnable.
func (m *Manager) Start(ctx context.Context) error {
	m.log.Info("starting event stream manager", "server", m.serverURL)
	t := time.NewTicker(m.discoverEvery)
	defer t.Stop()

	m.reconcileConns(ctx)
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return nil
		case <-t.C:
			m.reconcileConns(ctx)
		}
	}
}

type orgCoord struct {
	slug      string
	secretRef clavexv1alpha1.SecretRef
	namespace string
}

// discover unions all managed CRs into the set of org slugs that should have a
// live connection, remembering one authSecretRef per slug to resolve its key.
func (m *Manager) discover(ctx context.Context) map[string]orgCoord {
	m.mu.Lock()
	regs := make([]*registration, 0, len(m.regs))
	for _, r := range m.regs {
		regs = append(regs, r)
	}
	m.mu.Unlock()

	out := map[string]orgCoord{}
	for _, r := range regs {
		items, err := r.lister(ctx)
		if err != nil {
			m.log.Error(err, "listing CRs for stream discovery", "resource_type", r.resourceType)
			continue
		}
		for _, it := range items {
			if it.OrgSlug == "" {
				continue
			}
			if _, ok := out[it.OrgSlug]; !ok {
				out[it.OrgSlug] = orgCoord{slug: it.OrgSlug, secretRef: it.SecretRef, namespace: it.Namespace}
			}
		}
	}
	return out
}

// reconcileConns opens connections for newly-seen orgs and closes those whose
// CRs have all been deleted.
func (m *Manager) reconcileConns(ctx context.Context) {
	desired := m.discover(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	for slug, coord := range desired {
		if _, ok := m.conns[slug]; ok {
			continue
		}
		apiKey, err := m.resolveKey(ctx, coord.secretRef, coord.namespace)
		if err != nil {
			m.log.Error(err, "resolving API key for org stream; will retry", "org", slug)
			continue
		}
		c := newConn(m, slug, apiKey)
		m.conns[slug] = c
		go c.run(ctx)
		m.log.Info("opened event stream", "org", slug)
	}

	for slug, c := range m.conns {
		if _, ok := desired[slug]; !ok {
			c.stop()
			delete(m.conns, slug)
			m.log.Info("closed event stream (no managed CRs remain)", "org", slug)
		}
	}
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for slug, c := range m.conns {
		c.stop()
		delete(m.conns, slug)
	}
}

// dispatch enqueues a reconcile for every CR of resourceType belonging to
// orgSlug. Unmanaged resource types are ignored.
func (m *Manager) dispatch(ctx context.Context, orgSlug, resourceType string) {
	m.mu.Lock()
	r := m.regs[resourceType]
	m.mu.Unlock()
	if r == nil {
		return // not a type this operator reconciles
	}

	items, err := r.lister(ctx)
	if err != nil {
		m.log.Error(err, "listing CRs to enqueue from stream event", "resource_type", resourceType)
		return
	}
	n := 0
	for _, it := range items {
		if it.OrgSlug != orgSlug {
			continue
		}
		key := resourceType + "|" + orgSlug + "|" + it.Object.GetNamespace() + "|" + it.Object.GetName()
		if !m.shouldEnqueue(key) {
			continue // within debounce window — collapse the echo
		}
		select {
		case r.ch <- event.GenericEvent{Object: it.Object}:
			n++
		default:
			// Channel full: the periodic poll remains as the backstop, so drop
			// rather than block the read loop.
			m.log.Info("enqueue channel full; relying on periodic requeue",
				"resource_type", resourceType, "org", orgSlug)
		}
	}
	if n > 0 {
		m.log.V(1).Info("enqueued reconcile from event stream",
			"resource_type", resourceType, "org", orgSlug, "count", n)
	}
}
