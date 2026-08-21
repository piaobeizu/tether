// internal/permission/manager.go
package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Timeout is the deadline for a pending permission request.
// Package-level var so tests can override it. Tests override this; not parallel-safe.
var Timeout = 60 * time.Second

// Request is the generalized permission check request.
type Request struct {
	ID        string          // populated by Manager.Add
	Source    string          `json:"source"`              // "claude_hook" | "mcp:<servername>"
	SessionID string          `json:"session_id,omitempty"`
	ToolName  string          `json:"tool_name"`
	Args      json.RawMessage `json:"tool_input"`
	TaskID    string          `json:"task_id,omitempty"`
}

// Decision is the outcome of a permission check.
type Decision struct {
	Allow  bool
	Reason string
}

// consumer names WHAT IS WAITING for a decision, and it is the whole basis on
// which WithdrawSession decides a request is no longer worth offering to a user
// (tether#137).
//
// It is derived from the CALL PATH, not from Request.Source, and that is the
// point rather than a shortcut. Source is a string chosen by whoever POSTed the
// request: the production endpoint defaults it to "unknown" (http.go's
// requestHandler is mounted at /api/v1/permission/request with that default, and
// the gate forwards the agent's hook payload, which carries no `source` key), so
// matching on it would be matching on a value the daemon does not control. The
// call path is structural — there are exactly two ways into the map, and they
// differ in who is blocked on the answer.
type consumer uint8

const (
	// consumerOutOfProcess: the decision leaves this daemon as an HTTP response
	// to a gate subprocess that the AGENT spawned, and the only thing that reads
	// that gate's exit code is the agent that spawned it. tether#134 §2.5
	// measured the whole chain: the gate is a grandchild, so it survives the
	// agent (exec.CommandContext kills one pid), the decision reaches it, it
	// exits 0 — and the process that would have acted on that exit code is
	// already dead. Such a request is answerable and inert, which is what
	// WithdrawSession exists to stop offering.
	consumerOutOfProcess consumer = iota
	// consumerInProcess: the decision is returned to a goroutine INSIDE this
	// daemon, blocked in Check. Its one production caller is the MCP gateway
	// (internal/mcp/gateway/gateway.go), whose tool call is waiting on Check's
	// return value — so the consumer is the daemon itself and it does not die
	// with any chat connection. tether#134 §2.7: an MCP-sourced request survives
	// a chat reload with its consumer intact, so it is STILL ANSWERABLE and must
	// never be withdrawn. WithdrawSession refuses to touch this kind, and
	// TestWithdrawSession_LeavesADaemonConsumedRequestAlone is what holds that.
	consumerInProcess
)

type entry struct {
	req      *Request
	decideCh chan Decision
	timer    *time.Timer
	// consumer is who is blocked on decideCh; see the type.
	consumer consumer
	// seq is the order Add saw this request in. It exists so Pending can hand
	// back a STABLE order: the map gives none, and the consumer (a chat client
	// being handed the outstanding requests it never saw — tether#132) renders
	// them as a queue, so a random order would shuffle the cards a reload
	// restores. Arrival order is the one the live path already produced.
	seq uint64
}

// Manager manages in-flight permission requests.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*entry
	// seq numbers requests in the order they were added; see entry.seq. A
	// counter rather than a timestamp because two requests from one parallel
	// tool batch can share a millisecond, and "which of these two came first"
	// is exactly what would then be lost.
	seq uint64
}

// New returns a ready Manager.
func New() *Manager {
	return &Manager{pending: make(map[string]*entry)}
}

// Add registers req, sets req.ID, and returns a channel that yields the decision.
//
// The caller is out of process: this is the path http.go's request handler takes,
// and the thing waiting on the returned channel is an HTTP response to an
// agent-spawned gate. See consumer for why that is recorded here rather than read
// off Request.Source, and WithdrawSession for what it is recorded FOR.
func (m *Manager) Add(req *Request) <-chan Decision {
	return m.add(req, consumerOutOfProcess)
}

func (m *Manager) add(req *Request, c consumer) <-chan Decision {
	req.ID = newID()
	ch := make(chan Decision, 1)
	e := &entry{req: req, decideCh: ch, consumer: c}
	m.mu.Lock()
	m.seq++
	e.seq = m.seq
	m.pending[req.ID] = e
	// Armed AFTER the insert and inside the same critical section the callback
	// takes, so the timer cannot fire against a map that does not hold this
	// entry yet. Armed before the insert (which is what this was) the callback
	// found nothing, did nothing, and left an entry with no deadline at all —
	// unreachable at the production Timeout of a minute, and reachable by any
	// test that shortens it, which is how the expiry rule would end up pinned by
	// a test whose fixture is the one thing that cannot happen in production.
	e.timer = time.AfterFunc(Timeout, func() {
		m.mu.Lock()
		if _, ok := m.pending[req.ID]; ok {
			delete(m.pending, req.ID)
			ch <- Decision{Reason: "timeout"}
		}
		m.mu.Unlock()
	})
	m.mu.Unlock()
	return ch
}

// Decide resolves a pending request. Returns false if id is unknown.
func (m *Manager) Decide(id string, allow bool, reason string) bool {
	m.mu.Lock()
	e, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	e.timer.Stop()
	e.decideCh <- Decision{Allow: allow, Reason: reason}
	return true
}

// GetPending returns the pending request, or nil if id is unknown / already decided.
func (m *Manager) GetPending(id string) *Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.pending[id]; ok {
		return e.req
	}
	return nil
}

// Pending returns every request still awaiting a decision, oldest first.
//
// # Why this exists (tether#132)
//
// A permission request reaches the browser as ONE broadcast envelope and
// nothing else. That broadcast is a non-blocking send onto each chat client's
// channel (session.Registry.BroadcastAll), so a tab that has stalled loses it
// outright — and then the tool call sits waiting for a decision nobody can see
// a prompt for. The requests themselves were never lost; they are right here,
// in `pending`. What was missing was any way to ASK, so a reconnecting client
// could be handed what it never saw. This is that way.
//
// # Expiry needs no filter here, and that is a fact about Add, not a hope
//
// Every route out of `pending` is a DELETE: Add's time.AfterFunc removes the
// entry when Timeout elapses, Decide removes it when someone answers, and both
// the blocking Check and http.go's request handler call Decide when their
// caller goes away. So membership in this map IS "still awaiting a decision",
// and a request this returns is one a browser can still usefully answer. A
// second `expiresAt > now` test here would be a copy of the timer's job, able
// to disagree with it.
//
// The snapshot is of the POINTERS, taken under the lock and read outside it.
// A *Request is not mutated after Add sets its ID (Decide and the timer only
// remove the entry that holds it), so the caller cannot observe a torn one.
func (m *Manager) Pending() []*Request {
	m.mu.Lock()
	es := make([]*entry, 0, len(m.pending))
	for _, e := range m.pending {
		es = append(es, e)
	}
	m.mu.Unlock()
	sort.Slice(es, func(i, j int) bool { return es[i].seq < es[j].seq })
	out := make([]*Request, len(es))
	for i, e := range es {
		out[i] = e.req
	}
	return out
}

// WithdrawnReason is the Decision.Reason a withdrawn request is resolved with.
// Exported so the one caller that has to recognise it (and a test) does not
// re-spell it.
const WithdrawnReason = "the session that asked for this has ended"

// WithdrawSession removes every request belonging to sid whose decision has
// nowhere left to go, resolves each one, and returns them oldest first
// (tether#137, spec §5-F item 1).
//
// # What "nowhere left to go" means, and why it is not "expired"
//
// The caller is Registry.teardown by way of internal/server/mux.go, and teardown
// runs once per session, from fanOut's defer, i.e. after the agent's Events()
// channel has closed AND drained. So by the time this is called the agent is
// gone, not suspected of being gone — which matters, because tether#134 §2.4
// measured an accidental 0–43s window in which a reconnect ADOPTS a still-live
// agent and its turn keeps running. A withdrawal driven from the disconnect
// instead of from teardown would take away prompts that were still answerable in
// exactly that window; driven from teardown it cannot, and that ordering is the
// only thing making this safe.
//
// The request is REMOVED rather than flagged. Pending's doc comment argues the
// reason and it applies unchanged here: every route out of the map is a delete,
// so membership IS "still awaiting a decision", and a second "but is it really
// answerable" predicate alongside it would be a copy of that rule able to
// disagree with it. Removing also releases the two things a flag would leave
// hanging — the blocked HTTP handler and the gate process behind it — instead of
// making them wait out the remaining timeout.
//
// # What it must not touch
//
// consumerInProcess entries, whatever their SessionID. Those are the MCP
// gateway's, and their consumer is this daemon (tether#134 §2.7): the tool call
// is still blocked in Check and still wants an answer, so withdrawing one would
// break a feature that works. Both production call sites happen to leave
// CallRequest.SessionID empty today, so a sid match alone would miss them by
// luck; this does not rely on that, because a call site that threaded the chat
// sid through would be an ordinary change to make.
//
// An empty sid withdraws nothing. Requests carry an empty SessionID whenever
// their producer did not name one, and "every unattributed request in the
// daemon" is not what any caller means by "this session's".
func (m *Manager) WithdrawSession(sid string) []*Request {
	if sid == "" {
		return nil
	}
	m.mu.Lock()
	var es []*entry
	for id, e := range m.pending {
		if e.consumer != consumerOutOfProcess || e.req.SessionID != sid {
			continue
		}
		es = append(es, e)
		delete(m.pending, id)
	}
	m.mu.Unlock()

	// Same order as Pending, for the same reason: the caller announces these to a
	// UI that rendered them as a queue.
	sort.Slice(es, func(i, j int) bool { return es[i].seq < es[j].seq })
	out := make([]*Request, len(es))
	for i, e := range es {
		// Deleted from the map under the lock above, so neither Decide nor the
		// AfterFunc callback can also send on this channel: both look the entry up
		// in the map first and find nothing. That is what makes this send safe on a
		// buffered-by-one channel that is never received from when the gate has
		// already timed out and gone.
		e.timer.Stop()
		e.decideCh <- Decision{Reason: WithdrawnReason}
		out[i] = e.req
	}
	return out
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Check implements a blocking permission check for use by the gateway.
// It adds the request and blocks until decided or ctx cancelled.
//
// Registered as consumerInProcess, because the thing waiting for the decision is
// this call itself — the MCP gateway's tool call, inside the daemon. That is what
// keeps WithdrawSession's hands off it; see the consumer constants.
func (m *Manager) Check(ctx context.Context, req *Request) (*Decision, error) {
	ch := m.add(req, consumerInProcess)
	select {
	case d := <-ch:
		return &d, nil
	case <-ctx.Done():
		m.Decide(req.ID, false, "context cancelled")
		return nil, ctx.Err()
	}
}
