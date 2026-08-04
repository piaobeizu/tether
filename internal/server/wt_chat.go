package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

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

// admitChat decides whether a chat connection carrying sid may attach, or must be
// turned away as someone else's session (#83). See the caller for the full
// reasoning about what this gate does and does not cover.
//
// It asks Registry.OwnedByOther rather than composing
// `IsLive(sid) && !IsOwner(sid, clientID)`. The composition additionally rejects a
// live session NOBODY owns yet — which since tether#54 is a state that lasts until
// the user types, and which this connection is entitled to join (see
// Registry.OwnedByOther). A connection with no sid, or with no client id to check
// against, is always admitted: there is nothing to conflict with.
func admitChat(reg *session.Registry, sid, clientID string) bool {
	if sid == "" || clientID == "" {
		return true
	}
	return !reg.OwnedByOther(sid, clientID)
}

func serveChat(r *http.Request, wtsess *webtransport.Session, reg *session.Registry, clientID string) {
	defer wtsess.CloseWithError(0, "")
	ctx := wtsess.Context()

	sid := r.URL.Query().Get("sid")
	providerName := r.URL.Query().Get("provider")
	// tether#52 — which registered workspace the agent should run IN, as an opaque
	// id from ~/.tether/workspaces.json. A query parameter, like sid and provider
	// above, because that is where this route's per-connection parameters already
	// live; internal/wire carries the server→browser envelope schema and has no
	// part in the handshake, so nothing here is generated and wire.gen.ts does not
	// move.
	//
	// It is an ID and not a path on purpose, and Registry.Attach is where that is
	// enforced: the agent's cwd decides which files it can read and write, so a
	// request that could name a directory would be choosing that for itself. An
	// unregistered id is refused below rather than falling back to
	// --workspace-root — a silent fallback would turn "rejected" into "redirected".
	wsID := r.URL.Query().Get("ws")

	// If attaching to an existing session, verify ownership first. Only reject
	// when the session EXISTS and is owned by a different client (#83).
	//
	// A sid the registry does NOT track falls through — but since tether#50 that
	// no longer means "discard it and start over": Attach hands it to
	// `cc --resume` to recover the conversation's context. Note the consequence
	// for this gate, whose INTENT is unchanged: because only LIVE sessions
	// are checked, it is inert for exactly the sids that now carry restorable
	// context. Acceptable under tether's one-human-many-devices model, but it is
	// no longer the complete barrier its name suggests.
	//
	// tether#55 then changed what "live" MEANS underneath it — a registered
	// session whose agent has exited now answers false — so the set of sids that
	// bypass this gate grew to include those. Deliberate, and not a loosening in
	// substance: such a session is unreachable by ANY client (its cc is gone),
	// and the identical sid a moment later, after eviction, already bypassed the
	// gate. Reading it the other way round is what the bug was — the gate
	// consulted the owner recorded on a corpse and so rejected the owner's own
	// reconnect from a second device.
	//
	// The decision itself is admitChat, so that it is testable without standing up
	// a WebTransport session — a review of tether#54 found this gate wrong, and a
	// gate whose only home is the middle of a WT handler cannot be pinned.
	//
	// KNOWN COVERAGE GAP, unchanged by that extraction: nothing pins that this call
	// site still MAKES the call. serveChat takes a concrete *webtransport.Session,
	// so reaching it from a test needs a real QUIC connection, and no such harness
	// exists here. Deleting these three lines leaves the suite green — as it did
	// before the extraction, when the whole gate lived inline. What changed is that
	// the decision is now pinned; the wiring is covered only by live_verify.
	// tether#63: this refusal had the SAME silent-reconnect-loop failure the
	// unknown-workspace one did — it arrives after wts.Upgrade has already
	// succeeded, so the browser's reconnect ladder (web/src/panes/chat/index.tsx)
	// saw a successful handshake, reset its attempt counter, and retried
	// forever against a session it will never be allowed to join. Classified
	// as ErrCodeSessionOwned, terminal, so the ladder stops instead.
	if !admitChat(reg, sid, clientID) {
		refuse(ctx, wtsess, wire.NewErrorEnvelope(wire.ErrCodeSessionOwned, "session owned by another client; use /wt/events to attach read-only"))
		return
	}

	// Attach, rather than GetOrSpawnEntry: a sid the registry no longer tracks
	// gets a `cc --resume <sid>` ATTEMPT, so a browser reload or a daemon restart
	// no longer costs the model its memory of the conversation (tether#50). The
	// attempt is recoverable — see Attachment.Resolve below.
	//
	// An error here is now also how an unusable workspace request ends (tether#52):
	// Attach validates wsID BEFORE spawning, so a forged or stale id closes the
	// connection with an error envelope and no subprocess anywhere. Note this is
	// reached only after admitChat above, so workspace selection cannot be used to
	// reach a session admission would have refused.
	att, err := reg.Attach(ctx, sid, providerName, wsID)
	if err != nil {
		slog.Warn("serveChat: attach refused", "sid", sid, "ws", wsID, "err", err)
		refuse(ctx, wtsess, errorEnvelope(err))
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
		refuse(ctx, wtsess, errorEnvelope(err))
		return
	}
	realSID := res.SID
	slog.Info("serveChat: SessionID resolved", "sid", realSID, "recovered", res.Recovered)

	// Claim ownership (CAS — first caller wins). Through the attachment, which
	// holds the *Entry, rather than by looking realSID up in the registry: a
	// lookup here is at best redundant and, for a provider that mints its own
	// session id, still able to miss a session that is right there — which this
	// function would read as a fatal ownership race and answer by dropping the
	// connection mid-answer. See Attachment.SetOwner.
	if clientID != "" {
		if !att.SetOwner(clientID) {
			refuse(ctx, wtsess, wire.NewErrorEnvelope(wire.ErrCodeSessionOwned, "session owned by another client; use /wt/events to attach read-only"))
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
	// The notice is live-only — it is never written to history, so if the frontend
	// drops it there is no way to get it back. session_ready (sent just above) is
	// what triggers the frontend's history refetch for the new sid, and that
	// refetch REPLACES the message list; until tether#57 the notice sat in that
	// same list and survived only because the refetch happens to be skipped while
	// a turn is streaming. The frontend now keeps notices in a separate slice the
	// refetch does not own (web/src/lib/store.ts, mergeTranscript), so the two can
	// no longer clobber each other. Keep it that way: a notice added to the
	// server-truth message list is a notice the next refetch can silently eat.
	if res.Notice {
		// Two different truths, so two different sentences (tether#52). A failed
		// resume means the conversation is GONE. A rebind means it is intact and still
		// resumable — just not from this workspace, because cc keys a transcript on the
		// directory it was created in. Telling someone their context "could not be
		// restored" when it is sitting safely in another workspace is a lie, and a
		// notice a user has caught lying is one they stop reading.
		text := "Started a new session — the previous conversation's context could not be restored."
		if res.Rebound {
			text = "Started a new session in this workspace — the previous conversation belongs to a different workspace and stays there."
		}
		sendEnvelope(wtsess, wire.Envelope{Kind: wire.KindMessage, SessionID: realSID, Payload: map[string]any{
			"type": "notice",
			"text": text,
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

// refusalDrainGrace is how long a refused chat connection is held open after
// its error envelope has been written, before serveChat's deferred
// CloseWithError destroys the session.
//
// MEASURED, not guessed (tether#63 live-verify, Chrome 1xx headless against
// this daemon over localhost):
//
//   - With no grace, the envelope arrived 0 times out of 10. sendEnvelope
//     writes it on a FRESH unidirectional stream and returns immediately, so
//     the session was destroyed in the same tick; the browser saw the stream
//     die as "WebTransportError: Connection lost" and never read a byte. That
//     is why tether#52's refusal looked silent from the UI — not because the
//     frontend discarded the message, but because the message never arrived.
//   - With this grace, 6 out of 6, observed ~1ms after the handshake. The wait
//     is therefore about two orders of magnitude larger than the delivery it
//     protects, which is the margin a non-local link needs.
//
// A close code and reason on CloseWithError are NOT an alternative: the same
// runs showed Chrome rejecting `WebTransport.closed` with "Connection lost"
// rather than resolving it with the code/reason, for both 0/"" and a custom
// 4001/"...", so nothing the daemon puts there is readable by the browser.
//
// The cost is one sleeping goroutine per refused connection for 300ms. That is
// strictly less than the connection it is already holding, and a caller who
// wants to spend the daemon's goroutines can open sessions that are NOT refused
// far more cheaply — so this is not a new amplifier.
const refusalDrainGrace = 300 * time.Millisecond

// refuse sends a classified refusal to the browser and then holds the session
// open just long enough for it to be delivered (see refusalDrainGrace).
//
// Every refusal on this route goes through here rather than calling
// sendEnvelope directly, so that "a refusal the daemon decided is a refusal the
// browser is told about" is a property of the one function they all share
// instead of something each site is trusted to remember. It is a convention,
// not something the type system enforces — wire.Envelope's fields are exported
// and internal/server/wt_shell.go still builds a KindError by hand for the
// shell pane's lock_held (a different channel, with extra fields and no
// frontend consumer). What IS structural is that no refusal on the CHAT route
// can be written without going through here or being obviously different from
// its four neighbours.
//
// The wait ends early if the client has already gone: ctx is the WebTransport
// session's, so on the ErrCodeConnectionClosed path — where the refusal exists
// only because the browser hung up — there is nobody left to deliver to and the
// grace would be 300ms spent on nothing.
//
// The browser's half of the contract is web/src/panes/chat/index.tsx
// (shouldReconnectAfterClose).
func refuse(ctx context.Context, wtsess *webtransport.Session, env wire.Envelope) {
	sendEnvelope(wtsess, env)
	select {
	case <-ctx.Done():
	case <-time.After(refusalDrainGrace):
	}
}

// errorEnvelope converts err into a classified wire.KindError envelope.
//
// A session.Refusal (from Registry.Attach / Attachment.Resolve — see
// resolveWorkspace, spawnEntry, and resolve's doc comments for what each code
// means) carries the code the daemon already decided; errors.As unwraps to
// find one even if err is wrapped further up the call stack. Anything else —
// an error type this function does not specifically recognize — becomes an
// UNCLASSIFIED envelope: ErrorCode("") has no entry in wire's terminalCodes
// table, so ErrorCode.Terminal() answers false for it. That default is
// deliberate, not an oversight: it means a failure mode nobody has
// classified yet degrades to "the browser reconnects and hopes", the
// pre-tether#63 behaviour, rather than bricking a connection on a code this
// function was never taught to recognize.
// The MESSAGE is always err.Error(), never ref.Error(): errors.As finds the
// INNERMOST Refusal, so taking its text would discard every layer wrapped
// around it on the way up. Attachment.resolve does exactly that wrapping —
// "resume %s failed and fresh spawn failed: %w" — and a browser told only
// "spawn: ..." has lost the half that says which recovery was being attempted.
// The code comes from the Refusal, the words come from the whole error.
func errorEnvelope(err error) wire.Envelope {
	var ref *session.Refusal
	if errors.As(err, &ref) {
		return wire.NewErrorEnvelope(ref.Code, err.Error())
	}
	return wire.NewErrorEnvelope(wire.ErrorCode(""), err.Error())
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
