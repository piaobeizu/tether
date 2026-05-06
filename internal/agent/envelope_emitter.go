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

// WireEnvelope is the in-process projection of jsonl.Envelope that the
// attach socket / WT client subscribers consume. Identity-mapped from
// jsonl.Envelope today; kept as a separate type so future plumbing
// (rate-limit-event injection, daemon-broadcast catch-up cache) has a
// home that doesn't pollute the JSONL package.
type WireEnvelope struct {
	Kind              string                 `json:"kind"`
	ProviderType      string                 `json:"providerType"`
	SessionID         string                 `json:"sessionId"`
	Skill             string                 `json:"skill,omitempty"`
	PlaintextMetadata map[string]any         `json:"plaintextMetadata,omitempty"`
	CiphertextPayload []byte                 `json:"ciphertextPayload,omitempty"`
	SourceUUID        string                 `json:"sourceUuid,omitempty"`
}

// fromJSONL converts a jsonl.Envelope to the wire-facing shape.
func fromJSONL(e jsonl.Envelope) WireEnvelope {
	return WireEnvelope{
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
	sid    string
	in     <-chan jsonl.Envelope
	out    chan WireEnvelope
	cancel context.CancelFunc
	done   chan struct{}
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
func (e *EnvelopeEmitter) Subscribe(sid string) (<-chan WireEnvelope, func(), error) {
	if sid == "" {
		return nil, nil, errors.New("agent: Subscribe requires non-empty session id")
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, ErrEmitterClosed
	}
	in := e.watcher.Subscribe(sid)
	out := make(chan WireEnvelope, 64)
	subCtx, cancel := context.WithCancel(context.Background())
	sub := &emitterSub{
		sid:    sid,
		in:     in,
		out:    out,
		cancel: cancel,
		done:   make(chan struct{}),
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

// Stats returns a snapshot of the watcher's counters. Useful for
// tests + the health endpoint (eventually).
func (e *EnvelopeEmitter) Stats() (linesParsed, envelopesEmit, envelopesDrop int64) {
	st := e.watcher.StatsSnapshot()
	return st.LinesParsed, st.EnvelopesEmit, st.EnvelopesDrop
}

// relay is the per-subscriber goroutine: pulls jsonl.Envelopes from the
// watcher, projects to WireEnvelope, sends to out. Exits on ctx.Done()
// or when the watcher channel closes.
func (s *emitterSub) relay(ctx context.Context) {
	defer close(s.done)
	defer close(s.out)
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
