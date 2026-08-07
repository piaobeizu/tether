package wire

// Error classification for wire.KindError envelopes (tether#63).
//
// # The gap this closes
//
// Before this file, a KindError envelope's Payload was a bare string — cc's
// error text, or a hand-written sentence like "session owned by another
// client". That carries a message but no VERDICT: the browser cannot tell
// "this connection is permanently refused, stop retrying" from "this attempt
// failed, try again" without pattern-matching the string, which is exactly the
// kind of coupling this package exists to avoid (doc.go: "only types that
// cross the wire belong here"). tether#52 made an unknown/unregistered
// workspace fail closed — correctly — but the browser's reconnect ladder
// (web/src/panes/chat/index.tsx) resets its attempt counter on every
// SUCCESSFUL WebTransport handshake, and a refusal like this one arrives AFTER
// the handshake succeeds (wts.Upgrade in wt_chat.go has already returned by
// the time serveChat sends the error). So the counter never accumulates and
// the ladder retries forever, once a second, against a workspace that will
// never come back — an endless spinner where the daemon KNEW the retry was
// pointless the whole time.
//
// # Why disposition is its own wire field, not something the browser infers
//
// The obvious alternative is: ship the code, let the browser decide which
// codes are worth retrying. That puts the classification in TWO places that
// must be kept in sync across a wire boundary and, worse, across a language
// boundary — a Go const added here without a matching TypeScript case falls
// back to "unhandled" silently, no compiler in either language catches the
// gap, and the failure mode is the one this file exists to close (the daemon
// KNOWS the answer and does not say it). Sending Terminal as a bool alongside
// Code means the browser's ONE rule is "read the bit", never "recognize the
// string" — a rule that is still correct for a code the frontend has never
// heard of (see below), and one new terminal error case tomorrow needs a
// change in exactly one place: this file.
//
// # Why Terminal is derived from one table, not decided at each call site
//
// A call site constructing a KindError envelope by hand can set Terminal to
// whatever it wants, which means the true answer to "is unknown_workspace
// terminal" is scattered across however many places build one — and a future
// refusal added at a new call site is one copy-paste-and-forget away from
// shipping with the wrong bit. NewErrorEnvelope is the only constructor this
// package exposes for a KindError envelope, and it derives Terminal from
// terminalCodes below, so the classification lives in exactly one place a
// reviewer can read start to finish, and every caller gets it for free by
// naming a code instead of a bool.
//
// # Why an unclassified code defaults to retryable, not terminal
//
// terminalCodes is asserted exhaustive over every ErrorCode this package
// declares (errors_test.go's AST-based test parses the package's own source
// and fails the build if a new const is added without an entry), so
// "unclassified" should never happen for a code declared here. It CAN happen
// for a code this build does not know about: an older frontend talking to a
// newer daemon after a partial deploy, for instance. Defaulting a lookup miss
// to false (retryable) means that mismatch degrades to the old
// behaviour — reconnect and hope — rather than bricking a client that has
// done nothing wrong. Defaulting to true would mean any code neither side
// currently recognizes stops the browser retrying a connection that a fixed
// build might have accepted, which is the more dangerous failure to be wrong
// about: a stuck-retrying client wastes cycles, a client that gave up too
// early loses a reconnect it needed.
type ErrorCode string

const (
	// ErrCodeUnknownWorkspace means the requested workspace id is not in this
	// daemon's registry (session/workspace.go resolveWorkspace). Terminal: the
	// registry will not gain that id by retrying the same connection.
	ErrCodeUnknownWorkspace ErrorCode = "unknown_workspace"
	// ErrCodeNoWorkspaceRegistry means a workspace id was requested but this
	// daemon has no registry to check it against (it failed to load at
	// startup). Kept distinct from ErrCodeUnknownWorkspace on purpose — see
	// resolveWorkspace's doc comment — because an operator chasing "workspace
	// was deleted" and one chasing "registry failed to load at startup" are
	// chasing two different bugs. Terminal for the same reason: a registry
	// that failed to load once does not load itself by being asked again.
	ErrCodeNoWorkspaceRegistry ErrorCode = "no_workspace_registry"
	// ErrCodeUnknownProvider means the requested agent provider name is not
	// registered (session/registry.go spawnEntry). Terminal: the set of
	// registered providers is fixed at daemon startup, so retrying the same
	// request cannot change the answer.
	ErrCodeUnknownProvider ErrorCode = "unknown_provider"
	// ErrCodeSessionOwned means this sid is already claimed by a different
	// client (tether#54's OwnedByOther / Entry.setOwner).
	//
	// "Client" here is a DEVICE, not a tab: clientID comes from the login cookie
	// (internal/auth/middleware.go) and the SPA persists one per browser profile
	// in localStorage, so every tab of one browser shares it and admitChat lets
	// them all in. This refusal is therefore reached from a SECOND DEVICE, and
	// opening another tab is not a workaround for it.
	//
	// Terminal for this connection: Entry.ownerClientID has one writer and is
	// never reset, so the other device closing its tab does not release the
	// session — retrying the same chat connection is refused for as long as the
	// entry lives. /wt/events attaches read-only instead.
	ErrCodeSessionOwned ErrorCode = "session_owned_by_other"
	// ErrCodeSpawnFailed means provider.Spawn itself returned an error
	// (session/registry.go spawnEntry) — e.g. the agent binary is missing or
	// failed to start. Retryable: nothing here is a property of the browser's
	// request, and a transient exec failure (or an operator fixing the
	// binary) can succeed on the very next attempt.
	ErrCodeSpawnFailed ErrorCode = "spawn_failed"
	// ErrCodeConnectionClosed means the browser's connection was cancelled
	// before the agent session confirmed itself (session/attach.go resolve).
	// Retryable: the daemon did nothing wrong and there is nothing to fix —
	// reconnecting is the entire remedy, and is exactly what a browser that is
	// still around will do on its own.
	ErrCodeConnectionClosed ErrorCode = "connection_closed"
	// ErrCodeSessionUnconfirmed means a freshly spawned agent exited before
	// emitting a session id, with no resume to fall back to (session/attach.go
	// resolve's `!resuming` branch). MUST stay retryable — Attach's own doc
	// comment (internal/session/attach.go) is explicit that recovering a
	// failed resume depends on the browser's ordinary reconnect landing on the
	// `--resume` path next time; marking this terminal would turn a
	// recoverable hiccup (a bad spawn, a transient cc crash) into a dead end
	// the ladder refuses to retry out of.
	ErrCodeSessionUnconfirmed ErrorCode = "session_unconfirmed"
	// ErrCodeAgent means the agent process itself reported an error
	// (agent.EventError — session/registry.go translateEvent). It is the agent
	// speaking, not the daemon refusing the connection, and that is the whole of
	// what it says.
	//
	// It does NOT say the session survived, and until tether#80 this comment
	// claimed it did ("the session the browser is attached to is still alive and
	// usable"). That was wrong, and wrong in the direction that misleads: a
	// consumer reading it would present this error as a note about the current
	// turn on a healthy session. Every emit site is in
	// agent/opencode_provider.go (cc emits none at all), and they span three
	// materially different situations that arrive on the wire indistinguishable:
	//
	//   - session.error from opencode's event stream: a complaint about the turn;
	//     session alive, prompt delivered.
	//   - SendPrompt's busy branch, its resume-serve failure, and the two
	//     `opencode run` start failures: each emits and then returns nil, so the
	//     user's PROMPT WAS DROPPED, while the session stays alive.
	//   - watchServeExit: emits and then closes the event stream, so THE SESSION
	//     IS ALREADY GONE when this is read.
	//
	// Anything that needs to distinguish those needs a new code, not a reading of
	// this one. web/src/lib/store.ts's agent_error branch is written to that
	// constraint: it shows who spoke and what they said, and asserts nothing about
	// the session or the prompt.
	//
	// Retryable, and that verdict is unchanged and right at both ends of the
	// range: nothing here is a property of the browser's request, so a reconnect
	// is the correct response whether the agent merely complained or its serve
	// has exited.
	ErrCodeAgent ErrorCode = "agent_error"
	// ErrCodePromptUndelivered means the user's prompt did not reach any agent
	// and no further recovery is coming for it (session/attach.go reopen, on
	// every branch that gives up rather than the ones that hand the problem to
	// machinery which will retry).
	//
	// Named for the prompt rather than for the session because the session
	// state differs across those branches while the user-visible fact does
	// not: the reopen budget may be spent, or a live replacement may have been
	// found and refused the prompt anyway. All of them mean the same thing to
	// the person who just pressed enter — those words are gone — and that is
	// what the message needs to say.
	//
	// Retryable, and deliberately: the attachment was armed — either its session
	// confirmed, or it reused one that was live — so an ordinary reconnect lands
	// on the `--resume` path, which has its full fallback machinery behind it
	// for the case where the transcript turns out not to exist, and a fresh
	// attachment gets a fresh reopen budget. Marking it terminal would stop the
	// browser's ladder on the one failure a reconnect actually fixes.
	//
	// "Armed" and not "confirmed": reopenSID has two arming sites of different
	// strength and session/attach.go is explicit that collapsing them is wrong
	// ("Do not collapse these two into 'armed ⟹ the transcript exists'"). The
	// reuse site observes only liveEntry, so this code can reach the browser
	// from an attachment whose session never emitted a session id. The verdict
	// is the same either way, which is exactly why it is worth stating the
	// weaker premise it actually rests on.
	//
	// Not ErrCodeAgent, which would be the closest existing fit and is wrong
	// in the way that matters here: that code means the agent REPORTED
	// something about a turn it is still able to continue. On these branches
	// the agent said nothing at all — it died, or was never reachable — and
	// the connection is not usable until it is remade.
	ErrCodePromptUndelivered ErrorCode = "prompt_undelivered"
)

// ErrorPayload is the Payload of a KindError Envelope once it carries a Code.
// Message is unchanged from the pre-tether#63 behaviour (cc's error text, or a
// hand-written sentence) and is human-readable but NOT parsed by the browser;
// Code and Terminal are what the browser is meant to act on.
type ErrorPayload struct {
	Code     ErrorCode `json:"code"`
	Message  string    `json:"message"`
	Terminal bool      `json:"terminal"`
}

// terminalCodes classifies every ErrorCode declared in this package as
// terminal (true — refusal will not resolve by retrying the same request) or
// retryable (false — an ordinary reconnect might succeed). It is unexported
// deliberately: NewErrorEnvelope is the only way to reach it, so a KindError
// envelope's Terminal bit can never disagree with this table (see the package
// doc comment above for why that invariant is worth a single choke point).
//
// errors_test.go asserts this map has an entry for every ErrorCode const this
// package declares, by parsing the package's own source with go/ast — so a
// new code added without a corresponding line here fails the build instead of
// silently defaulting through Terminal() below.
var terminalCodes = map[ErrorCode]bool{
	ErrCodeUnknownWorkspace:    true,
	ErrCodeNoWorkspaceRegistry: true,
	ErrCodeUnknownProvider:     true,
	ErrCodeSessionOwned:        true,
	ErrCodeSpawnFailed:         false,
	ErrCodeConnectionClosed:    false,
	ErrCodeSessionUnconfirmed:  false,
	ErrCodeAgent:               false,
	ErrCodePromptUndelivered:   false,
}

// Terminal reports whether c is a permanent refusal the browser should stop
// retrying, as opposed to a transient failure an ordinary reconnect might
// clear. A code this package does not recognize answers false — see the
// package doc comment's "why an unclassified code defaults to retryable" for
// the reasoning; that default is deliberate, not a gap.
func (c ErrorCode) Terminal() bool {
	return terminalCodes[c]
}

// NewErrorEnvelope builds a KindError Envelope carrying an ErrorPayload. It is
// the only constructor this package exposes for one, precisely so that
// Terminal can never be set by hand at a call site — see the package doc
// comment for why that single choke point is the point.
//
// A convention, not an enforced invariant: Envelope's fields are exported, so
// a caller CAN hand-build a KindError, and one still does —
// internal/server/wt_shell.go's lock_held, on the shell channel, with its own
// extra fields. That one predates this file and is left alone deliberately
// (different route, different payload, no frontend consumer). It stays harmless
// because the browser's parseErrorPayload requires `message` and `terminal`
// and treats anything else as unclassified, i.e. retryable — the pre-tether#63
// behaviour.
func NewErrorEnvelope(code ErrorCode, msg string) Envelope {
	return Envelope{
		Kind: KindError,
		Payload: ErrorPayload{
			Code:     code,
			Message:  msg,
			Terminal: code.Terminal(),
		},
	}
}
