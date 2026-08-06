package server

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// handleWTShell handles /wt/shell WebTransport upgrades (s6 / D-05a §2 fact 4).
// The connection carries raw PTY bytes — no JSON framing, no envelope wrapping.
// xterm.js on the browser side consumes the raw stream directly.
func handleWTShell(reg *session.Registry, wts *webtransport.Server, authState *auth.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authState.ClientIDFromTicket(r) == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		sid := r.URL.Query().Get("sid")

		wtSess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ctx := wtSess.Context()

		// Accept the bidi stream the browser opens.
		stream, err := wtSess.AcceptStream(ctx)
		if err != nil {
			_ = wtSess.CloseWithError(0, "")
			return
		}

		clientID := newShellID()
		lock := reg.GetLock(sid)
		acquired, preempted := lock.Acquire(clientID)
		if !acquired {
			holder := lock.Holder()
			_, _ = stream.Write([]byte("\r\n[tether] session locked by " + holder + "\r\n"))
			// Broadcast lock-held event so the browser can offer force-takeover.
			reg.BroadcastAll(wire.Envelope{
				Kind: wire.KindError,
				Payload: map[string]any{
					"code":      "lock_held",
					"holder":    holder,
					"sessionId": sid,
				},
			})
			_ = stream.Close()
			_ = wtSess.CloseWithError(0, "lock held")
			return
		}
		defer lock.Release(clientID)

		// Spawn claude under PTY. cc internally coordinates jsonl with any
		// concurrent chat subprocess (D-05a §2 fact 3).
		// WorkdirForSession, not reg.Workdir: since tether#52 the chat session this
		// shell is about to `--resume` may live in any registered workspace, and a
		// shell in a different directory finds no conversation there (see
		// buildPTYCommand). The sid is enough to look it up — the shell needs no
		// workspace parameter of its own, and giving it one would let the two
		// disagree.
		cmd := buildPTYCommand(ctx, resolveClaudePath(), sid, reg.WorkdirForSession(sid))
		cmd.Env = buildPTYEnv(reg.PermEndpoint)

		// Start the PTY at the size the browser already knows it will render
		// at, rather than at the kernel default. Waiting for the first resize
		// frame instead would paint one screenful of TUI at the wrong width and
		// then reflow it, and would leave the size wrong for the whole session
		// whenever /wt/control never connects.
		ptmx, err := startPTY(cmd, parseWinsize(r.URL.Query()))
		if err != nil {
			_, _ = stream.Write([]byte("\r\n[tether] failed to start shell: " + err.Error() + "\r\n"))
			_ = stream.Close()
			_ = wtSess.CloseWithError(1, "pty start failed")
			return
		}

		defer attachShellResize(reg, sid, ptmx)()

		done := make(chan struct{})
		var closeOnce sync.Once
		closePTY := func() { closeOnce.Do(func() { ptmx.Close() }) }

		// PTY → WT: forward raw output bytes.
		go func() {
			defer close(done)
			_, _ = io.Copy(stream, ptmx)
		}()

		// WT → PTY: forward keyboard input.
		go func() {
			_, _ = io.Copy(ptmx, stream)
			closePTY()
		}()

		select {
		case <-done:
			// PTY process exited normally.
		case <-preempted:
			// Force-taken by another client.
			_, _ = stream.Write([]byte("\r\n[tether] session taken over\r\n"))
			closePTY()
		case <-ctx.Done():
			// WebTransport session disconnected.
			closePTY()
		}

		_ = stream.Close()
		_ = cmd.Wait()
	}
}

// attachShellResize routes later /wt/control resize frames to this PTY and
// returns the detach func, so the caller can write `defer attach(...)()`.
//
// Size changes cannot ride the /wt/shell stream — it is raw PTY bytes by
// contract (D-05a §2 fact 4) — so they arrive on /wt/control and are routed
// back by sid, the same key the shell lock uses, which is what guarantees at
// most one live shell owns it. tether#68.
//
// Extracted from handleWTShell (rather than inlined there) so the whole path
// from a control frame to the kernel's winsize is reachable from a test: the
// handler itself needs a live WebTransport session, and an untested seam here
// is exactly the kind that keeps compiling while doing nothing.
func attachShellResize(reg *session.Registry, sid string, ptmx *os.File) func() {
	reg.RegisterShellResize(sid, func(cols, rows uint16) error {
		return pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
	})
	return func() { reg.UnregisterShellResize(sid) }
}

// parseWinsize reads the terminal size the browser reported on the /wt/shell
// query string. Returns nil when either dimension is absent, unparseable, or
// zero — the caller then starts the PTY at the kernel default, which is what
// every shell did before tether#68. A bad size is not worth failing a shell
// over; it degrades to the old behaviour and the first resize frame corrects it.
func parseWinsize(q url.Values) *pty.Winsize {
	cols, errCols := strconv.ParseUint(q.Get("cols"), 10, 16)
	rows, errRows := strconv.ParseUint(q.Get("rows"), 10, 16)
	if errCols != nil || errRows != nil || cols == 0 || rows == 0 {
		return nil
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}
}

// startPTY starts cmd on a PTY, sized when the client told us a size and at
// the kernel default when it did not.
func startPTY(cmd *exec.Cmd, ws *pty.Winsize) (*os.File, error) {
	if ws == nil {
		return pty.Start(cmd)
	}
	return pty.StartWithSize(cmd, ws)
}

// handleLockForce handles POST /api/v1/session/{sid}/lock/force (D-15 force-takeover).
func handleLockForce(reg *session.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Path: /api/v1/session/{sid}/lock/force → parts = [{sid}, "lock", "force"]
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/session/"), "/")
		if len(parts) != 3 || parts[1] != "lock" || parts[2] != "force" {
			http.NotFound(w, r)
			return
		}
		sid := parts[0]

		var body struct {
			ClientID string `json:"clientId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		clientID := body.ClientID
		if clientID == "" {
			clientID = newShellID()
		}

		lock := reg.GetLock(sid)
		lock.ForceAcquire(clientID)

		reg.BroadcastAll(wire.Envelope{
			Kind:      wire.KindMessage,
			SessionID: wire.SessionID(sid),
			Payload: map[string]any{
				"type":     "lock_taken",
				"clientId": clientID,
			},
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"clientId": clientID})
	}
}

// newShellID returns a random hex ID for shell session client tracking.
// Defined here rather than in internal/permission to avoid coupling
// unrelated identity domains to the permission package.
func newShellID() string {
	b := make([]byte, 8)
	if _, err := cryptorand.Read(b); err != nil {
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// buildPTYCommand builds the `claude` invocation for the PTY shell pane.
//
// The working directory is load-bearing, not cosmetic: the shell pane is handed
// the *chat* session's sid (web/src/panes/shell/index.tsx reads it from the
// store) and resumes it with `--resume <sid>`, but cc stores conversations under
// ~/.claude/projects/<encoded-cwd>/ — so a shell whose cwd differs from the cwd
// the chat session was created in gets "No conversation found" and drops the
// user into a fresh conversation. Both spawn paths therefore resolve their cwd
// through the same agent.ResolveWorkdir (tether#51); "" means "inherit the
// daemon's cwd", which is the pre-tether#51 behaviour.
//
// Since tether#52 workdir is the directory of THAT SESSION's workspace — the
// caller gets it from session.Registry.WorkdirForSession(sid), which falls back
// to the daemon-global root for a sid it knows nothing about. It is no longer
// simply Registry.Workdir, because chat is no longer always there.
//
// Extracted as a plain function so this contract is unit-testable without
// standing up a WebTransport session and a PTY.
func buildPTYCommand(ctx context.Context, ccPath, sid, workdir string) *exec.Cmd {
	var args []string
	if sid != "" {
		args = append(args, "--resume", sid)
	}
	cmd := exec.CommandContext(ctx, ccPath, args...)
	cmd.Dir = agent.ResolveWorkdir(workdir)
	return cmd
}

// buildPTYEnv constructs the env for the PTY shell subprocess.
// IS_SANDBOX=1 injected for root (D-05a §2 fact 5). TERM set for full TUI.
// permEndpoint is injected when non-empty so the PreToolUse hook can reach the daemon.
func buildPTYEnv(permEndpoint string) []string {
	env := os.Environ()
	env = append(env, "TERM=xterm-256color")
	if os.Geteuid() == 0 {
		env = append(env, "IS_SANDBOX=1")
	}
	if permEndpoint != "" {
		env = append(env, "TETHER_DAEMON_PERM_ENDPOINT="+permEndpoint)
	}
	return env
}
