// wtsubsystem.go — watchdog-supervised owner of the WebTransport
// listener (slice #2 listener + slice #3 envelope dispatch wire-up).
//
// What this subsystem does:
//
//   - Boots a wt.Server on the configured addr (or the test-supplied
//     pre-bound listener).
//   - Wires a real SessionHandler that, per accepted WT session:
//     1. accepts the client-initiated control stream,
//     2. reads the v0.1 sid header (`{"sessionId":"..."}\n`),
//     3. resolves the per-session shared key via Config.WTKeySource
//     (default DevSharedKey),
//     4. subscribes to the daemon's envelope emitter for that sid,
//     5. opens the events stream + calls wt.PushEnvelopeStream until
//     ctx done / subscriber closed / write fails.
//
// Restart safety: like clientSubsystem, this stores config rather
// than a built *wt.Server. Each Run() builds a fresh listener so a
// crashed Serve loop recovers cleanly on the next watchdog restart.
//
// v0.1 placeholder — the sid header is a pre-protocol step that the
// real pair handshake (slice #4, internal/pair/) supersedes. See
// internal/transport/wt/header.go for the migration story.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/transport/wt"
	"github.com/piaobeizu/tether/internal/watchdog"
)

// wtSessionHandshakeTimeout caps how long we wait for the client to
// send the control-stream sid header. Generous enough for slow CI but
// short enough that a stalled / hostile peer doesn't pin a goroutine
// forever. The pair handshake (slice #4) will tighten this further.
const wtSessionHandshakeTimeout = 10 * time.Second

// wtSubsystemConfig bundles the inputs the wt subsystem needs to do
// real envelope dispatch. addr + listener feed the wt.Server; the
// emitter + keySource drive the per-session handler.
type wtSubsystemConfig struct {
	addr      string
	listener  net.PacketConn
	emitter   *agent.EnvelopeEmitter
	keySource func(sid string) ([]byte, error)
	logf      func(format string, args ...any)
	warnf     func(format string, args ...any)
}

// wtSubsystem owns the lifetime of the WT listener under the watchdog.
//
// It holds *config* (Addr + an optional pre-bound packet conn for
// tests + the emitter and key source), not a *wt.Server — so a
// watchdog restart constructs a fresh listener rather than re-running
// a closed one.
type wtSubsystem struct {
	cfg wtSubsystemConfig
}

func (w *wtSubsystem) Name() string { return "wt" }

func (w *wtSubsystem) Run(ctx context.Context, hb func()) error {
	hb()

	// Per-session handler closure — captures the emitter + key source
	// + logger references from the subsystem config. Each accepted WT
	// session runs this in its own goroutine (Server.handleUpgrade).
	handler := func(sess *wt.Session) {
		w.handleSession(ctx, sess)
	}

	srv, err := wt.New(ctx, wt.Config{
		Addr:           w.cfg.addr,
		SessionHandler: handler,
	})
	if err != nil {
		return fmt.Errorf("wt subsystem: new: %w", err)
	}
	defer func() { _ = srv.Close() }()

	// Heartbeat side goroutine so the supervisor's deadlock detector
	// stays happy while we just sit waiting for ctx / Serve to return.
	hbDone := make(chan struct{})
	defer func() { <-hbDone }()
	go func() {
		defer close(hbDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				hb()
			}
		}
	}()

	if w.cfg.logf != nil {
		w.cfg.logf("[daemon] wt listener starting on %s", w.cfg.addr)
	}

	var serveErr error
	if w.cfg.listener != nil {
		serveErr = srv.ServeListener(ctx, w.cfg.listener)
	} else {
		serveErr = srv.Serve(ctx)
	}
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return serveErr
	}
	return nil
}

// handleSession is the per-session handler injected into the wt.Server.
// Lifecycle:
//
//  1. Accept the control bidi stream and read the v0.1 sid header
//     (placeholder — slice #4 pair handshake replaces this).
//  2. Resolve the per-session shared key via cfg.keySource (default
//     wt.DevSharedKey).
//  3. Subscribe to the emitter for that sid.
//  4. Open the events bidi stream and call PushEnvelopeStream until
//     ctx cancels, the subscriber closes, or the write side errors.
//
// Any failure before dispatch starts results in the session being
// closed (the wt.Server's defer fires closeWithError when this
// returns).
func (w *wtSubsystem) handleSession(parentCtx context.Context, sess *wt.Session) {
	// The session's own context cancels on peer-close; tying our work
	// to BOTH the daemon ctx and the session ctx means we exit as soon
	// as either side is done.
	ctx, cancel := context.WithCancel(sess.Context())
	defer cancel()
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	// 1. Accept the control stream + read the sid header.
	hsCtx, hsCancel := context.WithTimeout(ctx, wtSessionHandshakeTimeout)
	ctrl, err := sess.Control(hsCtx)
	hsCancel()
	if err != nil {
		w.warnSession("accept control stream: %v", err)
		return
	}
	// Don't close ctrl — the pair handshake (slice #4) will reuse it.
	// Closing here would force the client to open a fresh control
	// stream for the future handshake. For v0.1 we just stop reading
	// after the header line.
	sid, err := wt.ReadSessionIDHeader(ctrl)
	if err != nil {
		w.warnSession("read session id header: %v", err)
		return
	}

	// 2. Resolve the per-session shared key.
	key, err := w.resolveKey(sid)
	if err != nil {
		w.warnSession("key source for sid=%s: %v", sid, err)
		return
	}

	// 3. Subscribe to the envelope emitter.
	if w.cfg.emitter == nil {
		w.warnSession("no emitter configured (sid=%s); cannot dispatch", sid)
		return
	}
	envCh, teardown, err := w.cfg.emitter.Subscribe(sid)
	if err != nil {
		w.warnSession("emitter subscribe sid=%s: %v", sid, err)
		return
	}
	defer teardown()

	// 4. Open the events stream + push envelopes.
	evStream, err := sess.Events(ctx)
	if err != nil {
		w.warnSession("open events stream sid=%s: %v", sid, err)
		return
	}
	defer func() { _ = evStream.Close() }()

	// ToDeviceID for v0.1: we use the WT session's RemoteAddr as a
	// stable per-session identifier. Slice #4 will replace this with
	// the device-id learned from the pair handshake. The v0.1 value is
	// AD-bound on the wire but never compared against anything by the
	// receiver — it's a label, not an authentication factor.
	toDevice := sess.RemoteAddr()
	if toDevice == "" {
		toDevice = "wt-peer"
	}
	if w.cfg.logf != nil {
		w.cfg.logf("[daemon] wt session sid=%s peer=%s dispatching", sid, toDevice)
	}

	pushErr := wt.PushEnvelopeStream(ctx, evStream, envCh, wt.PushEnvelopeOptions{
		SharedKey:    key,
		FromDeviceID: "daemon-default",
		ToDeviceID:   toDevice,
	})
	if pushErr != nil && w.cfg.warnf != nil {
		w.cfg.warnf("[daemon] wt session sid=%s push: %v", sid, pushErr)
	}
}

// resolveKey returns the per-session AEAD key. Falls back to
// wt.DevSharedKey when no source is configured (v0.1 default).
func (w *wtSubsystem) resolveKey(sid string) ([]byte, error) {
	if w.cfg.keySource == nil {
		key := make([]byte, len(wt.DevSharedKey))
		copy(key, wt.DevSharedKey[:])
		return key, nil
	}
	k, err := w.cfg.keySource(sid)
	if err != nil {
		return nil, err
	}
	if len(k) != wt.SharedKeySize {
		return nil, fmt.Errorf("wt key source returned %d bytes (want %d)", len(k), wt.SharedKeySize)
	}
	return k, nil
}

// warnSession logs a session-level setup failure. Goes through warnf
// (always-on) when configured because session setup failures are rare
// and usually indicate a configuration / peer mismatch the operator
// must see; -v=false would otherwise hide them.
func (w *wtSubsystem) warnSession(format string, args ...any) {
	if w.cfg.warnf != nil {
		w.cfg.warnf("[daemon] wt session: "+format, args...)
	}
}

// wtSubsystemFactory wraps wtSubsystem under the SubsystemFactory
// signature daemon.Run consumes.
func wtSubsystemFactory(cfg wtSubsystemConfig) SubsystemFactory {
	return func() watchdog.Subsystem {
		return &wtSubsystem{cfg: cfg}
	}
}
