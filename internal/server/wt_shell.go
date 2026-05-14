package server

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/quic-go/webtransport-go"

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
		ccPath := resolveClaudePath()
		var args []string
		if sid != "" {
			args = append(args, "--resume", sid)
		}
		cmd := exec.CommandContext(ctx, ccPath, args...)
		cmd.Env = buildPTYEnv(reg.PermEndpoint)

		ptmx, err := pty.Start(cmd)
		if err != nil {
			_, _ = stream.Write([]byte("\r\n[tether] failed to start shell: " + err.Error() + "\r\n"))
			_ = stream.Close()
			_ = wtSess.CloseWithError(1, "pty start failed")
			return
		}

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
