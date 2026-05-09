package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// handleWTChat handles /wt/chat WebTransport upgrade.
// Each connection spawns (or attaches to) a cc stream-json session.
// Bidi stream: browser → daemon = user prompt JSON lines,
//              daemon → browser = wire.Envelope JSON lines.
func handleWTChat(reg *session.Registry, wts *webtransport.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wtsess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go serveChat(r, wtsess, reg)
	}
}

func serveChat(r *http.Request, wtsess *webtransport.Session, reg *session.Registry) {
	defer wtsess.CloseWithError(0, "")

	ccSID := r.URL.Query().Get("sid")
	agentSess, err := reg.GetOrSpawn(wtsess.Context(), ccSID)
	if err != nil {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: err.Error()})
		return
	}

	realSID := agentSess.SessionID()
	sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindMessage, SessionID: realSID, Payload: map[string]any{
		"type":      "session_ready",
		"sessionId": realSID,
	}})

	// Subscribe to broadcast events for this session.
	subCh := make(chan wire.Envelope, 32)
	reg.Subscribe(realSID, subCh)
	defer reg.Unsubscribe(realSID, subCh)

	// Forward broadcast events to browser.
	go func() {
		for env := range subCh {
			env.SessionID = realSID
			sendEnvelope(wtsess, env)
		}
	}()

	// Read user prompts from browser bidi stream.
	stream, err := wtsess.AcceptStream(wtsess.Context())
	if err != nil {
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(io.LimitReader(stream, 4<<20))
	for scanner.Scan() {
		var msg struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil || msg.Text == "" {
			continue
		}
		if err := agentSess.SendPrompt(wtsess.Context(), msg.Text); err != nil {
			slog.Warn("send prompt", "err", err)
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
