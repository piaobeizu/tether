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

	// If attaching to an existing session, verify ownership first. Only reject
	// when the session EXISTS and is owned by a different client (#83).
	//
	// A sid the registry does NOT track falls through — but since tether#50 that
	// no longer means "discard it and start over": Attach hands it to
	// `cc --resume` to recover the conversation's context. Note the consequence
	// for this gate, whose EXPRESSION is unchanged: because only LIVE sessions
	// are checked, it is inert for exactly the sids that now carry restorable
	// context. Acceptable under tether's one-human-many-devices model, but it is
	// no longer the complete barrier its name suggests.
	//
	// tether#55 then changed what IsLive MEANS underneath it — a registered
	// session whose agent has exited now answers false — so the set of sids that
	// bypass this gate grew to include those. Deliberate, and not a loosening in
	// substance: such a session is unreachable by ANY client (its cc is gone),
	// and the identical sid a moment later, after eviction, already bypassed the
	// gate. Reading it the other way round is what the bug was — the gate
	// consulted the owner recorded on a corpse and so rejected the owner's own
	// reconnect from a second device.
	if sid != "" && clientID != "" && reg.IsLive(sid) && !reg.IsOwner(sid, clientID) {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: "session owned by another client; use /wt/events to attach read-only"})
		return
	}

	// Attach, rather than GetOrSpawnEntry: a sid the registry no longer tracks
	// gets a `cc --resume <sid>` ATTEMPT, so a browser reload or a daemon restart
	// no longer costs the model its memory of the conversation (tether#50). The
	// attempt is recoverable — see Attachment.Resolve below.
	att, err := reg.Attach(ctx, sid, providerName)
	if err != nil {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: err.Error()})
		return
	}

	// Subscribe by attachment BEFORE sending the first prompt. The sid is
	// only published AFTER cc consumes a prompt, so a sid-keyed Subscribe
	// after the prompt-reader goroutine starts would race with fanOut. By
	// attaching the channel to the entry directly, every event the agent
	// emits has a destination from the moment it's emitted. Going through the
	// attachment (not the Entry) additionally re-registers subCh if a failed
	// resume swaps the Entry underneath us.
	subCh := make(chan wire.Envelope, 32)
	att.Subscribe(subCh)
	defer att.Unsubscribe(subCh)

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
			// An error here is EXPECTED on the failed-resume path: cc exited
			// without reading its stdin, so this write hits a broken pipe. The
			// attachment buffered the prompt and Resolve replays it onto a fresh
			// session, so this must stay a warning — bailing out here is exactly
			// the wedge tether#49 removed.
			if err := att.SendPrompt(ctx, msg.Text); err != nil {
				slog.Warn("send prompt", "err", err)
			}
			// Record the user message under the sid that actually answered it.
			// WaitSID (not the session's own SessionID) because a fallback
			// answers under a NEW id: the dead session reports "", and history
			// silently drops anything recorded against "" — which would lose the
			// user's own first message from the transcript a reload replays.
			go func(text string) {
				reg.RecordUserMessage(att.WaitSID(), text)
			}(msg.Text)
		}
	}()

	// Now confirm the session (cc's system/init only arrives AFTER the first
	// prompt is delivered on cc stdin by the goroutine above). If this connection
	// tried to resume and the resume failed, Resolve transparently falls back to
	// a fresh session and replays the buffered prompt(s) — so the user still gets
	// an answer to what they just typed.
	res, err := att.Resolve(ctx)
	if err != nil {
		slog.Warn("serveChat: session did not resolve", "err", err)
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: err.Error()})
		return
	}
	realSID := res.SID
	slog.Info("serveChat: SessionID resolved", "sid", realSID, "recovered", res.Recovered)

	// Claim ownership (CAS — first caller wins). Through the attachment, NOT
	// reg.SetOwner(realSID, …): the sid-keyed lookup loses a race with the
	// registry's re-key goroutine and would drop this connection mid-answer — see
	// Attachment.SetOwner.
	if clientID != "" {
		if !att.SetOwner(clientID) {
			sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindError, Payload: "session owned by another client; use /wt/events to attach read-only"})
			return
		}
	}

	sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindMessage, SessionID: realSID, Payload: map[string]any{
		"type":      "session_ready",
		"sessionId": realSID,
	}})

	// Tell the user their context is gone — but ONLY when they actually had some
	// (see HistoryStore.HasHistory for the gate's reasoning).
	//
	// Sent after session_ready by intent, not by guarantee: sendEnvelope opens a
	// NEW unidirectional stream per envelope and the browser drains streams
	// concurrently, so send order is not delivery order. Nothing here depends on
	// the ordering — the store's notice branch ignores env.SessionID — but do not
	// add anything that does without first making the transport ordered.
	//
	// KNOWN LIMIT: the notice is a live-only message, and session_ready triggers
	// the frontend's history refetch for the new sid, which REPLACES the message
	// list. Today the notice survives because that refetch is skipped while a turn
	// is streaming; if it ever resolves in a quiet moment the notice is silently
	// discarded with no way to bring it back. Tracked separately.
	if res.Notice {
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindMessage, SessionID: realSID, Payload: map[string]any{
			"type": "notice",
			"text": "Started a new session — the previous conversation's context could not be restored.",
		}})
	}

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
