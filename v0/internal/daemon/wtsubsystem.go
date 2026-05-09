// wtsubsystem.go — watchdog-supervised owner of the WebTransport
// listener (slice #2 listener + slice #3 envelope dispatch + slice #4
// pair-glue wire-up).
//
// What this subsystem does:
//
//   - Boots a wt.Server on the configured addr (or the test-supplied
//     pre-bound listener).
//   - Wires a real SessionHandler that, per accepted WT session:
//     1. accepts the client-initiated control stream,
//     2. peeks the first JSON line to disambiguate pair.invite vs the
//     v0.1 SessionIDHeader,
//     3. on pair.invite → runs pair.Server.Run to completion (persists
//     deviceId + long-term key via pair.Registry, audits success/fail);
//     after pair.complete the daemon disconnects so the client can
//     reconnect with a SessionIDHeader{SessionID, DeviceID} for the
//     real session,
//     4. on SessionIDHeader → resolves the per-session shared key:
//     - DeviceID non-empty → load long-term key from pair.Registry,
//     - DeviceID empty / not-found → fall back via legacy keySource
//     (Config.WTKeySource), defaulting to wt.DevSharedKey,
//     5. subscribes to the daemon's envelope emitter for that sid,
//     6. opens the events stream + calls wt.PushEnvelopeStream until
//     ctx done / subscriber closed / write fails.
//
// Restart safety: like clientSubsystem, this stores config rather
// than a built *wt.Server. Each Run() builds a fresh listener so a
// crashed Serve loop recovers cleanly on the next watchdog restart.

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/pair"
	"github.com/piaobeizu/tether/internal/transport/wt"
	"github.com/piaobeizu/tether/internal/watchdog"
)

// wtSessionHandshakeTimeout caps how long we wait for the client to
// send the control-stream first frame (pair.invite OR SessionIDHeader).
// Generous enough for slow CI but short enough that a stalled / hostile
// peer doesn't pin a goroutine forever. The pair handshake itself
// applies its own per-stage timeouts on top of this.
const wtSessionHandshakeTimeout = 10 * time.Second

// wtPairHandshakeTimeout is the upper bound on a full pair flow on the
// control channel — pair.* frames have their own per-stage 30s/60s
// timeouts, but this caps a stalled pair from indefinitely holding the
// session goroutine.
const wtPairHandshakeTimeout = 90 * time.Second

// wtSubsystemConfig bundles the inputs the wt subsystem needs. addr +
// listener feed the wt.Server; the emitter + keySource drive the per-
// session handler. registry + pairServerFactory wire the pair handshake.
type wtSubsystemConfig struct {
	addr     string
	listener net.PacketConn
	emitter  *agent.EnvelopeEmitter
	// legacyKeySource is the back-compat hook from Config.WTKeySource —
	// used when the peer sends SessionIDHeader without a DeviceID.
	// nil → defaults to wt.DevSharedKey.
	legacyKeySource func(sid string) ([]byte, error)
	// registry is the pair-registry handle used both to (a) persist
	// successful pair runs and (b) resolve a deviceId → long-term-key
	// for sessions that announce a DeviceID. nil disables both paths
	// (legacy fallback only).
	registry *pair.Registry
	// pairServerFactory builds a fresh pair.Server per pair handshake.
	// Captures the daemon's identity, registry, and audit log. nil
	// disables inbound pair entirely (control-channel pair.invite is
	// rejected).
	pairServerFactory func() *pair.Server
	// authBroker is the per-daemon AuthBroker. When non-nil, the
	// per-session control-stream dispatcher (post-header) routes
	// incoming JSON lines whose `type == "auth.tool-decision"` to
	// authBroker.SubmitDecision. Nil disables control-frame
	// interception — non-decision lines (and all lines when nil) are
	// dropped silently to keep the wire forward-compatible.
	authBroker *agent.AuthBroker
	logf       func(format string, args ...any)
	warnf      func(format string, args ...any)
}

// wtSubsystem owns the lifetime of the WT listener under the watchdog.
type wtSubsystem struct {
	cfg wtSubsystemConfig
}

func (w *wtSubsystem) Name() string { return "wt" }

func (w *wtSubsystem) Run(ctx context.Context, hb func()) error {
	hb()

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
// It first peeks the leading JSON line on the control stream to decide
// between two modes:
//
//   - pair.invite (envelope shape with kind="pair.invite", keyVersion=0)
//     → run pair.Server.Run to completion, persist + audit, then return.
//     The peer reconnects with a SessionIDHeader for the real session.
//   - SessionIDHeader (with sessionId, optional deviceId) → fall through
//     to envelope dispatch using the resolved key.
func (w *wtSubsystem) handleSession(parentCtx context.Context, sess *wt.Session) {
	ctx, cancel := context.WithCancel(sess.Context())
	defer cancel()
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	hsCtx, hsCancel := context.WithTimeout(ctx, wtSessionHandshakeTimeout)
	ctrl, err := sess.Control(hsCtx)
	hsCancel()
	if err != nil {
		w.warnSession("accept control stream: %v", err)
		return
	}

	// Wrap with bufio so we can peek the first line and feed the same
	// reader into the pair driver if it's pair.invite (the pair.Server
	// reads its own stream of \n-line JSON envelopes).
	br := bufio.NewReader(ctrl)
	hsCtx, hsCancel = context.WithTimeout(ctx, wtSessionHandshakeTimeout)
	firstLine, err := readLineWithCtx(hsCtx, br)
	hsCancel()
	if err != nil {
		w.warnSession("read first frame: %v", err)
		return
	}

	if isPairInviteLine(firstLine) {
		w.handlePairInvite(ctx, ctrl, br, firstLine)
		return
	}

	hdr, err := wt.ParseSessionHeaderLine(firstLine)
	if err != nil {
		w.warnSession("parse session header: %v", err)
		return
	}
	w.handleSessionDispatch(ctx, sess, hdr, br)
}

// handlePairInvite drives the responder-side pair flow over the control
// stream. It feeds the peeked first line (the pair.invite envelope) +
// the rest of the bufio reader into a small adapter that re-emits
// firstLine before delegating to the underlying stream. Persistence and
// audit are handled inside pair.Server.Run via the registry/audit refs
// stashed on the factory closure.
func (w *wtSubsystem) handlePairInvite(ctx context.Context, ctrl io.ReadWriter, br *bufio.Reader, firstLine []byte) {
	if w.cfg.pairServerFactory == nil {
		w.warnSession("pair.invite received but no pair server configured (registry nil); rejecting")
		return
	}
	srv := w.cfg.pairServerFactory()

	// Stitch the peeked first line back onto the front of the stream
	// the pair.Server consumes. The Server's stream codec is line-
	// delimited JSON envelopes, so multireader of (firstLine bytes,
	// remainder of br) preserves message boundaries cleanly.
	stream := &prependedRW{
		r: io.MultiReader(strings.NewReader(string(firstLine)), br),
		w: ctrl,
	}

	pairCtx, pairCancel := context.WithTimeout(ctx, wtPairHandshakeTimeout)
	defer pairCancel()
	res, err := srv.Run(pairCtx, stream)
	if err != nil {
		// Audit/abort frames are emitted by pair.Server itself; just log
		// for operator visibility. ErrAlreadyPaired is the §14 Q2
		// re-pair=reject path — not a daemon bug, just an expected reject.
		if errors.Is(err, pair.ErrAlreadyPaired) {
			if w.cfg.logf != nil {
				w.cfg.logf("[daemon] pair: re-pair attempt rejected: %v", err)
			}
			return
		}
		w.warnSession("pair.Server.Run: %v", err)
		return
	}
	if w.cfg.logf != nil {
		w.cfg.logf("[daemon] pair success: peer=%s sas=%s", res.PeerDeviceID, res.SAS)
	}
	// Disconnect: pair clients reconnect with SessionIDHeader for the
	// real session. We just return — wt.Server's defer fires
	// closeWithError on session exit.
}

// handleSessionDispatch is the v0.1 envelope-dispatch path. Resolves
// the AEAD key (per-device via Registry or legacy fallback), subscribes
// to the emitter, and pushes envelopes onto the events stream.
//
// `ctrlReader` is the post-header control-stream reader (bufio.Reader
// wrapped around the WT control bidi). It's tied through here so the
// post-header control-frame dispatcher (auth.tool-decision routing →
// AuthBroker) consumes exactly what handleSession peeked off, with no
// risk of byte-stealing on a separate reader.
func (w *wtSubsystem) handleSessionDispatch(ctx context.Context, sess *wt.Session, hdr wt.SessionIDHeader, ctrlReader *bufio.Reader) {
	key, err := w.resolveKeyForSession(hdr)
	if err != nil {
		w.warnSession("key resolve sid=%s deviceId=%s: %v", hdr.SessionID, hdr.DeviceID, err)
		return
	}

	if w.cfg.emitter == nil {
		w.warnSession("no emitter configured (sid=%s); cannot dispatch", hdr.SessionID)
		return
	}
	envCh, teardown, err := w.cfg.emitter.Subscribe(hdr.SessionID)
	if err != nil {
		w.warnSession("emitter subscribe sid=%s: %v", hdr.SessionID, err)
		return
	}
	defer teardown()

	evStream, err := sess.Events(ctx)
	if err != nil {
		w.warnSession("open events stream sid=%s: %v", hdr.SessionID, err)
		return
	}
	defer func() { _ = evStream.Close() }()

	// Prefer the announced deviceId for AD binding; fall back to the
	// remote addr for legacy peers (matches pre-slice-#4 behavior).
	toDevice := hdr.DeviceID
	if toDevice == "" {
		toDevice = sess.RemoteAddr()
		if toDevice == "" {
			toDevice = "wt-peer"
		}
	}
	if w.cfg.logf != nil {
		w.cfg.logf("[daemon] wt session sid=%s peer=%s dispatching", hdr.SessionID, toDevice)
	}

	// Spawn the post-header control-frame dispatcher (C1).
	//
	// AppShell sends `auth.tool-decision` JSON lines on the WT control
	// stream after the SessionIDHeader. Without this loop the daemon
	// reads the header once and never the rest of the stream, so the
	// AuthBroker's Ask() call sits its full timeout (60s) and fail-
	// closes — every cc tool gets denied. This mirrors the UDS
	// dispatcher in attach_socket.go::readInputs (which already routes
	// auth.tool-decision pre-lock).
	//
	// Lifecycle: scanner exits when the control stream closes (peer
	// half-closes) OR ctx (the session ctx) is cancelled. We don't
	// block the dispatcher Run on this goroutine — the events push
	// loop exit drives session shutdown via teardown + evStream.Close.
	if w.cfg.authBroker != nil {
		go w.runControlDispatcher(ctx, hdr.SessionID, ctrlReader)
	}

	pushErr := wt.PushEnvelopeStream(ctx, evStream, envCh, wt.PushEnvelopeOptions{
		SharedKey:    key,
		FromDeviceID: "daemon-default",
		ToDeviceID:   toDevice,
	})
	if pushErr != nil && w.cfg.warnf != nil {
		w.cfg.warnf("[daemon] wt session sid=%s push: %v", hdr.SessionID, pushErr)
	}
}

// runControlDispatcher loops over the post-header control stream,
// decoding each \n-delimited JSON line and routing recognized
// control-frame kinds to the daemon's broker. Today only
// `auth.tool-decision` is wired; future control-frame kinds (e.g.
// reauthenticate, resume) extend the switch.
//
// Tolerant on failure: a malformed/non-matching line is dropped
// silently — we never tear the session down because a peer sent garbage
// on the control stream. Real errors (stream EOF, ctx done) exit the
// goroutine cleanly.
func (w *wtSubsystem) runControlDispatcher(ctx context.Context, sid string, br *bufio.Reader) {
	scanner := bufio.NewScanner(br)
	// Cap each control frame at 64 KiB. Auth-decision frames are
	// ~150 bytes today; a 64 KiB cap leaves ample slack for future
	// control kinds while preventing a hostile peer from forcing
	// unbounded buffer growth via a never-newline-terminated line.
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			// Mirror attach_socket.go::readInputs (lines 459-471) — try
			// to decode as the v0.1 control-frame envelope; on failure
			// drop the line silently. Non-JSON garbage and JSON-shaped
			// non-decision frames both fall through.
			var frame agent.AuthDecisionFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				continue
			}
			if frame.Type != agent.KindAuthToolDecision {
				continue
			}
			if w.cfg.authBroker == nil {
				continue
			}
			if subErr := w.cfg.authBroker.SubmitDecision(frame); subErr != nil {
				if w.cfg.warnf != nil {
					w.cfg.warnf("[daemon] wt control sid=%s: auth decision: %v", sid, subErr)
				}
			}
		}
		// scanner.Err() may be the WT stream-close error or a deadline
		// hit; both are normal session-end signals. Surface only at
		// debug level (logf, not warnf) to avoid log spam on routine
		// disconnects.
		if err := scanner.Err(); err != nil && w.cfg.logf != nil {
			w.cfg.logf("[daemon] wt control sid=%s: scanner exit: %v", sid, err)
		}
	}()
	select {
	case <-ctx.Done():
		// Session ctx cancelled — the underlying stream Read will
		// unblock when wt.Server tears the session down. We just stop
		// waiting; the goroutine exits on its own once Read returns.
		return
	case <-done:
		return
	}
}

// resolveKeyForSession picks the AEAD key for envelope dispatch. The
// resolution order is:
//
//  1. If hdr.DeviceID non-empty AND registry configured → load that
//     device's long-term key.
//  2. Else if legacyKeySource non-nil → call it with the sid.
//  3. Else default to wt.DevSharedKey.
//
// The pair.Registry path is the production future; the legacy paths
// stay for back-compat with the cross-stack tests + dev/smoke flows
// that haven't paired yet.
func (w *wtSubsystem) resolveKeyForSession(hdr wt.SessionIDHeader) ([]byte, error) {
	if hdr.DeviceID != "" && w.cfg.registry != nil {
		rec, err := w.cfg.registry.Load(pair.DeviceID(hdr.DeviceID))
		if err == nil {
			if len(rec.LongTermKey) != wt.SharedKeySize {
				return nil, fmt.Errorf("registry deviceId=%s ltk size %d (want %d)", hdr.DeviceID, len(rec.LongTermKey), wt.SharedKeySize)
			}
			out := make([]byte, wt.SharedKeySize)
			copy(out, rec.LongTermKey)
			return out, nil
		}
		if !errors.Is(err, pair.ErrNotFound) {
			return nil, fmt.Errorf("registry load deviceId=%s: %w", hdr.DeviceID, err)
		}
		// Not found: fall through to legacy. v0.1 keeps the smoke-test
		// path alive; v0.2 will reject.
		if w.cfg.logf != nil {
			w.cfg.logf("[daemon] wt session deviceId=%s not in registry; falling back to legacy key", hdr.DeviceID)
		}
	}
	if w.cfg.legacyKeySource != nil {
		k, err := w.cfg.legacyKeySource(hdr.SessionID)
		if err != nil {
			return nil, err
		}
		if len(k) != wt.SharedKeySize {
			return nil, fmt.Errorf("legacy key source returned %d bytes (want %d)", len(k), wt.SharedKeySize)
		}
		return k, nil
	}
	out := make([]byte, len(wt.DevSharedKey))
	copy(out, wt.DevSharedKey[:])
	return out, nil
}

// warnSession logs a session-level setup failure.
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

// isPairInviteLine returns true iff the JSON line decodes to an
// envelope shape with kind="pair.invite" (and keyVersion=0). We don't
// validate the inner ciphertext here — that's pair.Server's job; we
// just need a robust disambiguator from SessionIDHeader.
func isPairInviteLine(line []byte) bool {
	var probe struct {
		Kind       string `json:"kind"`
		KeyVersion int    `json:"keyVersion"`
		SessionID  string `json:"sessionId"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return false
	}
	// SessionIDHeader has sessionId; pair envelopes never do.
	if probe.SessionID != "" {
		return false
	}
	return probe.Kind == string(pair.KindInvite) && probe.KeyVersion == pair.KeyVersionPair
}

// readLineWithCtx reads a single \n-terminated line, respecting ctx
// cancellation. bufio.Reader.ReadBytes does not support deadlines so
// we run it on a goroutine and select.
func readLineWithCtx(ctx context.Context, br *bufio.Reader) ([]byte, error) {
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := br.ReadBytes('\n')
		ch <- res{l, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) && len(r.line) == 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, r.err
		}
		return r.line, nil
	}
}

// prependedRW adapts a (Reader, Writer) pair to io.ReadWriter. Used
// to feed pair.Server.Run a stream where the leading line has already
// been peeked off the underlying control bufio.Reader.
type prependedRW struct {
	r io.Reader
	w io.Writer
}

func (p *prependedRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *prependedRW) Write(b []byte) (int, error) { return p.w.Write(b) }
