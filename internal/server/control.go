package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// handleWTControl handles /wt/control WebTransport upgrade.
// Bidi stream: browser → daemon = wire.ClientFrame JSON lines (ping/action),
//
//	daemon → browser = wire.ControlFrame JSON lines (pong/...).
func handleWTControl(reg *session.Registry, wts *webtransport.Server, authState *auth.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Validate WT ticket before upgrading — same pattern as /wt/chat and
		// /wt/events; Chrome WT CONNECT does not carry cookies.
		clientID := authState.ClientIDFromTicket(r)
		if clientID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		wtsess, err := wts.Upgrade(w, r)
		if err != nil {
			slog.Warn("WT control upgrade failed", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go serveControl(wtsess, reg)
	}
}

func serveControl(wtsess *webtransport.Session, reg *session.Registry) {
	defer wtsess.CloseWithError(0, "")
	ctx := wtsess.Context()

	stream, err := wtsess.AcceptStream(ctx)
	if err != nil {
		slog.Warn("serveControl: AcceptStream err", "err", err)
		return
	}
	defer stream.Close()

	// Scan in a goroutine feeding a channel so the main loop can select on
	// ctx.Done() and unblock promptly on session cancellation (mirrors
	// serveEvents / serveChat; a bare scanner.Scan() blocks until the QUIC
	// stream delivers EOF/RST, which can lag on a half-open session).
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(io.LimitReader(stream, 4<<20))
		for scanner.Scan() {
			// scanner reuses its buffer; copy before handing off.
			b := make([]byte, len(scanner.Bytes()))
			copy(b, scanner.Bytes())
			select {
			case lines <- b:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			slog.Debug("serveControl: scan err", "err", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-lines:
			if !ok {
				return // stream closed
			}
			var frame wire.ClientFrame
			if err := json.Unmarshal(raw, &frame); err != nil {
				continue
			}
			resp, ok := routeClientFrame(reg, frame)
			if !ok {
				continue
			}
			b, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(stream, "%s\n", b); err != nil {
				return // write failure = client gone
			}
		}
	}
}

// routeClientFrame dispatches one decoded client frame to its handler and
// returns the reply to write back, if any.
//
// Split out of serveControl's read loop so the kind→handler wiring is
// reachable from a test without standing up a WebTransport session: the
// handlers themselves are already unit-testable, but "does a frame of this
// kind actually reach that handler" was not, and that is precisely the hop
// that silently does nothing when a case is missing.
func routeClientFrame(reg *session.Registry, f wire.ClientFrame) (*wire.ControlFrame, bool) {
	switch f.Kind {
	case wire.ClientFrameAction:
		handleActionFrame(reg, f)
		return nil, false
	case wire.ClientFrameResize:
		handleResizeFrame(reg, f)
		return nil, false
	}
	return RespondToControl(f)
}

// handleResizeFrame applies a client-reported terminal size to the PTY behind
// the shell f names (tether#68, tether#150).
//
// f.ShellID is passed through rather than interpreted here: since tether#121 one
// session can have a shell per tab and per device, and choosing between them is
// Registry.ResizeShell's job — it is the only place that knows which of them are
// still live.
//
// An unknown session or shell is an expected race, not a bug — /wt/control is
// not session-scoped, so a resize can arrive after its shell closed, or before
// it opened. Log at debug and drop; there is nothing to tell the user.
func handleResizeFrame(reg *session.Registry, f wire.ClientFrame) {
	if f.Cols == 0 || f.Rows == 0 {
		return // a zero dimension would blank the remote TUI
	}
	if err := reg.ResizeShell(f.SessionID, f.ShellID, f.Cols, f.Rows); err != nil {
		slog.Debug("serveControl: shell resize dropped",
			"sid", f.SessionID, "shell", f.ShellID, "cols", f.Cols, "rows", f.Rows, "err", err)
	}
}

// RespondToControl computes the server's reply to a client control frame.
// Pure function (no I/O) so it can be unit-tested without a WT harness.
// Returns (nil, false) for frame kinds that don't warrant a reply.
func RespondToControl(f wire.ClientFrame) (*wire.ControlFrame, bool) {
	if f.Kind == wire.ClientFramePing {
		return &wire.ControlFrame{Kind: wire.ControlPong, TS: f.TS}, true
	}
	return nil, false
}

// handleActionFrame routes an "action" ClientFrame (D-19 §5) to its target
// session, keyed by f.SessionID — the /wt/control channel is not otherwise
// session-scoped, so SessionID is the only routing key available.
//
//   - "approve" is delivered to the session's agent via
//     Registry.DeliverAction, wrapped as a __tether_action__ control
//     payload the emitting skill recognizes (docs/wire/fenced-contract.md
//     §5). The daemon never interprets DAG semantics itself (D-20).
//   - "pause" (tether#8 T9) routes to Registry.InterruptSession, which calls
//     the session's agent.Session.Interrupt() directly (NOT SendPrompt /
//     __tether_action__ — this is a transport-level interrupt, not a chat
//     message). For cc this writes a stream-json control_request on stdin
//     instead of SIGINT-ing the subprocess, so the process stays alive and
//     the session is immediately resumable.
//   - "rollback" and any unrecognized action are ignored: aihub has no
//     rollback primitive, so the button/action stays permanently unwired.
//
// An unknown or already-ended SessionID is a normal race (the frontend
// can't atomically know the session outlived the click), never a crash:
// DeliverAction's error is logged and dropped.
func handleActionFrame(reg *session.Registry, f wire.ClientFrame) {
	switch f.Action {
	case "approve":
		if err := reg.DeliverAction(f.SessionID, f.Action, f.BlockID, f.Skill); err != nil {
			slog.Warn("serveControl: action delivery failed",
				"sid", f.SessionID, "blockId", f.BlockID, "action", f.Action, "err", err)
		}
	case "pause":
		if err := reg.InterruptSession(f.SessionID); err != nil {
			slog.Warn("serveControl: interrupt failed",
				"sid", f.SessionID, "blockId", f.BlockID, "action", f.Action, "err", err)
		}
	default:
		// "rollback" and anything else: not wired, ignore.
	}
}
