// Package doctor implements the `tether doctor` preflight checks (s5.5).
package doctor

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/server"
)

// Status is the outcome of a single check.
//
// Three values and not a bool, because "this is broken" and "I could not check
// this" are different claims and doctor was making the first one when it meant
// the second: the cert check read ~/.tether/cert.pem no matter which of the
// three cert paths a deployment used, so every healthy --cert-file host was
// told its cert was missing (tether#84). A check that is always red is a check
// people learn to skip, which costs the whole report its meaning — so a check
// that cannot reach its subject now says so instead of failing it.
type Status string

const (
	// StatusOK means the check ran and found what it was looking for.
	StatusOK Status = "ok"
	// StatusFail means the check ran and found a real problem.
	StatusFail Status = "fail"
	// StatusSkip means the check could not run, or has nothing to assert
	// about this deployment. Not a verdict on the subject either way.
	StatusSkip Status = "skip"
)

// CheckResult is the result of a single preflight check.
//
// The json tags drive decoding only; encoding goes through MarshalJSON, which
// derives the legacy "ok" field rather than storing it. One state, one field —
// an OK bool alongside Status is two things to keep in agreement, and the pair
// disagreeing is how a skip would be reported as a pass in one place and a
// failure in another.
type CheckResult struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// Failed reports whether this check found a genuine problem. A skipped check
// has not.
func (c CheckResult) Failed() bool { return c.Status == StatusFail }

// MarshalJSON emits status plus a derived ok for consumers written against the
// pre-tether#84 shape. ok is "did not fail", so a skipped check serialises as
// ok:true — matching Report.OK, which skips do not flip. A consumer that needs
// to tell a skip from a pass has to read status.
func (c CheckResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Status  Status `json:"status"`
		Message string `json:"message"`
		Detail  string `json:"detail,omitempty"`
	}{c.Name, !c.Failed(), c.Status, c.Message, c.Detail})
}

// Report is the aggregated result of all preflight checks.
type Report struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
}

// Run executes all mandatory preflight checks against the configuration the
// server would be started with. If verbose is true, extra diagnostic detail is
// populated on each CheckResult.
//
// cfg is the same *server.Config `tether server` is given, because several
// checks are only answerable relative to it — which certificate is served is
// decided entirely by --cert-file/--key-file/--acme-domain, and nothing on disk
// records the choice after the fact (~/.tether/config.json carries the [mcp]
// section and nothing else). Passing the struct rather than a doctor-local copy
// is what keeps a flag meaning the same thing in both commands.
//
// Only Port, CertFile, KeyFile and AcmeDomain are read today; the checks that
// still assume a default (the MCP port, the api-tokens path) are noted at their
// own definitions.
func Run(cfg *server.Config, verbose bool) Report {
	if cfg == nil {
		cfg = &server.Config{}
	}
	checks := []CheckResult{
		checkCCBinary(verbose),
		checkOpencodeBinary(verbose),
		checkDataDir(verbose),
		checkCertState(cfg, verbose),
		checkPortBindable(cfg.Port, verbose),
		checkCCSettingsHooks(verbose),
		checkMCPSettingsInject(verbose),
		checkMCPAPITokens(verbose),
		checkMCPLoopback(verbose),
	}

	allOK := true
	for _, c := range checks {
		if c.Failed() {
			allOK = false
		}
	}
	return Report{OK: allOK, Checks: checks}
}

// ok, fail and skip build a CheckResult in one of the three states.
//
// Three constructors rather than one with a Status argument so that the state a
// check returns is the first thing on the line, and so that adding a state
// later cannot silently default an existing call site to the wrong one.
func ok(name, message string) CheckResult {
	return CheckResult{Name: name, Status: StatusOK, Message: message}
}

func fail(name, message string) CheckResult {
	return CheckResult{Name: name, Status: StatusFail, Message: message}
}

func skip(name, message string) CheckResult {
	return CheckResult{Name: name, Status: StatusSkip, Message: message}
}

// checkOpencodeBinary verifies the opencode binary is on PATH (optional).
//
// Absent is StatusSkip, not StatusOK: it is a fact about an optional dependency
// rather than a healthy one, and a ✓ next to "opencode not found" invites the
// reader to stop trusting the marks.
func checkOpencodeBinary(verbose bool) CheckResult {
	path, err := exec.LookPath("opencode")
	if err != nil {
		return skip("opencode-binary", "opencode not found on PATH (optional; only needed for opencode sessions)")
	}
	r := ok("opencode-binary", "opencode found")
	if verbose {
		r.Detail = path
	}
	return r
}

// checkCCBinary verifies the cc binary the server would spawn exists and is
// executable.
//
// It asks server.ResolveClaudePath rather than looking on PATH itself, because
// the server does not look only on PATH: $TETHER_CC_PATH is consulted first,
// PATH second, and six installer directories back it up. Checking only PATH
// answered a narrower question than the spawn this check exists to predict, and
// failed hosts that run cc perfectly well — the same shape of bug as the cert
// check above, on the check the operator reads first.
//
// exec.LookPath finishes the job for both kinds of answer: given a path
// containing a separator it tests that path for existence and the execute bit
// (lookPath, Go 1.26.3 os/exec/lp_unix.go:53), and given a bare name it does
// the PATH search — which is what resolving to the literal "claude" fallback
// asks for, and what the spawn would do with it.
func checkCCBinary(verbose bool) CheckResult {
	resolved := server.ResolveClaudePath()
	path, err := exec.LookPath(resolved)
	if err != nil {
		return fail("cc-binary", fmt.Sprintf("claude binary not usable (resolved to %q): %v — set $TETHER_CC_PATH or install cc on PATH", resolved, err))
	}
	r := ok("cc-binary", "claude found")
	if verbose {
		r.Detail = path
	}
	return r
}

// checkDataDir verifies ~/.tether/ exists and is writable.
//
// Unlike the cert, this one directory is common to all three cert paths: Run
// creates it at Step 2 whatever the flags say, and the session store and the
// hook binary live under it unconditionally. (The api-token store usually does
// too, but Config.APITokensPath can move it — see checkMCPAPITokens.)
func checkDataDir(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		// A failure, not a skip, and this is the one check that must say so.
		// Several checks give up when $HOME is undefined — a systemd unit
		// without it is the usual way — and each of those is honestly reporting
		// that it could not look. But the server does not merely fail to be
		// inspected on such a host, it fails to start: Step 2's tetherDataDir
		// returns this same error out of Run. If every check skipped, `tether
		// doctor` would exit 0 for a daemon that cannot boot, which is worse
		// than the false alarm this wi set out to remove.
		return fail("data-dir", "cannot determine home dir (the server needs it at startup too): "+err.Error())
	}
	dir := filepath.Join(home, ".tether")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fail("data-dir", "~/.tether does not exist (run `tether server` once to create it)")
	}
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return fail("data-dir", "~/.tether not writable: "+err.Error())
	}
	_ = os.Remove(probe)
	r := ok("data-dir", "~/.tether exists and writable")
	if verbose {
		r.Detail = dir
	}
	return r
}

// checkCertState verifies that the cert this deployment serves exists and has
// > 24h remaining.
//
// Which cert that is comes from server.LocateCert, not from this package: the
// three cert paths keep their certificates in three places, and reading
// ~/.tether/cert.pem regardless — which is what this did — reported a healthy
// --cert-file host as broken, and read the wrong file entirely on an ACME one.
// The ACME case is worth spelling out, because "wrong file" there does not mean
// "no file": Run's Step 4 loads-or-mints the managed cert before Step 4b throws
// it away for certmagic's (lifecycle.go), and certRenewalFor starts no rotation
// loop for ACME — so an ACME host (one without --cert-file, which would send
// Step 4 down the operator branch and mint nothing) has a ~/.tether/cert.pem
// that is real, is not served, and is not maintained. Past its 14th day this check went red against a
// daemon serving a perfectly good Let's Encrypt cert, and told the operator to
// go and check ~/.tether writability.
//
// Missing is judged per source rather than uniformly, because the three
// deployments differ in who is supposed to produce the file:
//
//   - operator files: a fault, and a hard one. LoadOrGenCert propagates the
//     read failure and Run returns it (lifecycle.go Step 4), so the server does
//     not start at all.
//   - acme: nothing to say yet. certmagic obtains the cert during startup, so
//     an empty store before the first run is expected.
//   - managed: nothing to say either, but for a second reason as well. It is
//     the source picked when no cert flags were passed, which is also the state
//     of knowing nothing about the deployment — an operator running bare
//     `tether doctor` on a --cert-file host lands here. And even taken at face
//     value it is not a fault: loadOrRotateManaged mints a replacement whenever
//     the stored pair fails to load, so a deleted cert.pem repairs itself on the
//     next tick of a running daemon.
func checkCertState(cfg *server.Config, verbose bool) CheckResult {
	loc, err := server.LocateCert(cfg)
	if err != nil {
		return skip("cert-state", "cannot work out where this deployment's cert lives: "+err.Error())
	}
	hint := lonePEMFlagHint(cfg, loc.Source) + acmeStoreHint(loc.Source)

	// Checked before the served cert, and for a config that may not serve these
	// files at all. Step 4 loads the pair whenever both flags are set and
	// returns the error, so an unreadable --cert-file stops the daemon even
	// under --acme-domain — a startup-fatal fault that reporting only on the
	// ACME store would step straight past. This is also what covers the key on
	// the operator path: crypto/tls loads the pair or nothing, so a valid cert
	// beside a missing key is still a server that does not start, and "cert
	// valid" would be a false pass.
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		if err := server.CheckOperatorPEM(cfg.CertFile, cfg.KeyFile); err != nil {
			msg := "operator cert/key pair will not load: " + err.Error()
			if loc.Source != server.CertSourceOperatorFiles {
				msg += " — this deployment does not serve them, but the server loads them at startup anyway and refuses to start"
			}
			return fail("cert-state", msg)
		}
	}

	if loc.Path == "" {
		// Only CertSourceACME reports an empty path, and only when certmagic's
		// store holds no cert for this domain.
		return skip("cert-state", fmt.Sprintf("no ACME cert stored for %s yet — certmagic obtains one at startup", cfg.AcmeDomain))
	}

	data, err := os.ReadFile(loc.Path)
	if err != nil {
		if loc.Source == server.CertSourceManaged {
			return skip("cert-state", "no managed cert at "+loc.Path+
				" — `tether server` mints one on first start"+
				"; if this host serves --cert-file or --acme-domain, re-run doctor with the same flags"+hint)
		}
		// No "the server will not start" here. That is true of the operator
		// path, where Run propagates the load error, and it is not established
		// for ACME: LocateCert only names a stored cert it has just stat'd, so
		// arriving here at all means the file changed underneath us, and what
		// certmagic does next is its own business.
		return fail("cert-state", fmt.Sprintf("%s cert unreadable at %s: %v", loc.Source, loc.Path, err))
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fail("cert-state", fmt.Sprintf("%s: invalid PEM", loc.Path))
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fail("cert-state", fmt.Sprintf("%s: parse error: %v", loc.Path, err))
	}
	remaining := time.Until(cert.NotAfter)
	if remaining < 24*time.Hour {
		return fail("cert-state", fmt.Sprintf("%s cert expires in %v (< 24h); %s%s",
			loc.Source, remaining.Round(time.Minute), renewalRemedy(loc), hint))
	}
	r := ok("cert-state", fmt.Sprintf("%s cert valid, expires in %v%s", loc.Source, remaining.Round(time.Hour), hint))
	if verbose {
		r.Detail = fmt.Sprintf("path=%s notAfter=%s", loc.Path, cert.NotAfter.Format(time.RFC3339))
	}
	return r
}

// renewalRemedy says what, on this cert path, is supposed to have replaced a
// cert that is nearly out of time — and therefore what an operator seeing this
// should look at. One sentence per path, because they are three different
// stories about who renews (see certRenewalFor).
func renewalRemedy(loc server.CertLocation) string {
	switch loc.Source {
	case server.CertSourceOperatorFiles:
		return "nothing in tether can renew a cert it did not issue — renew " + loc.Path +
			" yourself; a running daemon picks the new file up within a minute"
	case server.CertSourceACME:
		return "certmagic renews this one in the background — check the server log for renewal failures"
	default:
		// Unchanged since tether#72 made rotation actually reach live traffic:
		// the managed loop re-mints inside 24h of expiry, once an hour.
		return "a running server rotates within the hour — if it has not, check ~/.tether writability"
	}
}

// acmeStoreHint points at an ACME store found while reporting on the managed
// cert.
//
// This is the one thing doctor can say without being told, and it closes the
// case that the flags alone do not: on a host serving --acme-domain, Run's Step
// 4 leaves a managed cert behind that nothing renews, so a bare `tether doctor`
// there eventually reports an expired cert — accurately, about a file the
// daemon does not serve, with a remedy pointing at ~/.tether. That is the false
// alarm this wi is about, wearing a different hat.
//
// It is a note appended to the verdict rather than a verdict of its own, and
// deliberately so. A store is evidence that this host has obtained an ACME cert
// at some point, not proof of what it serves now — an operator who moved from
// --acme-domain back to the managed cert leaves the same directory behind. So
// the managed cert keeps being judged on its own terms (a real managed
// deployment with an expired cert still goes red) and the reader is told, in
// one line, which flag would settle it. Downgrading the verdict to a skip
// instead would leave that operator with no way to get an answer at all: there
// is no --managed flag to assert the other reading with.
func acmeStoreHint(src server.CertSource) string {
	if src != server.CertSourceManaged {
		return ""
	}
	domains, err := server.ACMEStoredDomains()
	if err != nil || len(domains) == 0 {
		return ""
	}
	return fmt.Sprintf(" (this host also has an ACME cert store for %s — if that is what it serves, re-run with --acme-domain)",
		strings.Join(domains, ", "))
}

// lonePEMFlagHint warns that a --cert-file passed without --key-file (or the
// reverse) is being ignored.
//
// The condition is not spelled out here on purpose: reaching CertSourceManaged
// with either flag set can only mean the pair is incomplete, since a complete
// one selects CertSourceOperatorFiles and --acme-domain selects
// CertSourceACME. Asking server.CertSourceFor rather than re-testing the flags
// is what keeps this in step with the loader — which quietly falls back to the
// managed cert in exactly this case, and is the reason the operator needs
// telling.
func lonePEMFlagHint(cfg *server.Config, src server.CertSource) string {
	if src != server.CertSourceManaged || (cfg.CertFile == "" && cfg.KeyFile == "") {
		return ""
	}
	return " (note: --cert-file and --key-file only take effect together; the one you passed is being ignored)"
}

// checkPortBindable verifies that the given port can be bound on TCP.
func checkPortBindable(port int, verbose bool) CheckResult {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fail("port-bindable", fmt.Sprintf("port %d not bindable: %v", port, err))
	}
	_ = l.Close()
	r := ok("port-bindable", fmt.Sprintf("port %d available", port))
	if verbose {
		r.Detail = addr
	}
	return r
}

// checkMCPSettingsInject verifies that ~/.claude/settings.json contains the
// tether-managed mcpServers.tether entry injected by `tether server`.
// It shares the residual assumption noted on checkCCSettingsHooks: a server
// started with --skip-mcp-inject never writes this entry, and doctor is not
// told about that flag.
func checkMCPSettingsInject(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return skip("mcp-settings-inject", "cannot determine home dir: "+err.Error())
	}
	path := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fail("mcp-settings-inject", "~/.claude/settings.json not found — run `tether server` to inject")
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fail("mcp-settings-inject", "~/.claude/settings.json: parse error: "+err.Error())
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if entry, found := mcpServers["tether"].(map[string]any); found {
		if managed, _ := entry[agent.TetherManagedKey].(bool); managed {
			mcpURL, _ := entry["url"].(string)
			r := ok("mcp-settings-inject", "tether MCP server injected in settings.json")
			if verbose {
				r.Detail = "url=" + mcpURL
			}
			return r
		}
	}
	return fail("mcp-settings-inject", "tether MCP server not in settings.json — run `tether server` to inject")
}

// checkMCPAPITokens reports how many external API tokens are configured in
// ~/.tether/api-tokens.json. A missing or empty file is not an error — it
// just means no external MCP clients (Cursor, Goose) have been authorised yet.
//
// Residual single-path assumption, deliberately left: server.Config.APITokensPath
// can move this file, and doctor exposes no flag for it, so a deployment that
// sets it would be told "no external API tokens yet" while its store is full.
// That misreports a count rather than a state — it can never turn a healthy host
// red — so it waits for the flag rather than adding one nothing passes.
func checkMCPAPITokens(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return skip("mcp-api-tokens", "cannot determine home dir: "+err.Error())
	}
	path := filepath.Join(home, ".tether", "api-tokens.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ok("mcp-api-tokens", "no external API tokens yet (create via POST /api/v1/mcp/tokens or OAuth flow)")
	}
	if err != nil {
		return skip("mcp-api-tokens", "api-tokens.json unreadable: "+err.Error())
	}
	var file struct {
		Tokens []struct{ ID string } `json:"tokens"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fail("mcp-api-tokens", "api-tokens.json: parse error (file may be corrupt): "+err.Error())
	}
	n := len(file.Tokens)
	if n == 0 {
		return ok("mcp-api-tokens", "api-tokens.json exists, no external tokens yet")
	}
	r := ok("mcp-api-tokens", fmt.Sprintf("%d external API token(s) configured", n))
	if verbose {
		r.Detail = path
	}
	return r
}

// checkMCPLoopback attempts a TCP connection to the tether MCP loopback
// endpoint. The port is read from mcpServers.tether.url in
// ~/.claude/settings.json; falls back to :8899 if absent.
//
// An unreachable endpoint is StatusSkip: doctor is a preflight command and is
// routinely run with no server started, so a refused connection says nothing
// about the health of the thing being connected to. It is also why this check
// does not consult server.Config.MCPPort — the injected URL is a record of what
// the last-started server actually used, which is a better source than a flag
// nobody passed to doctor.
func checkMCPLoopback(verbose bool) CheckResult {
	port := 8899
	home, _ := os.UserHomeDir()
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
			var settings map[string]any
			if json.Unmarshal(data, &settings) == nil {
				if mcpServers, _ := settings["mcpServers"].(map[string]any); mcpServers != nil {
					for _, v := range mcpServers {
						if m, ok := v.(map[string]any); ok {
							if managed, _ := m[agent.TetherManagedKey].(bool); managed {
								if rawURL, _ := m["url"].(string); rawURL != "" {
									if u, err := url.Parse(rawURL); err == nil {
										if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
											port = p
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return skip("mcp-loopback", fmt.Sprintf("MCP loopback not reachable at %s (is `tether server` running?)", addr))
	}
	_ = conn.Close()
	r := ok("mcp-loopback", fmt.Sprintf("MCP loopback reachable at %s", addr))
	if verbose {
		r.Detail = addr
	}
	return r
}

// checkCCSettingsHooks verifies that ~/.config/claude/settings.json contains
// the tether-managed PreToolUse hook entry.
//
// Residual single-path assumption, deliberately left: a server started with
// TETHER_NO_PERMISSION_HOOK=1 never injects this hook, and a daemon's
// environment is not something a hand-run doctor shares — so honouring the
// variable here would only be right when doctor happens to be launched the same
// way the daemon was, and wrong (a green tick over an uninjected hook) the rest
// of the time. Reporting the absence stays the safer half of that trade, so
// this keeps failing until doctor is given a way to be told.
func checkCCSettingsHooks(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return skip("cc-settings-hooks", "cannot determine home dir: "+err.Error())
	}
	// Claude Code reads ~/.claude/settings.json (user-level), not ~/.config/claude/settings.json.
	path := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fail("cc-settings-hooks", "~/.claude/settings.json not found — run `tether server` to inject hook")
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fail("cc-settings-hooks", "settings.json: parse error: "+err.Error())
	}
	hooks, _ := settings["hooks"].(map[string]any)
	list, _ := hooks["PreToolUse"].([]any)
	for _, h := range list {
		if hm, isObj := h.(map[string]any); isObj {
			if managed, _ := hm[agent.TetherManagedKey].(bool); managed {
				r := ok("cc-settings-hooks", "tether PreToolUse hook is active")
				if verbose {
					r.Detail = path
				}
				return r
			}
		}
	}
	return fail("cc-settings-hooks", "tether hook not found in settings.json — run `tether server` to inject")
}
