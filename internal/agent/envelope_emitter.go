// Package agent — envelope emitter wires the cc JSONL watcher to the
// daemon's outbound channel.
//
// The watcher (internal/cc/jsonl) tails ~/.claude/projects/<encoded>/
// and produces a stream of jsonl.Envelope values per session. The
// envelope emitter:
//
//   - owns the *jsonl.Watcher lifetime,
//   - exposes one EnvelopeEmitter.Subscribe(sid) per attach session,
//   - returns a channel of fully-formed wire envelopes (just the
//     plaintext metadata + payload — Epic #5 encryption hooks in later),
//   - tears down per-subscriber state cleanly when the consumer closes.
//
// This sits alongside ClaudeCodeProvider rather than inside it because
// JSONL watching is provider-agnostic at the wire-shape level: §5.6 /
// §11.N already define the EVENT/HOOK/STATE → wire mapping and the
// daemon orchestrator drives the watcher independently of any one
// AgentProvider session lifecycle.

package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/piaobeizu/tether/internal/cc/jsonl"
)

// LocalEnvelope is the in-process projection of jsonl.Envelope that the
// daemon's local consumers (attach socket, eventually a daemon-side
// catch-up cache, etc.) receive. Identity-mapped from jsonl.Envelope
// today; kept as a separate type so future plumbing (rate-limit-event
// injection, daemon-broadcast catch-up cache) has a home that doesn't
// pollute the JSONL package.
//
// IMPORTANT — NOT THE SPEC §3.3.1 WIRE ENVELOPE.
// This struct is the SHAPE THE DAEMON SERVES TO LOCAL CLIENTS over the
// Unix attach socket. It carries plaintext metadata + (eventually)
// ciphertext payload but does NOT include the §3.3.1 fields needed for
// over-network delivery: id, fromDeviceId, toDeviceId, ts, keyVersion,
// nonce, AD-bound kind, transport-layer ciphertext binding. When
// QUIC/WT lands, the cross-device wire envelope is a separate type
// (provisional name: agent.WireEnvelope or transport.OutboundEnvelope)
// that wraps + signs/encrypts a LocalEnvelope. Code that reads "Local"
// here must not assume the bytes are wire-ready for the public
// internet.
type LocalEnvelope struct {
	Kind              string                 `json:"kind"`
	ProviderType      string                 `json:"providerType"`
	SessionID         string                 `json:"sessionId"`
	Skill             string                 `json:"skill,omitempty"`
	PlaintextMetadata map[string]any         `json:"plaintextMetadata,omitempty"`
	CiphertextPayload []byte                 `json:"ciphertextPayload,omitempty"`
	SourceUUID        string                 `json:"sourceUuid,omitempty"`
}

// fromJSONL converts a jsonl.Envelope to the wire-facing shape.
func fromJSONL(e jsonl.Envelope) LocalEnvelope {
	return LocalEnvelope{
		Kind:              string(e.Kind),
		ProviderType:      e.ProviderType,
		SessionID:         e.SessionID,
		Skill:             e.Skill,
		PlaintextMetadata: e.PlaintextMetadata,
		CiphertextPayload: e.CiphertextPayload,
		SourceUUID:        e.SourceUUID,
	}
}

// EnvelopeEmitter owns the JSONL watcher and the per-session fan-out.
//
// Lifecycle:
//
//	em, err := NewEnvelopeEmitter(ctx, EmitterConfig{...})
//	defer em.Close()
//	ch, cancel := em.Subscribe("sid-foo")
//	for env := range ch { ... }
//	cancel() // when done
//
// Concurrency: Subscribe / Close are safe from any goroutine. The
// underlying jsonl.Watcher runs its own drain goroutine; this struct
// adds one fan-out goroutine per Subscribe call.
type EnvelopeEmitter struct {
	watcher *jsonl.Watcher

	mu     sync.Mutex
	closed bool
	subs   []*emitterSub
}

// EmitterConfig configures NewEnvelopeEmitter.
type EmitterConfig struct {
	// ProjectsDir is the directory the watcher tails. v0.1 default
	// (when "") is ~/.claude/projects/. Tests override.
	ProjectsDir string

	// SubscriberBuffer overrides the per-subscriber channel buffer
	// (default jsonl.DefaultSubscriberBuffer).
	SubscriberBuffer int

	// OnError is called for non-fatal watcher errors. Optional.
	OnError func(path string, err error)

	// OnDrop is called when the watcher's per-session channel
	// back-pressures and an envelope is dropped. Optional.
	OnDrop func(sessionID string, kind string)
}

// emitterSub is one Subscribe registration with its own JSONL channel
// + outbound wire channel.
type emitterSub struct {
	sid     string
	in      <-chan jsonl.Envelope
	out     chan LocalEnvelope
	cancel  context.CancelFunc
	done    chan struct{}
	watcher *jsonl.Watcher // back-ref for self-deregistration on exit
}

// ErrEmitterClosed is returned by Subscribe after Close has run.
var ErrEmitterClosed = errors.New("agent: envelope emitter closed")

// NewEnvelopeEmitter constructs an emitter rooted at cfg.ProjectsDir
// (or ~/.claude/projects/ when empty). Returns an error on watcher
// init failure (root unreadable, fsnotify init).
func NewEnvelopeEmitter(ctx context.Context, cfg EmitterConfig) (*EnvelopeEmitter, error) {
	root := cfg.ProjectsDir
	if root == "" {
		// Caller is expected to have resolved $HOME; we don't reach
		// for it here to keep this constructor pure.
		return nil, errors.New("agent: EmitterConfig.ProjectsDir required (resolve ~/.claude/projects/ at the call site)")
	}

	wopts := jsonl.Options{
		SubscriberBuffer: cfg.SubscriberBuffer,
		OnError:          cfg.OnError,
	}
	if cfg.OnDrop != nil {
		wopts.OnDrop = func(sessionID string, kind jsonl.EnvelopeKind) {
			cfg.OnDrop(sessionID, string(kind))
		}
	}

	w, err := jsonl.New(ctx, root, wopts)
	if err != nil {
		return nil, err
	}
	return &EnvelopeEmitter{watcher: w}, nil
}

// Subscribe registers interest in a session's envelope stream. Returns
// the receive channel + a cancel func to tear down the subscription
// (closes the channel; safe to call multiple times).
//
// Empty sid is rejected — callers must supply the cc session_id.
func (e *EnvelopeEmitter) Subscribe(sid string) (<-chan LocalEnvelope, func(), error) {
	if sid == "" {
		return nil, nil, errors.New("agent: Subscribe requires non-empty session id")
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, ErrEmitterClosed
	}
	in := e.watcher.Subscribe(sid)
	out := make(chan LocalEnvelope, 64)
	subCtx, cancel := context.WithCancel(context.Background())
	sub := &emitterSub{
		sid:    sid,
		in:     in,
		out:    out,
		cancel: cancel,
		done:   make(chan struct{}),
		// Capture the watcher so relay can deregister itself on exit.
		// Without this, every Subscribe leaves a stale *subscriber in
		// jsonl.Watcher.subs[sid] until Watcher.Close — i.e. a leak per
		// attach connection. See watcher.Unsubscribe doc on the
		// receiver-termination contract (close-race rationale for why
		// we deregister but don't expect a channel-close signal).
		watcher: e.watcher,
	}
	e.subs = append(e.subs, sub)
	e.mu.Unlock()

	go sub.relay(subCtx)

	teardown := func() {
		sub.cancel()
		<-sub.done
	}
	return out, teardown, nil
}

// Close tears down the watcher + all subscriber goroutines. Safe to
// call multiple times.
func (e *EnvelopeEmitter) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	subs := append([]*emitterSub(nil), e.subs...)
	e.subs = nil
	e.mu.Unlock()

	for _, s := range subs {
		s.cancel()
		<-s.done
	}
	return e.watcher.Close()
}

// HasSubscribers reports whether at least one live subscriber is
// registered for `sid` (i.e. a daemon attach connection is currently
// streaming envelopes for that session). Returns false if the emitter
// is closed.
//
// Used by the GC stub-session sweeper (internal/daemon) to refuse
// deletion of any JSONL file whose session is currently being watched
// by a live attach client. Advisory — callers must combine with file-
// stat predicates (mtime old, line count low) so a subscriber
// attaching mid-sweep does not race against an already-stale file.
func (e *EnvelopeEmitter) HasSubscribers(sid string) bool {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return false
	}
	return e.watcher.HasSubscribers(sid)
}

// Stats returns a snapshot of the watcher's counters. Useful for
// tests + the health endpoint (eventually).
func (e *EnvelopeEmitter) Stats() (linesParsed, envelopesEmit, envelopesDrop int64) {
	st := e.watcher.StatsSnapshot()
	return st.LinesParsed, st.EnvelopesEmit, st.EnvelopesDrop
}

// Inject pushes a daemon-originated envelope to all subscribers of
// env.SessionID. Used by AuthBroker (and any future server-side event
// source) to fan a non-JSONL envelope into the same delivery path as
// watcher events.
//
// Best-effort delivery: if a subscriber's outbound channel is full, the
// envelope is dropped for that subscriber (the channel buffer of 64 is
// the same back-pressure model as the watcher relay). Returns
// ErrEmitterClosed only if the emitter has been Close()'d.
func (e *EnvelopeEmitter) Inject(env LocalEnvelope) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrEmitterClosed
	}
	// Snapshot subs to avoid holding the lock while sending. New
	// subscribers added concurrently won't see this envelope; that's
	// fine — Inject is a fan-out broadcast, not a replay log.
	subs := make([]*emitterSub, 0, len(e.subs))
	for _, s := range e.subs {
		if s.sid == env.SessionID {
			subs = append(subs, s)
		}
	}
	e.mu.Unlock()
	for _, s := range subs {
		select {
		case s.out <- env:
		default:
			// Subscriber slow / disconnected; drop. The watcher's
			// OnDrop reporting only fires on JSONL drops, so we don't
			// double-count here.
		}
	}
	return nil
}

// relay is the per-subscriber goroutine: pulls jsonl.Envelopes from the
// watcher, projects to LocalEnvelope, sends to out. Exits on ctx.Done()
// or when the watcher channel closes.
//
// On exit, deregisters from the underlying jsonl.Watcher so the
// watcher's subs map doesn't grow unboundedly across attach
// connect/disconnect cycles. See jsonl.Watcher.Unsubscribe for the
// non-close contract this relies on.
func (s *emitterSub) relay(ctx context.Context) {
	defer close(s.done)
	defer close(s.out)
	defer s.watcher.Unsubscribe(s.sid, s.in)
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-s.in:
			if !ok {
				return
			}
			wire := fromJSONL(env)
			select {
			case s.out <- wire:
			case <-ctx.Done():
				return
			}
		}
	}
}
