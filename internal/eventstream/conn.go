package eventstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Backoff bounds for reconnection after a dropped connection.
const (
	backoffMin = 500 * time.Millisecond
	backoffMax = 30 * time.Second
)

// conn is a single org's WebSocket connection to the Admin API event stream. It
// reconnects with exponential backoff and re-runs replay on every (re)connect
// so events missed during a disconnect window are recovered.
type conn struct {
	m      *Manager
	slug   string
	apiKey string
	cancel context.CancelFunc
	done   chan struct{}
}

func newConn(m *Manager, slug, apiKey string) *conn {
	return &conn{m: m, slug: slug, apiKey: apiKey, done: make(chan struct{})}
}

// stop cancels the connection and blocks until its goroutine has exited.
func (c *conn) stop() {
	if c.cancel != nil {
		c.cancel()
	}
	<-c.done
}

// run maintains the connection until parent is cancelled, reconnecting with
// exponential backoff between drops.
func (c *conn) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	defer close(c.done)

	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := c.connectAndRead(ctx)
		if ctx.Err() != nil {
			return
		}
		// A connection that stayed up for a while resets the backoff, so a
		// single long-lived stream that blips does not inherit a huge delay.
		if time.Since(start) > backoffMax {
			backoff = backoffMin
		}
		c.m.log.V(1).Info("event stream disconnected; reconnecting",
			"org", c.slug, "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// connectAndRead dials the stream and reads until the connection fails or ctx
// is cancelled. It always requests replay so a reconnect recovers events missed
// while disconnected.
func (c *conn) connectAndRead(ctx context.Context) error {
	endpoint, err := c.wsURL()
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("X-API-Key", c.apiKey)

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	ws, resp, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %w (http %d)", c.slug, err, resp.StatusCode)
		}
		return fmt.Errorf("dial %s: %w", c.slug, err)
	}
	defer ws.Close()
	c.m.log.Info("event stream connected", "org", c.slug)

	// ReadMessage blocks and does not observe ctx; closing the socket when ctx
	// is cancelled unblocks it so the connection can shut down promptly. Close
	// is safe to call concurrently with ReadMessage (gorilla/websocket).
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-stopWatch:
		}
	}()

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		c.handleMessage(ctx, msg)
	}
}

// wsURL builds the org-scoped stream URL with the replay window, translating the
// http(s) base to ws(s).
func (c *conn) wsURL() (string, error) {
	base := c.m.serverURL
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	case strings.HasPrefix(base, "wss://"), strings.HasPrefix(base, "ws://"):
		// already a websocket scheme
	default:
		return "", fmt.Errorf("unsupported server URL scheme: %q", c.m.serverURL)
	}
	q := url.Values{}
	q.Set("replay", strconv.Itoa(c.m.replayCount))
	return fmt.Sprintf("%s/%s/events?%s", base, url.PathEscape(c.slug), q.Encode()), nil
}

// handleMessage extracts the resource_type, drops control frames and unmanaged
// types, and asks the Manager to enqueue the matching CRs.
func (c *conn) handleMessage(ctx context.Context, msg []byte) {
	rt := parseResourceType(msg)
	if rt == "" || !isManagedResource(rt) {
		return
	}
	c.m.dispatch(ctx, c.slug, rt)
}

// parseResourceType pulls resource_type out of either wire shape the stream
// emits: live CloudEvents envelopes carry it under "data", replayed audit
// records carry it at the top level. Control frames ({"type":"heartbeat"} /
// {"type":"connected"}) and anything without a resource_type return "".
func parseResourceType(msg []byte) string {
	var env struct {
		Type         string          `json:"type"`
		ResourceType string          `json:"resource_type"`
		Data         json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return ""
	}
	if env.Type == "heartbeat" || env.Type == "connected" {
		return ""
	}
	if env.ResourceType != "" {
		return env.ResourceType
	}
	if len(env.Data) > 0 {
		var d struct {
			ResourceType string `json:"resource_type"`
		}
		if json.Unmarshal(env.Data, &d) == nil {
			return d.ResourceType
		}
	}
	return ""
}
