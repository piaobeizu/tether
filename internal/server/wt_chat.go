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
		// Validate WT ticket before upgrading — Chrome WT CONNECT does not
		// carry cookies, so auth passes a short-lived ?ticket= instead.
		clientID := authState.ClientIDFromTicket(r)
		if clientID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		slog.Info("WT chat upgrade attempt", "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
		wtsess, err := wts.Upgrade(w, r)
		if err != nil {
			slog.Warn("WT chat upgrade failed", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("WT chat upgrade OK")
		go serveChat(r, wtsess, reg, clientID)
	}
}

func serveChat(r *http.Request, wtsess *webtransport.Session, reg *session.Registry, clientID string) {
	defer wtsess.CloseWithError(0, "")
	ctx := wtsess.Context()

	sid := r.URL.Query().Get("sid")
	providerName := r.URL.Query().Get("provider")

	// If resuming an existing session, verify ownership before spawning.
	// Only reject if the session EXISTS and is owned by a different client
	// (#83). If the session doesn't exist (e.g. server restart), silently
	// ignore the stale sid and fall through to spawn a fresh session below.
	if sid != "" && clientID != "" && reg.IsLive(sid) && !reg.IsOwner(sid, clientID) {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: "session owned by another client; use /wt/events to attach read-only"})
		return
	}

	entry, err := reg.GetOrSpawnEntry(ctx, sid, providerName)
	if err != nil {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: err.Error()})
		return
	}
	agentSess := entry.Session()

	// Subscribe by entry pointer BEFORE sending the first prompt. The sid is
	// only published AFTER cc consumes a prompt, so a sid-keyed Subscribe
	// after the prompt-reader goroutine starts would race with fanOut. By
	// attaching the channel to the entry directly, every event the agent
	// emits has a destination from the moment it's emitted.
	subCh := make(chan wire.Envelope, 32)
	entry.Subscribe(subCh)
	defer entry.Unsubscribe(subCh)

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
			// Record user message; sid is resolved after SessionID() returns.
			go func(text string) {
				sid := agentSess.SessionID()
				reg.RecordUserMessage(sid, text)
			}(msg.Text)
		}
	}()

	// Now wait for cc's system/init (it only arrives AFTER the first prompt
	// is delivered on cc stdin by the goroutine above).
	realSID := agentSess.SessionID()
	slog.Info("serveChat: SessionID resolved", "sid", realSID)
	if realSID == "" {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: "agent exited before emitting session id"})
		return
	}

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
