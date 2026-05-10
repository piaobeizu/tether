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

// handleWTChat handles /wt/chat WebTransport upgrade.
// Each connection spawns (or attaches to) a cc stream-json session.
// Bidi stream: browser → daemon = user prompt JSON lines,
//              daemon → browser = wire.Envelope JSON lines.
func handleWTChat(reg *session.Registry, wts *webtransport.Server, authState *auth.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("WT chat upgrade attempt", "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
		wtsess, err := wts.Upgrade(w, r)
		if err != nil {
			slog.Warn("WT chat upgrade failed", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("WT chat upgrade OK")
		go serveChat(r, wtsess, reg, authState)
	}
}

func serveChat(r *http.Request, wtsess *webtransport.Session, reg *session.Registry, authState *auth.State) {
	defer wtsess.CloseWithError(0, "")
	ctx := wtsess.Context()

	ccSID := r.URL.Query().Get("sid")
	// Derive client identity from verified JWT cookie (not the forgeable ?clientId= param).
	clientID := authState.ClientIDFromRequest(r)
	providerName := r.URL.Query().Get("provider")

	// If resuming an existing session, verify ownership before spawning.
	// Note: ccSID == realSID for resumed sessions (registry is keyed by the same ID).
	if ccSID != "" && clientID != "" && !reg.IsOwner(ccSID, clientID) {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: "session owned by another client; use /wt/events to attach read-only"})
		return
	}

	agentSess, err := reg.GetOrSpawn(ctx, ccSID, providerName)
	if err != nil {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: err.Error()})
		return
	}

	// Accept bidi stream BEFORE waiting for SessionID. cc's stream-json
	// `--input-format` mode does NOT emit system/init until the first user
	// prompt arrives. So we must read prompts from the browser stream and
	// pipe them to cc stdin first; cc will then emit system/init.
	slog.Info("serveChat: waiting for bidi stream")
	stream, err := wtsess.AcceptStream(ctx)
	if err != nil {
		slog.Warn("serveChat: AcceptStream err", "err", err)
		return
	}
	slog.Info("serveChat: bidi stream accepted")
	defer stream.Close()

	// Goroutine: read prompts from browser and forward to cc stdin.
	// This must run in parallel with the SessionID() wait below.
	go func() {
		scanner := bufio.NewScanner(io.LimitReader(stream, 4<<20))
		for scanner.Scan() {
			var msg struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil || msg.Text == "" {
				continue
			}
			slog.Info("chat prompt received", "len", len(msg.Text))
			if err := agentSess.SendPrompt(ctx, msg.Text); err != nil {
				slog.Warn("send prompt", "err", err)
			}
		}
	}()

	// Now wait for cc's system/init (it only arrives AFTER the first prompt
	// is delivered on cc stdin by the goroutine above).
	realSID := agentSess.SessionID()
	slog.Info("serveChat: SessionID resolved", "sid", realSID)

	// Claim ownership (CAS — first caller wins).
	if clientID != "" {
		if !reg.SetOwner(realSID, clientID) {
			sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: "session ownership race; retry"})
			return
		}
	}

	sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindMessage, SessionID: realSID, Payload: map[string]any{
		"type":      "session_ready",
		"sessionId": realSID,
	}})

	// Subscribe to broadcast events; forward to browser until ctx done.
	subCh := make(chan wire.Envelope, 32)
	reg.Subscribe(realSID, subCh)
	defer reg.Unsubscribe(realSID, subCh)

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-subCh:
			if !ok {
				return
			}
			env.SessionID = realSID
			sendEnvelope(wtsess, env)
		}
	}
}

func sendEnvelope(wtsess *webtransport.Session, env wire.Envelope) {
	stream, err := wtsess.OpenUniStreamSync(wtsess.Context())
	if err != nil {
		return
	}
	defer stream.Close()
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	fmt.Fprintf(stream, "%s\n", b)
}
