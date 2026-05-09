package lock

import (
	"errors"
	"sync"
	"time"
)

// DefaultAutoReleaseWindow is the spec §11.D auto-release threshold: a
// holder that has been idle (no Touch / TryAcquire / ForceTakeover) for
// this long is treated as having released the lock, so a different client
// may take over without ForceTakeover.
const DefaultAutoReleaseWindow = 60 * time.Second

// ClientID identifies who holds (or wants) the lock. The Kind values are
// the only ones v0.1 emits — Mobile App ("mobile") and any local terminal
// attach ("terminal", whether the same machine or SSH'd in per §11.U).
// DeviceID is the per-pairing device identifier from devices.json so the
// audit log can attribute force-takeovers to specific phones / shells.
type ClientID struct {
	Kind     string
	DeviceID string
}

// Predefined Kind values. Other values are not rejected (the package is
// kind-agnostic) but the daemon's own handlers should stick to these so
// the audit log stays grep-able.
const (
	KindMobile   = "mobile"
	KindTerminal = "terminal"
)

// Equal reports whether two ClientIDs identify the same client. The zero
// ClientID is never equal to a non-zero ClientID; this is intentional so
// callers can use the zero value to mean "anonymous / not applicable"
// without it accidentally matching a real client.
func (c ClientID) Equal(o ClientID) bool {
	if c.Kind == "" && c.DeviceID == "" {
		return false
	}
	return c.Kind == o.Kind && c.DeviceID == o.DeviceID
}

// AcquireReason classifies how the lock was taken. It distinguishes the
// implicit-on-first-byte path from explicit Mobile App / terminal force
// takeover, and the implicit-on-stale-holder path that fires when the
// previous holder went idle past AutoReleaseWindow.
type AcquireReason string

const (
	// AcquireFirstByte: the lock was unheld and the requesting client
	// became holder.
	AcquireFirstByte AcquireReason = "first-byte"
	// AcquireStaleTakeover: the previous holder had been idle past
	// AutoReleaseWindow; the requesting client took over implicitly.
	AcquireStaleTakeover AcquireReason = "stale-takeover"
	// AcquireForce: the requesting client invoked ForceTakeover (Ctrl+\
	// Ctrl+T from terminal or "Take over" UI from Mobile App).
	AcquireForce AcquireReason = "force-takeover"
	// ReleaseAuto: not an acquire reason — used in History entries
	// emitted by Sweep when an idle holder is auto-released without
	// another client taking over.
	ReleaseAuto AcquireReason = "auto-release"
	// ReleaseExplicit: holder called Release.
	ReleaseExplicit AcquireReason = "release"
)

// TakeoverEvent records one state change in the lock's history. Holder
// fields use ClientID-by-value (not pointer) — a zero ClientID
// (Kind=="" && DeviceID=="") means "no holder", matching the wire
// shape used in audit logs.
type TakeoverEvent struct {
	At         time.Time
	Reason     AcquireReason
	PrevHolder ClientID // zero ClientID = was unheld
	NewHolder  ClientID // zero ClientID = now unheld (only on auto-release / explicit release)
}

// ErrInUse is returned by TryAcquire when an active (non-stale) holder
// owns the lock and the requester is not the holder. The caller is
// expected to surface a "session in use" message and offer the
// force-takeover affordance.
var ErrInUse = errors.New("lock: held by another client")

// ErrNotHolder is returned by Touch / Release when called with a client
// that does not currently hold the lock. Defense against caller bugs
// (mistakenly Touch'ing on behalf of a different attach session).
var ErrNotHolder = errors.New("lock: caller is not current holder")

// nowFunc is the time source. Tests override via WithClock; production
// uses time.Now.
type nowFunc func() time.Time

// LogSink is the durable-audit-log seam. Lock calls Append on every
// state change (acquire / release / takeover) so the caller can
// persist the event past process lifetime. Production wires this to
// NewJSONLLogSink; tests may use a recording fake; nil sink (default
// constructor with no WithLogSink) means in-memory History only,
// matching pre-§11.D-persistence behavior.
//
// Implementations MUST be safe for concurrent Append. Append is called
// under the Lock's internal mutex, so a slow sink will serialize
// state-machine progress — keep it cheap; spec §11.D anticipates low
// volume (a handful of events per session).
//
// An Append error is logged via the sink-error callback (if any, see
// JSONLLogSinkConfig) but does NOT roll back the state-machine
// transition: the in-memory state is the source of truth for the
// current session; the on-disk log is best-effort durability for
// post-mortem audit.
type LogSink interface {
	Append(ev TakeoverEvent) error
}

// Lock is the per-session writer-lock state machine. It's safe for
// concurrent use. Construct via New.
//
// The lock is a passive data structure: it does not run any goroutine.
// The auto-release window is checked at every TryAcquire / Touch, and
// Sweep is exposed for callers who want to actively reap idle holders
// (e.g. to fire an audit-log entry the moment a holder goes stale rather
// than waiting for the next acquire attempt).
type Lock struct {
	mu                sync.Mutex
	holder            ClientID  // zero = no holder
	lastWriteAt       time.Time // valid only when holder != zero
	autoReleaseWindow time.Duration
	now               nowFunc
	history           []TakeoverEvent
	sink              LogSink
	onSinkErr         func(error)
}

// Option configures Lock at construction time.
type Option func(*Lock)

// WithAutoReleaseWindow overrides the default 60s auto-release window.
// Useful for tests; production should keep the spec default.
func WithAutoReleaseWindow(d time.Duration) Option {
	return func(l *Lock) {
		if d > 0 {
			l.autoReleaseWindow = d
		}
	}
}

// WithClock overrides the time source. Test-only — production omits this
// and gets time.Now.
func WithClock(now func() time.Time) Option {
	return func(l *Lock) {
		if now != nil {
			l.now = now
		}
	}
}

// WithLogSink installs a durable LogSink. nil sink is silently ignored
// (back-compat with tests + the in-memory-only constructor). The sink
// receives one Append call per emitted TakeoverEvent, in event order.
//
// Append is called while the Lock's internal mutex is held — the sink
// MUST NOT call back into the same Lock or it will deadlock. JSONL
// file sinks (NewJSONLLogSink) only touch their own file lock, so
// they're safe.
func WithLogSink(sink LogSink) Option {
	return func(l *Lock) {
		if sink != nil {
			l.sink = sink
		}
	}
}

// WithSinkErrorHandler installs a callback invoked when LogSink.Append
// returns an error. Passing nil disables the callback (errors are
// silently dropped). Production wires this to a logger; the lock state
// machine never blocks or rolls back on sink errors — durability is
// best-effort, the in-memory state is authoritative.
func WithSinkErrorHandler(fn func(error)) Option {
	return func(l *Lock) {
		l.onSinkErr = fn
	}
}

// New constructs a Lock with no holder. Default auto-release window is
// DefaultAutoReleaseWindow; default clock is time.Now. Options override.
func New(opts ...Option) *Lock {
	l := &Lock{
		autoReleaseWindow: DefaultAutoReleaseWindow,
		now:               time.Now,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// AutoReleaseWindow reports the configured idle threshold. Exposed for
// daemon health pages / debug introspection; not part of the lock
// protocol.
func (l *Lock) AutoReleaseWindow() time.Duration {
	return l.autoReleaseWindow
}

// Holder returns the current holder ClientID and a flag indicating
// whether the lock is held. The flag is false (and the returned ClientID
// zero) when the lock is unheld OR the current holder has gone stale
// past AutoReleaseWindow at the moment of this call. Holder DOES advance
// the state machine when it observes a stale holder — that observation
// is recorded as an auto-release TakeoverEvent so callers reading the
// audit log see exactly when staleness was detected.
func (l *Lock) Holder() (ClientID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reapStaleLocked()
	if l.holderZero() {
		return ClientID{}, false
	}
	return l.holder, true
}

// IsHolder reports whether c is the current holder. Convenience wrapper;
// equivalent to comparing Holder() to c manually but cheaper because it
// doesn't allocate the returned tuple at the call site. Stale holders
// are reaped before the comparison, matching Holder's semantics.
func (l *Lock) IsHolder(c ClientID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reapStaleLocked()
	return !l.holderZero() && l.holder.Equal(c)
}

// LastWriteAt returns the timestamp of the most recent Touch (or the
// time of the most recent acquire if Touch hasn't been called since).
// Returns zero time when the lock is unheld. Useful for "lock idle" UI.
func (l *Lock) LastWriteAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holderZero() {
		return time.Time{}
	}
	return l.lastWriteAt
}

// TryAcquire is the implicit-acquire path the input gate calls before
// every write attempt. Returns nil and grants the lock to c when:
//   - the lock was unheld (records reason=AcquireFirstByte), or
//   - the previous holder was idle past AutoReleaseWindow (reason=
//     AcquireStaleTakeover, with the prior holder visible in the
//     emitted TakeoverEvent), or
//   - c is already the current holder (no state change, no event
//     appended; LastWriteAt is NOT updated — call Touch for that).
//
// Returns ErrInUse and does not mutate state when an active holder owns
// the lock and is someone other than c. The caller should surface an
// in-use UI affordance and may follow up with ForceTakeover.
//
// c.Equal(zero) clients are rejected with ErrNotHolder — anonymous
// callers must explicitly ForceTakeover (which also rejects the zero
// ClientID for the same reason: every audit-log entry needs a real
// attribution).
func (l *Lock) TryAcquire(c ClientID) error {
	if c.Kind == "" && c.DeviceID == "" {
		// Zero ClientID — never accept implicitly. Equal() also rejects
		// the zero value to keep "anonymous" from accidentally matching
		// a real client; that policy + this guard means daemon code can
		// safely use the zero ClientID as "not applicable" sentinel.
		return ErrNotHolder
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.holderZero() {
		l.grantLocked(c, now, AcquireFirstByte)
		return nil
	}
	if l.holder.Equal(c) {
		// Already holder. No-op; caller may follow with Touch on actual
		// write. Do not extend the window here — extending on TryAcquire
		// alone would let a buggy caller indefinitely "hold" without
		// ever writing.
		return nil
	}
	// Different client wants in. Reap stale holder if applicable.
	if now.Sub(l.lastWriteAt) > l.autoReleaseWindow {
		l.grantLocked(c, now, AcquireStaleTakeover)
		return nil
	}
	return ErrInUse
}

// ForceTakeover always grants the lock to c, regardless of current
// holder. Records reason=AcquireForce in the audit log so the prior
// holder shows up in History (spec §11.D "Audit log" requirement: every
// state change writes an entry — force takeovers in particular need to
// be visible).
//
// c must be a non-zero ClientID; zero is rejected with ErrNotHolder.
func (l *Lock) ForceTakeover(c ClientID) error {
	if c.Kind == "" && c.DeviceID == "" {
		return ErrNotHolder
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.holderZero() && l.holder.Equal(c) {
		// Already holder. Still slide the window forward — a force
		// takeover when you ARE the holder is most likely a UI button
		// re-press and we shouldn't surprise the user by leaving the
		// timer near expiry.
		l.lastWriteAt = l.now()
		return nil
	}
	l.grantLocked(c, l.now(), AcquireForce)
	return nil
}

// Touch slides the auto-release timer forward to "now". Called by the
// input gate AFTER each successful PTY write so an active holder doesn't
// get stale-reaped mid-session.
//
// Returns ErrNotHolder if c is not the current holder (defends against
// the caller mistakenly Touching on behalf of a different attach
// session). Does NOT auto-acquire; pair with TryAcquire to acquire then
// Touch on confirm.
func (l *Lock) Touch(c ClientID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holderZero() || !l.holder.Equal(c) {
		return ErrNotHolder
	}
	l.lastWriteAt = l.now()
	return nil
}

// Release explicitly drops the lock. Only the current holder may release
// (returns ErrNotHolder otherwise). Records a TakeoverEvent with reason
// =ReleaseExplicit so the audit trail shows a clean handoff distinct
// from auto-release.
//
// After Release returns nil, the lock is unheld and the next TryAcquire
// from any client takes it.
func (l *Lock) Release(c ClientID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holderZero() || !l.holder.Equal(c) {
		return ErrNotHolder
	}
	prev := l.holder
	l.holder = ClientID{}
	l.lastWriteAt = time.Time{}
	ev := TakeoverEvent{
		At:         l.now(),
		Reason:     ReleaseExplicit,
		PrevHolder: prev,
		NewHolder:  ClientID{},
	}
	l.history = append(l.history, ev)
	l.emitLocked(ev)
	return nil
}

// Sweep checks whether the current holder has been idle past
// AutoReleaseWindow and, if so, releases the lock and appends an
// auto-release TakeoverEvent. Returns true when a sweep actually fired
// (state changed); false otherwise (no holder, or holder still active).
//
// Sweep is optional — TryAcquire reaps stale holders lazily — but a
// daemon that wants to emit audit-log entries promptly (e.g. so the
// "lock idle for >60s" line appears at the moment it goes idle, not when
// the next attach client tries to write) can call Sweep on a periodic
// timer.
func (l *Lock) Sweep() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reapStaleLocked()
}

// History returns a defensive copy of the audit-log slice. Callers are
// free to mutate the returned slice; doing so does not affect the lock.
// Order is chronological (oldest first); each call re-snapshots so
// callers polling for new entries should track length.
func (l *Lock) History() []TakeoverEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]TakeoverEvent, len(l.history))
	copy(out, l.history)
	return out
}

// --- locked helpers (caller already holds l.mu) ---------------------------

// holderZero reports whether the holder field is the zero ClientID.
// Centralized so the "what counts as unheld" question has one answer.
func (l *Lock) holderZero() bool {
	return l.holder.Kind == "" && l.holder.DeviceID == ""
}

// grantLocked records a transition to a new holder. Used by all three
// acquire paths (first-byte, stale-takeover, force).
func (l *Lock) grantLocked(c ClientID, at time.Time, reason AcquireReason) {
	prev := l.holder
	l.holder = c
	l.lastWriteAt = at
	ev := TakeoverEvent{
		At:         at,
		Reason:     reason,
		PrevHolder: prev,
		NewHolder:  c,
	}
	l.history = append(l.history, ev)
	l.emitLocked(ev)
}

// reapStaleLocked checks staleness of the current holder and releases if
// applicable. Returns true when state actually changed. Idempotent.
func (l *Lock) reapStaleLocked() bool {
	if l.holderZero() {
		return false
	}
	if l.now().Sub(l.lastWriteAt) <= l.autoReleaseWindow {
		return false
	}
	prev := l.holder
	l.holder = ClientID{}
	l.lastWriteAt = time.Time{}
	ev := TakeoverEvent{
		At:         l.now(),
		Reason:     ReleaseAuto,
		PrevHolder: prev,
		NewHolder:  ClientID{},
	}
	l.history = append(l.history, ev)
	l.emitLocked(ev)
	return true
}

// emitLocked dispatches a freshly-recorded event to the configured
// LogSink (if any). Caller MUST already hold l.mu. Errors are routed
// to the sink-error handler (or dropped) — never bubble up.
//
// Synchronous on purpose: §11.D audit log is meant to be durable, so a
// crash immediately after the in-memory transition should still find
// the event on disk. The throughput cost is negligible (a few events
// per session). If profiling later shows otherwise we can buffer.
func (l *Lock) emitLocked(ev TakeoverEvent) {
	if l.sink == nil {
		return
	}
	if err := l.sink.Append(ev); err != nil && l.onSinkErr != nil {
		l.onSinkErr(err)
	}
}
