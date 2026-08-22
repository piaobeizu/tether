package cchook

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Environment variables the compiled hook reads, declared in the same file as
// the hook source that reads them so the two cannot drift out of sight of each
// other. TestHookSource_ReadsDeclaredEnvVars pins that, since the hook is a
// string constant and the compiler cannot.
const (
	// EnvEndpoint carries the daemon's loopback permission callback URL.
	EnvEndpoint = "TETHER_DAEMON_PERM_ENDPOINT"
	// EnvManaged marks a cc subprocess as one the tether daemon spawned. It is
	// what lets the hook tell "not our cc, leave it alone" apart from "our cc,
	// but the endpoint is missing" — two states that were indistinguishable, and
	// so both resolved as allow (tether#117 A4b).
	EnvManaged = "TETHER_DAEMON_MANAGED"
	// ManagedValue is the only value of EnvManaged the hook accepts as the mark.
	ManagedValue = "1"
)

// Gate describes how a cc subprocess the daemon spawns is wired to the
// PreToolUse permission gate. Build it ONCE at startup — server.Run does, into
// Config.PermGate — and hand that one value to every cc spawn path, so no path
// can invent its own answer.
//
// Both cc spawn paths consume it since tether#149: session.Registry.spawnEntry
// for chat and server.buildPTYEnv for the PTY shell pane, each appending Env()
// to the child's environment and nothing else. Adding a THIRD spawn path means
// calling Env() there too — see TestBothCCSpawnPathsCarryTheGate, which is the
// only test that can observe more than one path at once.
//
// What the mark does NOT do, so it is not mis-read as the A4b fix: the deny
// branch below (marked, no endpoint) is unreachable from server.setupPermGate
// today, because Managed=true there always carries an endpoint — it is a
// formatted string built from the daemon's own listen address, not something
// that can go missing. What actually closed A4b in production is that the
// endpoint no longer depends on the settings.json patch succeeding. The mark
// exists so that a spawn path which forgets to wire the gate becomes VISIBLE
// (the child is unmarked, the hook takes the "not our cc" branch, and the test
// above goes red) instead of silently allowing every tool call.
//
// That the value is shared rather than recomputed is not hypothetical tidiness.
// The endpoint reaches the hook through the environment, and TWO places build
// that environment. A mark added to one and forgotten in the other fails open
// in exactly the branch it was added to close, and no test inside either
// package's own scope can see that.
type Gate struct {
	// Managed is true when this daemon owns the cc's permission gate. False
	// leaves the child completely unmarked, which is what keeps the owner's own
	// standalone/IDE cc runs working even with a hook entry in settings.json.
	Managed bool
	// Endpoint is the daemon's permission callback URL. Empty with Managed true
	// is a real, deliberate state: the child is marked but has nowhere to ask,
	// and the hook denies rather than allows.
	Endpoint string
}

// Env returns the environment entries a daemon-spawned cc subprocess must
// carry. Append the result to the child's environment; nil means "add nothing".
func (g Gate) Env() []string {
	if !g.Managed {
		return nil
	}
	env := []string{EnvManaged + "=" + ManagedValue}
	if g.Endpoint != "" {
		env = append(env, EnvEndpoint+"="+g.Endpoint)
	}
	return env
}

// hookSource is the permission hook source code embedded verbatim.
// EnsureHookBinary compiles it on startup if the binary is missing or stale.
const hookSource = `package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	body, _ := io.ReadAll(os.Stdin)
	endpoint := os.Getenv("TETHER_DAEMON_PERM_ENDPOINT")
	managed := os.Getenv("TETHER_DAEMON_MANAGED") == "1"
	if endpoint == "" {
		if managed {
			// Our cc, but no endpoint: the daemon marked this child and then
			// could not tell it where to ask. Deny — exit 2 is the ONLY code
			// cc blocks on; every other non-zero code it treats as
			// non-blocking, i.e. the tool runs anyway.
			fmt.Fprintln(os.Stderr,
				"[hook] tether-managed cc with no TETHER_DAEMON_PERM_ENDPOINT; denying")
			os.Exit(2)
		}
		// Unmarked AND no endpoint: this is NOT a tether-spawned cc (it's the
		// user's IDE / standalone cc). Exit 0 so we don't break unrelated cc
		// invocations. The hook only enforces tether's permission UI for cc
		// subprocesses launched by the tether daemon — and the mark is what
		// keeps that fail-open scoped to them, instead of covering a
		// tether-spawned cc whose endpoint went missing.
		os.Exit(0)
	}
	// InsecureSkipVerify is safe: endpoint is always loopback (127.0.0.1).
	client := &http.Client{
		Timeout: 65 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hook] daemon unreachable: %v\n", err)
		os.Exit(2)
	}
	defer resp.Body.Close()
	var dec struct {
		Allow   bool   ` + "`json:\"allow\"`" + `
		Message string ` + "`json:\"message,omitempty\"`" + `
	}
	if err := json.NewDecoder(resp.Body).Decode(&dec); err != nil {
		fmt.Fprintf(os.Stderr, "[hook] decode response: %v\n", err)
		os.Exit(2)
	}
	if dec.Allow {
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "[hook] denied: %s\n", dec.Message)
	os.Exit(2)
}
`

// EnsureHookBinary compiles the hook binary to binPath if it is absent, not
// runnable, or the source hash has changed. Mode 0755 on success (D-05b §4.2).
func EnsureHookBinary(binPath string) error {
	srcHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hookSource)))

	// The hash file is a claim ABOUT the binary, not the binary. Checking it
	// alone reported "up to date" for a directory that held only
	// <binPath>.hash — after an `rm`, a wiped tmpfs, a half-finished upgrade.
	// Startup then succeeded, InjectPermHook wrote the missing path into
	// ~/.claude/settings.json as the PreToolUse command, and every tool call
	// exited 127. cc blocks a tool ONLY on exit code 2 and treats every other
	// non-zero code as non-blocking, so every tool ran with no permission
	// prompt at all, the sole trace being cc's own stderr (tether#117 A4a).
	//
	// So the fast path has to establish that something RUNNABLE is there:
	// present, a regular file (os.Stat succeeds on a directory too), and
	// carrying an exec bit (mode 0644 exits 126, non-blocking again).
	if runnable(binPath) {
		if existing, err := os.ReadFile(binPath + ".hash"); err == nil {
			if string(existing) == srcHash {
				return nil // up-to-date
			}
		}
	}

	tmpDir, err := os.MkdirTemp("", "tether-hook-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	srcPath := filepath.Join(tmpDir, "hook.go")
	if err := os.WriteFile(srcPath, []byte(hookSource), 0o600); err != nil {
		return err
	}

	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compile hook: %w", err)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return err
	}
	// `go build -o <path>` exits 0 and writes the binary INSIDE <path> when
	// <path> is a directory (verified: exit 0, output at <path>/<pkg>). So a
	// zero exit code is not evidence that binPath is now a hook — check the
	// payload, not the marker. Reached with a directory sitting where the hook
	// belongs; returning nil there is what let a fail-open gate look healthy.
	// The .hash is deliberately not written, so this is not remembered as
	// "up to date" on the next start.
	if !runnable(binPath) {
		return fmt.Errorf("compile hook: %s is not a runnable file after build", binPath)
	}
	return os.WriteFile(binPath+".hash", []byte(srcHash), 0o600)
}

// runnable reports whether path is something cc could actually execute: a
// regular file, carrying an exec bit where exec bits mean anything. Deliberately
// NOT exec.LookPath — that consults PATH and, on a noexec mount, reports a
// perfectly good 0755 file as unusable. The three states this rules out
// (absent, a directory, no exec bit) all end the same way inside cc: a non-zero
// exit code that is not 2, which cc treats as non-blocking, which means the tool
// runs ungated.
//
// The exec-bit clause is skipped on Windows because os.Stat there returns 0444
// or 0666 for EVERY regular file and never an exec bit (os/types_windows.go
// fileStat.mode — only directories get 0111). Applied unconditionally it would
// therefore be false for a perfectly good hook, and since tether releases for
// windows/amd64 the effect would be a `go build` on every single daemon start —
// invisible to CI, which cross-compiles and vets those platforms but runs the
// tests only on linux.
func runnable(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}
