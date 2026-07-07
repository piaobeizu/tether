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

func serveControl(wtsess *webtransport.Session, _ *session.Registry) {
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
			resp, ok := RespondToControl(frame)
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

// RespondToControl computes the server's reply to a client control frame.
// Pure function (no I/O) so it can be unit-tested without a WT harness.
// Returns (nil, false) for frame kinds that don't warrant a reply.
func RespondToControl(f wire.ClientFrame) (*wire.ControlFrame, bool) {
	if f.Kind == wire.ClientFramePing {
		return &wire.ControlFrame{Kind: wire.ControlPong, TS: f.TS}, true
	}
	return nil, false
}
