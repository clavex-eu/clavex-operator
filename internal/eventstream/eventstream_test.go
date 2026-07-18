package eventstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
)

// ── parseResourceType ─────────────────────────────────────────────────────────

func TestParseResourceType(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"heartbeat control frame", `{"type":"heartbeat"}`, ""},
		{"connected welcome frame", `{"type":"connected","org_id":"x","slug":"acme"}`, ""},
		{"live CloudEvents nested data", `{"type":"com.clavex.audit.oidc_client.created","data":{"action":"oidc_client.created","resource_type":"oidc_client"}}`, "oidc_client"},
		{"replay record top-level", `{"action":"role.created","resource_type":"role","status":"success"}`, "role"},
		{"unmanaged type still parsed", `{"resource_type":"session"}`, "session"},
		{"no resource_type", `{"action":"user.login"}`, ""},
		{"malformed json", `{not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseResourceType([]byte(tc.msg)); got != tc.want {
				t.Errorf("parseResourceType(%s)=%q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsManagedResource(t *testing.T) {
	managed := []string{ResourceOIDCClient, ResourceRole, ResourceGroup, ResourceAuthPolicy, ResourceOrg, ResourceWebhook, ResourceIdentityProvider}
	for _, rt := range managed {
		if !isManagedResource(rt) {
			t.Errorf("%q should be managed", rt)
		}
	}
	for _, rt := range []string{"session", "user", "mfa", ""} {
		if isManagedResource(rt) {
			t.Errorf("%q should not be managed", rt)
		}
	}
}

// ── dispatch → enqueue ────────────────────────────────────────────────────────

func testManager(t *testing.T, serverURL string) *Manager {
	t.Helper()
	return NewManager(serverURL, func(context.Context, clavexv1alpha1.SecretRef, string) (string, error) {
		return "clv_testkey", nil
	}, logr.Discard())
}

func clientCR(name, orgSlug string) *clavexv1alpha1.ClavexClient {
	return &clavexv1alpha1.ClavexClient{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       clavexv1alpha1.ClavexClientSpec{OrgRef: orgSlug},
	}
}

// A relevant event enqueues a reconcile for exactly the CRs of that Kind in the
// event's org — and nothing for CRs in a different org.
func TestDispatch_RelevantEnqueuesMatchingOrg(t *testing.T) {
	m := testManager(t, "http://example")
	ch := m.Register(ResourceOIDCClient, func(context.Context) ([]Item, error) {
		return []Item{
			{Object: clientCR("a1", "orgA"), OrgSlug: "orgA"},
			{Object: clientCR("a2", "orgA"), OrgSlug: "orgA"},
			{Object: clientCR("b1", "orgB"), OrgSlug: "orgB"},
		}, nil
	})

	m.dispatch(context.Background(), "orgA", ResourceOIDCClient)

	got := drain(ch, 2, 200*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2 enqueued CRs for orgA, got %d", len(got))
	}
	for _, ev := range got {
		if ev.Object.GetName() == "b1" {
			t.Error("orgB CR must not be enqueued for an orgA event")
		}
	}
}

// Rapid repeated events for the same object (the operator's own write echoing
// back through the stream) collapse to a single reconcile within the debounce
// window, so a non-converging drift comparison cannot hammer the Admin API.
func TestDispatch_DebouncesEcho(t *testing.T) {
	m := testManager(t, "http://example")
	ch := m.Register(ResourceOIDCClient, func(context.Context) ([]Item, error) {
		return []Item{{Object: clientCR("a1", "orgA"), OrgSlug: "orgA"}}, nil
	})

	m.dispatch(context.Background(), "orgA", ResourceOIDCClient)
	m.dispatch(context.Background(), "orgA", ResourceOIDCClient) // echo, within window

	if got := drain(ch, 2, 200*time.Millisecond); len(got) != 1 {
		t.Fatalf("expected the echo to collapse to 1 enqueue, got %d", len(got))
	}
}

// An event whose resource_type the operator does not manage enqueues nothing.
func TestDispatch_UnmanagedTypeIgnored(t *testing.T) {
	m := testManager(t, "http://example")
	ch := m.Register(ResourceOIDCClient, func(context.Context) ([]Item, error) {
		return []Item{{Object: clientCR("a1", "orgA"), OrgSlug: "orgA"}}, nil
	})

	// "session" is never registered → no channel, dispatch is a no-op.
	m.dispatch(context.Background(), "orgA", "session")

	if got := drain(ch, 1, 100*time.Millisecond); len(got) != 0 {
		t.Errorf("unmanaged resource_type should enqueue nothing, got %d", len(got))
	}
}

func drain(ch <-chan event.GenericEvent, want int, timeout time.Duration) []event.GenericEvent {
	var out []event.GenericEvent
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	// Give a brief moment to catch any unexpected extra events.
	select {
	case ev := <-ch:
		out = append(out, ev)
	case <-time.After(20 * time.Millisecond):
	}
	return out
}

// ── reconnect + replay (integration) ──────────────────────────────────────────

// A dropped connection is re-established with backoff, and every (re)connect
// carries the replay parameter so events missed while disconnected are
// recovered. The recovered event triggers a reconcile without the poll.
func TestConn_ReconnectReplaysAndEnqueues(t *testing.T) {
	var connectCount int32
	var sawReplay atomic.Value // string
	sawReplay.Store("")

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawReplay.Store(r.URL.Query().Get("replay"))
		n := atomic.AddInt32(&connectCount, 1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if n == 1 {
			// Simulate a dropped connection immediately to force a reconnect.
			return
		}
		// On the reconnect, deliver a replayed event the client must act on.
		_ = ws.WriteMessage(websocket.TextMessage,
			[]byte(`{"action":"oidc_client.updated","resource_type":"oidc_client","status":"success"}`))
		// Hold the connection open until the client goes away.
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	m := testManager(t, srv.URL)
	ch := m.Register(ResourceOIDCClient, func(context.Context) ([]Item, error) {
		return []Item{{Object: clientCR("a1", "orgA"), OrgSlug: "orgA"}}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	c := newConn(m, "orgA", "clv_testkey")
	go c.run(ctx)
	defer func() { cancel(); c.stop() }()

	select {
	case ev := <-ch:
		if ev.Object.GetName() != "a1" {
			t.Errorf("unexpected enqueued object %q", ev.Object.GetName())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected a reconcile enqueued from the replayed event after reconnect")
	}

	if got := atomic.LoadInt32(&connectCount); got < 2 {
		t.Errorf("expected at least 2 connections (drop + reconnect), got %d", got)
	}
	if got := sawReplay.Load().(string); got != "50" {
		t.Errorf("expected replay=50 on connect, got %q", got)
	}
}
