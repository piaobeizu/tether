// Package doctor implements the `tether doctor` preflight checks (s5.5).
package doctor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
// Port, CertFile, KeyFile, AcmeDomain, MCPPort and SkipMCPInject are read
// today. Most of the rest of Config describes how the server runs rather than
// what a preflight check could look at; the one field a check does want and
// still ignores is APITokensPath, deliberately — see checkMCPAPITokens.
//
// The whole struct now reaches checkPortBindable rather than Port alone, and the
// cert fields are the reason: telling "your own daemon holds this port" from
// "somebody else does" means comparing what is served against the certificate
// these flags select, exactly as checkCertState does (identifyPortHolder).
func Run(cfg *server.Config, verbose bool) Report {
	if cfg == nil {
		cfg = &server.Config{}
	}
	checks := []CheckResult{
		checkCCBinary(verbose),
		checkOpencodeBinary(verbose),
		checkDataDir(verbose),
		checkCertState(cfg, verbose),
		checkPortBindable(cfg, verbose),
		// Ordered as startup is: Step 3 compiles the hook binary, then injects the
		// entry that names it, so a reader scanning down the report meets the
		// cause above the symptom.
		checkGoToolchain(verbose),
		checkCCSettingsHooks(verbose),
		checkMCPSettingsInject(cfg, verbose),
		checkMCPAPITokens(verbose),
		checkMCPLoopback(cfg, verbose),
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

// checkPortBindable verifies that the sockets `tether server` needs on this
// port are free — TCP *and* UDP, on every interface.
//
// One address was probed before tether#123, and it was neither of the two the
// server uses: net.Listen("tcp", "127.0.0.1:<port>"). Config.addr() returns
// ":<port>" (lifecycle.go), and that one string reaches two listeners —
// srv.tcp.ListenAndServeTLS binds it on TCP for every interface, and
// srv.wts.ListenAndServe hands it to the HTTP/3 server, which binds it on UDP.
// So the old probe returned a tick for two states in which startup dies on
// errCh, both measured:
//
//   - UDP held by another process. doctor said "port 8898 available"; the server
//     died with "UDP/WT: bind: address already in use". This is the one that
//     arrives without anybody constructing it, because QUIC/WebTransport is this
//     daemon's primary transport and a previous tether's UDP socket can outlive
//     its TCP one.
//   - TCP held on any address other than 127.0.0.1 — 10.146.0.11:18899 in the
//     measurement, and 127.0.0.2 behaves identically. A wildcard bind collides
//     with a bind on one address; a loopback-only probe does not see it.
//
// What happens when a socket is taken is also where this check stopped being
// permanently red against a live daemon. `tether doctor` on a host running its
// own server could never exit 0: this check failed, and Report.OK is the exit
// code (cmd/tether/main.go). Inspecting a running deployment is a normal thing
// to do — it is what checkMCPLoopback was deliberately written to allow — so a
// port held by *this deployment's own daemon* is a skip: nothing is broken and
// there is no bind left to predict.
//
// "Its own daemon" is established and never assumed (identifyPortHolder). With
// no such evidence the verdict stays a failure, because what the check did
// establish — nothing can bind this port now, so `tether server` will not start
// here — is true whoever holds it. Softening that to a skip would be the wrong
// half of the trade for a check whose whole job is to predict a bind.
func checkPortBindable(cfg *server.Config, verbose bool) CheckResult {
	port := cfg.Port
	addr := fmt.Sprintf(":%d", port)

	var taken []string
	if l, err := net.Listen("tcp", addr); err != nil {
		taken = append(taken, "TCP (HTTPS/h2): "+err.Error())
	} else {
		_ = l.Close()
	}
	// net.ListenPacket and not a second net.Listen: the HTTP/3 server opens a UDP
	// socket, and a TCP probe of the same port number answers a different
	// question. The two protocols are allocated independently, which is exactly
	// why one of them could be busy while the old check reported the port free.
	if c, err := net.ListenPacket("udp", addr); err != nil {
		taken = append(taken, "UDP (QUIC/HTTP3/WebTransport, this daemon's primary transport): "+err.Error())
	} else {
		_ = c.Close()
	}

	if len(taken) == 0 {
		r := ok("port-bindable", fmt.Sprintf("port %d bindable on TCP and UDP", port))
		if verbose {
			r.Detail = "probed " + addr + " on both protocols, the pair Config.addr() feeds"
		}
		return r
	}

	holder := identifyPortHolder(cfg, port)
	if holder.thisDeployment {
		r := skip("port-bindable", fmt.Sprintf(
			"port %d is held by this deployment's own daemon, so it is already running and there is no bind to preflight — %s (%s)",
			port, strings.Join(taken, "; "), holder.evidence))
		if verbose {
			r.Detail = holder.detail
		}
		return r
	}
	r := fail("port-bindable", fmt.Sprintf(
		"port %d not bindable, so `tether server` will not start here — %s. %s",
		port, strings.Join(taken, "; "), holder.evidence))
	if verbose {
		r.Detail = holder.detail
	}
	return r
}

// portHolder is what doctor could establish about whatever holds the port.
//
// evidence is a clause, and it is set on both paths: on the failure path the
// difference between "somebody else's server is on your port" and "doctor could
// not tell, and here is the flag that would let it" is the whole value of the
// line the operator reads.
type portHolder struct {
	thisDeployment bool
	evidence       string
	detail         string
}

// certFlagsHint is what a verdict that could not compare certificates owes the
// reader. The flags are how doctor is told which deployment it is looking at —
// the rule tether#84 settled for checkCertState, reaching one indirection
// further along, since the same three paths decide what a running daemon
// presents on the port.
const certFlagsHint = " Re-run with the same --cert-file/--key-file or --acme-domain the server has, so doctor can compare what is served against the certificate this deployment serves."

// identifyPortHolder asks whether the port is held by a daemon of this
// deployment, and answers only on evidence.
//
// The evidence is the certificate. A tether daemon serves TLS on this port, and
// which certificate it serves is decided entirely by the cert flags — the
// question server.LocateCert already answers for checkCertState — so a served
// leaf that matches that file byte for byte is a process holding this
// deployment's private key, which no unrelated service on the host has. It is
// the same shape of proof as checkMCPLoopback's bearer token: compare what
// answers against the secret this deployment records as its own, rather than
// treating "something answered" as an identity.
//
// InsecureSkipVerify is not a relaxation here, it is the mechanism. This is not
// a client deciding whether to trust a server; it is a comparison against a
// named file, and the managed cert is self-signed, so verification would reject
// the one certificate that settles the question.
//
// Only the TCP half is probed, and that covers a UDP half held by the same
// daemon: a running server holds both or is not running at all, since Run sends
// the HTTP/3 listener's bind error to errCh and returns. So a TCP listener
// presenting this deployment's cert means the process that bound UDP at startup
// is still up. The inference does not run the other way — a stale UDP socket
// with nothing on TCP identifies nobody, which is precisely the state this check
// exists to go red on.
func identifyPortHolder(cfg *server.Config, port int) portHolder {
	probeAddr := fmt.Sprintf("127.0.0.1:%d", port)
	served, err := servedLeafDER(cfg, probeAddr)
	if err != nil {
		return portHolder{
			evidence: fmt.Sprintf("Nothing completed a TLS handshake on %s (%v), so whatever holds the port is not a running daemon of this deployment.", probeAddr, err),
			detail:   "tls probe of " + probeAddr + " failed: " + err.Error(),
		}
	}
	loc, err := server.LocateCert(cfg)
	if err != nil || loc.Path == "" {
		return portHolder{
			evidence: fmt.Sprintf("Something is serving TLS on %s, but doctor could not work out which certificate this deployment serves, so it cannot tell whether that is your own daemon.%s", probeAddr, certFlagsHint),
			detail:   fmt.Sprintf("LocateCert: path=%q err=%v", loc.Path, err),
		}
	}
	// Reached for a managed deployment with no cert.pem as well as for a genuinely
	// corrupt file, because LocateCert names the managed path without reading it.
	// That is the state a --cert-file host run through a bare `tether doctor` is
	// in — Step 4 mints no managed cert on the operator path — which is why this
	// branch carries the flags hint too rather than only the one above.
	want, err := leafCertDER(loc.Path)
	if err != nil {
		return portHolder{
			evidence: fmt.Sprintf("Something is serving TLS on %s, but this deployment's %s cert at %s could not be read to compare against it (%v), so the holder could not be identified.%s", probeAddr, loc.Source, loc.Path, err, certFlagsHint),
			detail:   fmt.Sprintf("reference cert %s unusable: %v", loc.Path, err),
		}
	}
	if !bytes.Equal(want, served) {
		return portHolder{
			evidence: fmt.Sprintf("The certificate served on %s is not this deployment's %s cert at %s, so another server has the port.", probeAddr, loc.Source, loc.Path),
			detail:   fmt.Sprintf("served leaf does not match %s (%s)", loc.Path, loc.Source),
		}
	}
	return portHolder{
		thisDeployment: true,
		evidence:       fmt.Sprintf("it serves this deployment's %s certificate from %s", loc.Source, loc.Path),
		detail:         fmt.Sprintf("tls leaf on %s matches %s (%s)", probeAddr, loc.Path, loc.Source),
	}
}

// servedLeafDER returns the DER of the leaf certificate offered by whatever is
// listening on addr.
//
// ServerName carries --acme-domain when there is one, because certmagic picks
// the certificate by SNI and would otherwise have nothing to offer; the managed
// and operator paths serve one certificate regardless. ALPN mirrors the TCP
// listener's own list (server.go) so a peer that gates on it still answers.
func servedLeafDER(cfg *server.Config, addr string) ([]byte, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
		&tls.Config{
			// Deliberate; see identifyPortHolder. The peer is compared against a
			// file, not trusted, and the certificate that proves the answer is
			// self-signed.
			InsecureSkipVerify: true,
			ServerName:         cfg.AcmeDomain,
			NextProtos:         []string{"h2", "http/1.1"},
		})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("the handshake carried no certificate")
	}
	return certs[0].Raw, nil
}

// leafCertDER returns the DER of the first certificate in a PEM file.
//
// First and not "the one that looks like a leaf": that is what crypto/tls serves
// as the leaf from both layouts this has to read — the managed store's single
// certificate, and an operator's fullchain, where the chain is ordered leaf
// first. Non-certificate blocks are stepped over so a file that keeps the key
// beside the cert still resolves.
func leafCertDER(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for rest := data; len(rest) > 0; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes, nil
		}
	}
	return nil, errors.New("no CERTIFICATE block")
}

// checkMCPSettingsInject verifies that ~/.claude/settings.json contains the
// tether-managed mcpServers.tether entry injected by `tether server`, and that
// it points at this deployment.
//
// --skip-mcp-inject is now honoured (it was the residual noted here before
// tether#89): a server started with it never writes the entry, so failing its
// absence was reporting the flag working as a fault. The environment-variable
// half of that residual — TETHER_NO_PERMISSION_HOOK on checkCCSettingsHooks —
// is a different problem and stays open: a flag is something an operator can
// repeat to doctor, and an environment variable of a process doctor never
// identifies is not.
//
// The port comparison is the second half of the tether#89 story. The entry is
// host-global — one ~/.claude/settings.json, overwritten by whichever tether
// daemon started last — so on a host running two of them it can name a port
// this deployment does not serve, and then cc is wired to the other daemon.
// That is worth a red mark on its own: this check exists to answer "will cc
// reach this deployment", and a present-but-misaimed entry answers no. It can
// only be made when the operator passed --mcp-port; without it doctor has
// nothing to compare against and says nothing.
func checkMCPSettingsInject(cfg *server.Config, verbose bool) CheckResult {
	if cfg.SkipMCPInject {
		return skip("mcp-settings-inject", "--skip-mcp-inject: this deployment does not write cc's settings.json entry, so there is nothing here to check")
	}
	inj, err := readInjectedMCP()
	if err != nil {
		return skip("mcp-settings-inject", "cannot determine home dir: "+err.Error())
	}
	switch {
	case inj.ReadErr != nil:
		return fail("mcp-settings-inject", "~/.claude/settings.json not found — run `tether server` to inject")
	case inj.ParseErr != nil:
		return fail("mcp-settings-inject", "~/.claude/settings.json: parse error: "+inj.ParseErr.Error())
	case !inj.Found:
		return fail("mcp-settings-inject", "tether MCP server not in settings.json — run `tether server` to inject")
	}
	if cfg.MCPPort != 0 && inj.Port != 0 && inj.Port != cfg.MCPPort {
		return fail("mcp-settings-inject", fmt.Sprintf(
			"settings.json points cc at port %d, but --mcp-port says this deployment serves MCP on %d — cc is wired to a different tether (re-run `tether server` on this host, or drop --skip-mcp-inject from it)",
			inj.Port, cfg.MCPPort))
	}
	// The port goes in the message, not just --verbose's Detail. What this
	// check verifies is that an entry is present, and an entry can be present
	// and aimed at a different daemon; when no --mcp-port was passed there is
	// nothing to compare it against, and the reader is the only one left who
	// can notice.
	r := ok("mcp-settings-inject", fmt.Sprintf("tether MCP server injected in settings.json (cc will use %s)", ccTarget(inj)))
	if verbose {
		r.Detail = "url=" + inj.URL
	}
	return r
}

// ccTarget describes, in the fewest words that stay true, where the injected
// entry sends cc.
func ccTarget(inj injectedMCP) string {
	if inj.Port == 0 {
		return "url " + inj.URL
	}
	return fmt.Sprintf("port %d", inj.Port)
}

// injectedMCP is what cc's ~/.claude/settings.json records about the tether
// MCP endpoint: the singleton entry agent.InjectMCPServer writes under the key
// "tether", with the port and bearer token it was given.
//
// The key is matched exactly rather than by scanning every tether-managed
// entry, which is what checkMCPLoopback used to do. Per-task MCP instances
// inject alongside it under "tether-<slug>" (internal/mcp/instance/instance.go),
// each on its own port and with its own token, so the scan picked whichever one
// Go's map iteration reached last — a different endpoint from one doctor run to
// the next. Only the "tether" entry names the loopback Run starts.
type injectedMCP struct {
	Path     string
	ReadErr  error // settings.json missing or unreadable
	ParseErr error // settings.json is not JSON
	Found    bool  // a tether-managed mcpServers.tether entry is present
	URL      string
	Port     int    // 0 when the URL carries no usable port
	Token    string // from headers.Authorization; "" when absent
}

// readInjectedMCP loads that entry. The returned error is reserved for "there
// is no home directory to look in"; everything else is a field, because the
// callers report the same conditions differently.
func readInjectedMCP() (injectedMCP, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return injectedMCP{}, err
	}
	inj := injectedMCP{Path: filepath.Join(home, ".claude", "settings.json")}
	data, err := os.ReadFile(inj.Path)
	if err != nil {
		inj.ReadErr = err
		return inj, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		inj.ParseErr = err
		return inj, nil
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	entry, _ := mcpServers["tether"].(map[string]any)
	if entry == nil {
		return inj, nil
	}
	if managed, _ := entry[agent.TetherManagedKey].(bool); !managed {
		return inj, nil
	}
	inj.Found = true
	inj.URL, _ = entry["url"].(string)
	if u, err := url.Parse(inj.URL); err == nil {
		if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
			inj.Port = p
		}
	}
	if headers, _ := entry["headers"].(map[string]any); headers != nil {
		if authz, _ := headers["Authorization"].(string); authz != "" {
			// Trimmed, and not only for tidiness: an untrimmed " " survives the
			// non-empty test in mcpBearerToken, gets offered as a credential,
			// and comes back 401 — turning a stray space in a hand-edited
			// settings.json into a red mcp-loopback on a healthy host.
			inj.Token = strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
		}
	}
	return inj, nil
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

// checkMCPLoopback verifies that the MCP loopback endpoint this deployment
// would serve is up, and that the thing answering on it is this deployment.
//
// Connecting is not the check. Before tether#89 it was: a 2s TCP dial that
// closed the connection and reported a tick, which on a host running a second
// tether daemon — the port being a host-wide default — meant `tether doctor`
// told an operator their MCP endpoint was healthy while looking at somebody
// else's. That is worse than the wrong-cert false alarm tether#84 removed. A
// check that goes red on a healthy host wastes an afternoon; a check that goes
// green on a broken one is why nobody looked.
//
// So the handshake is completed instead: an MCP initialize, carrying the bearer
// token out of ~/.tether/mcp-token, and the session it opens is deleted again
// (probeMCPEndpoint). Both halves of that carry evidence. The token is a fresh
// random secret per daemon start, so an endpoint that accepts it is one that
// holds the same secret — which is what makes the answer about *this*
// deployment rather than about tether-in-general; and only something speaking
// MCP returns an initialize result, which is what makes it about an MCP
// endpoint rather than about whatever else took the port.
//
// The identity that establishes is exactly "a process holding the secret this
// host's ~/.tether/mcp-token currently contains" — no more. Two daemons started
// under one $HOME write that file in turn and the later start wins it, so
// doctor answers for the survivor and calls the earlier one a stranger. Nothing
// else on the host tells two same-$HOME daemons apart, and no flag can: the
// cert flags tether#84 added distinguish deployments by what they serve, and
// two daemons under one $HOME may serve identical certs.
//
// Verdicts, and why they are not all failures:
//
//   - nothing listening: skip. doctor is a preflight command, routinely run
//     before the server exists, so a refused connection asserts nothing.
//   - handshake accepted: ok — the only path to a tick.
//   - 401: fail. Not because a 401 identifies what is there (any HTTP service
//     can send one), but because it rules out what would have to be there:
//     this deployment's own loopback compares against this very token
//     (MCPLoopback.ServeHTTP) and would have accepted it. So the port is held
//     by another process, and if this deployment serves MCP there `tether
//     server` will not start at all — Run returns the loopback's listen error
//     (Step 3b).
//   - anything else — no token to offer, a non-MCP answer, a stall: skip, with
//     the message saying that only the port being held was established. An
//     honest "could not tell" is the whole point of the third state.
func checkMCPLoopback(cfg *server.Config, verbose bool) CheckResult {
	port, portSrc := resolveMCPPort(cfg)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	endpoint := fmt.Sprintf("http://%s/mcp", addr)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return skip("mcp-loopback", fmt.Sprintf("MCP loopback not reachable at %s (%s; is `tether server` running?)", addr, portSrc))
	}
	_ = conn.Close()

	token, tokenSrc := mcpBearerToken(cfg)
	if token == "" {
		return skip("mcp-loopback", fmt.Sprintf(
			"%s is held by something, but doctor has no bearer token for this deployment (%s), so it cannot tell whose — only that the port is taken%s",
			addr, tokenSrc, portSrc.hint()))
	}

	id, err := probeMCPEndpoint(endpoint, token, 5*time.Second)
	switch {
	case errors.Is(err, errMCPUnauthorized):
		return fail("mcp-loopback", fmt.Sprintf(
			"%s is held by an endpoint that rejects the bearer token this host records (%s) with a 401, so it is not the daemon that wrote that record. Either another process has the port — in which case `tether server` cannot start here, its loopback listener will not bind — or a second daemon under this $HOME has overwritten the token file since this one started%s",
			addr, tokenSrc, portSrc.hint()))
	case err != nil:
		return skip("mcp-loopback", fmt.Sprintf(
			"%s is held by something that did not answer an MCP initialize (%v) — only that the port is taken was established%s",
			addr, err, portSrc.hint()))
	}

	// The port source is named even on the green line. It is the one thing this
	// verdict cannot establish for itself: the handshake proves whose endpoint
	// answered, never that the port it answered on is the port this deployment
	// serves. With two daemons under one $HOME the later start owns both the
	// settings.json entry and the token file, so a bare `tether doctor` would
	// hand back a tick for the wrong daemon — tether#89 again, one indirection
	// further along — and the reader has to be able to see that from the line
	// itself, not from --verbose.
	r := ok("mcp-loopback", fmt.Sprintf("MCP loopback at %s is this deployment's (initialize answered by %s %s; %s)",
		addr, id.ServerName, id.ServerVersion, portSrc))
	if verbose {
		r.Detail = fmt.Sprintf("endpoint=%s (%s); token from %s; protocolVersion=%s",
			endpoint, portSrc, tokenSrc, id.ProtocolVersion)
	}
	return r
}

// mcpPortSource records how the port under test was arrived at. An enum and
// not the message string it renders to, so that hint() branches on the fact
// rather than on prose someone will reword.
type mcpPortSource int

const (
	portFromFlag mcpPortSource = iota
	portFromSettings
	portFromDefault
)

func (s mcpPortSource) String() string {
	switch s {
	case portFromFlag:
		return "port from --mcp-port"
	case portFromSettings:
		return "port from cc's settings.json"
	default:
		return "assuming the default port"
	}
}

// hint nudges the operator towards the one flag that would remove the
// guesswork, and only when there is guesswork to remove.
func (s mcpPortSource) hint() string {
	if s == portFromFlag {
		return ""
	}
	return " (" + s.String() + " — pass --mcp-port to say which one this deployment uses)"
}

// resolveMCPPort works out which port this deployment's MCP loopback uses.
//
// --mcp-port first. The comment this replaces preferred cc's settings.json on
// the grounds that the injected URL "is a record of what the last-started
// server actually used, which is a better source than a flag nobody passed to
// doctor" — and the flag really was unpassable then, since `tether doctor` had
// none. Now that it has one, the same reasoning inverts: a flag the operator
// did pass is a statement about the deployment in front of them, while "the
// last-started server" is precisely the wrong daemon on the host this wi is
// about. It is also the rule tether#84 settled for the cert flags — run doctor
// the way you run the server and it reports on your deployment.
//
// settings.json second, and only when this deployment writes it. Under
// --skip-mcp-inject the entry is known to have been left by some other daemon,
// which makes it evidence about that one and not about this one.
//
// The bare default last, and it is a guess — 8899 is what lifecycle.go falls
// back to, so it is the right guess for a deployment that passed no flag, but
// nothing about this host confirms that is the deployment in front of us.
// Every ambiguous verdict says so (mcpPortSource.hint).
func resolveMCPPort(cfg *server.Config) (int, mcpPortSource) {
	if cfg.MCPPort != 0 {
		return cfg.MCPPort, portFromFlag
	}
	if !cfg.SkipMCPInject {
		if inj, err := readInjectedMCP(); err == nil && inj.Found && inj.Port != 0 {
			return inj.Port, portFromSettings
		}
	}
	return 8899, portFromDefault
}

// mcpBearerToken returns the secret this host holds for its own MCP loopback,
// with a phrase naming where it came from.
//
// ~/.tether/mcp-token first: Run regenerates and writes it on every start,
// before the settings.json injection and regardless of --skip-mcp-inject, so it
// is the record a deployment leaves whether or not it wires cc up. Not a
// guaranteed one — Run only warns if the write fails and carries on — which is
// why an absent token is a skip and never a verdict.
//
// cc's settings.json second, for the case the file is unreadable — the same
// token is embedded there as an Authorization header. It is a weaker source for
// the same reason it is a weaker port source (host-global, last writer wins),
// and it is skipped entirely under --skip-mcp-inject, where it certainly
// belongs to someone else.
func mcpBearerToken(cfg *server.Config) (token, source string) {
	path, err := server.MCPTokenPath()
	if err == nil {
		if data, readErr := os.ReadFile(path); readErr == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				return tok, "~/.tether/mcp-token"
			}
		}
	}
	if !cfg.SkipMCPInject {
		if inj, injErr := readInjectedMCP(); injErr == nil && inj.Found && inj.Token != "" {
			return inj.Token, "the Authorization header in cc's settings.json"
		}
	}
	return "", "~/.tether/mcp-token is absent, empty or unreadable"
}

// mcpIdentity is what a completed handshake said about the endpoint.
type mcpIdentity struct {
	ServerName      string
	ServerVersion   string
	ProtocolVersion string
}

// errMCPUnauthorized means the endpoint answered, and rejected our token. It is
// the one probe outcome that is a positive finding rather than an absence of
// one — not because it says what is there, but because this deployment's own
// loopback would have accepted the token, so whatever is there is not it.
var errMCPUnauthorized = errors.New("401 unauthorized")

// probeMCPEndpoint performs an MCP initialize against endpoint and terminates
// the session it opens.
//
// The session is deleted rather than left to time out because doctor is run
// against live daemons: MCPLoopback gives the SDK handler a 30-minute
// SessionTimeout, so a diagnostic that walked away would leave half an hour of
// server state behind every time somebody ran it. DELETE with the
// Mcp-Session-Id is the spec's session termination and the SDK's handler
// answers it 204 (streamable.go).
//
// Deliberately hand-rolled rather than driven through mcp.Client: the SDK's
// streamable client opens a standalone SSE stream and retries a failed
// connection five times by default, and a preflight probe wants neither. It
// wants one request, one answer, and a bounded wait.
func probeMCPEndpoint(endpoint, token string, timeout time.Duration) (mcpIdentity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Not built with encoding/json: every field is a constant, and the shape is
	// pinned to the wire format rather than to whatever a struct happens to
	// marshal to.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"tether-doctor","version":"1"}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return mcpIdentity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Both media types, because the SDK handler rejects a POST that does not
	// accept both (streamable.go) — and it answers this one as SSE.
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		return mcpIdentity{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return mcpIdentity{}, errMCPUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return mcpIdentity{}, fmt.Errorf("HTTP %s", resp.Status)
	}
	// Armed only once the status is known good. A session is opened by a 200,
	// and issuing the DELETE off a header seen on some other status would be
	// sending this deployment's token somewhere on the strength of a reply the
	// probe has already decided not to trust.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		defer terminateMCPSession(endpoint, token, sid)
	}

	payload, err := firstJSONRPCMessage(resp)
	if err != nil {
		return mcpIdentity{}, err
	}
	var rpc struct {
		Result *struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return mcpIdentity{}, fmt.Errorf("reply is not JSON-RPC: %w", err)
	}
	if rpc.Error != nil {
		return mcpIdentity{}, fmt.Errorf("initialize refused: %s", rpc.Error.Message)
	}
	if rpc.Result == nil || rpc.Result.ServerInfo.Name == "" {
		return mcpIdentity{}, errors.New("reply carries no initialize result")
	}
	// serverInfo.name is reported, not asserted on. BuildMCPServer hardcodes
	// "tether", but this package cannot keep a constant in step with that one,
	// and the token is what already settled whose endpoint this is — gating on
	// the string would only add a way for a rename to turn a healthy host red.
	return mcpIdentity{
		ServerName:      rpc.Result.ServerInfo.Name,
		ServerVersion:   rpc.Result.ServerInfo.Version,
		ProtocolVersion: rpc.Result.ProtocolVersion,
	}, nil
}

// firstJSONRPCMessage pulls one JSON-RPC message out of an initialize response,
// which the SDK sends as a one-event SSE stream but is permitted to send as
// plain JSON.
//
// Scanned line by line rather than read whole: an SSE body is a stream the
// server may hold open after the event we asked for, and io.ReadAll would then
// block until the context deadline and report a healthy endpoint as a timeout.
//
// The first *non-empty* data frame, not the first data frame. The SDK writes
// `data:` with an empty payload for its resumption priming event (writeEvent in
// event.go emits the field whether or not there is data), which today is
// double-gated off — MCPLoopback configures no EventStore, and priming belongs
// to a protocol version later than the one this probe pins — but "first frame"
// would turn every healthy endpoint into a skip the day either gate moves.
func firstJSONRPCMessage(resp *http.Response) ([]byte, error) {
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(mediaType) != "text/event-stream" {
		payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		return payload, nil
	}
	// The reader's cap is above the scanner's so that an over-long line is
	// reported as one ("token too long") rather than arriving truncated and
	// failing later as malformed JSON.
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 2<<20))
	// Scanner's default 64KiB line cap is below what an initialize result may
	// legitimately reach once a server fills in capabilities and instructions,
	// and hitting it would surface as a probe error against a healthy endpoint.
	sc.Buffer(make([]byte, 0, 4<<10), 1<<20)
	for sc.Scan() {
		data, found := strings.CutPrefix(sc.Text(), "data:")
		if !found {
			continue
		}
		if data = strings.TrimSpace(data); data != "" {
			return []byte(data), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("event stream carried no data frame")
}

// noRedirectClient is the only HTTP client this package uses.
//
// A preflight probe is asking about one socket, so following a hop would both
// answer about a different one and hand this deployment's bearer token to
// whoever named the target — and net/http would let it: Authorization survives
// a redirect to the same hostname on a different port, since the rule is
// isDomainOrSubdomain and ports are not part of it (net/http client.go,
// shouldCopyHeaderOnRedirect). Both requests go through here, because the
// second one — the session DELETE — carries the same token as the first and
// was the half that kept using http.DefaultClient.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// terminateMCPSession is best-effort: the probe has its answer by the time this
// runs, and a session the server declines to drop is its own business.
func terminateMCPSession(endpoint, token, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_ = resp.Body.Close()
}

// checkCCSettingsHooks verifies that ~/.claude/settings.json contains the
// tether-managed PreToolUse hook entry, and that the command that entry names
// will actually run.
//
// Presence was the whole check before tether#123, which made "tether PreToolUse
// hook is active" a claim about a JSON key rather than about a hook. The entry
// agent.InjectPermHook writes carries the thing that matters inside it —
// {"type":"command","command":"~/.tether/bin/tether-permission-hook"} — and a
// constructible, non-hypothetical host has that path pointing at nothing: a
// daemon killed with SIGKILL never runs RemovePermHook, so the entry outlives
// it, and ~/.tether/bin is then cleaned. cc goes on firing the hook for every
// tool call, every fire fails, and doctor printed a tick. Worse, a restart does
// not repair it: cchook.EnsureHookBinary early-returns on the .hash file beside
// the binary and never looks for the binary itself.
//
// So the command is looked up, with exec.LookPath, for the same reason
// checkCCBinary does it: the standard that check sets out for itself is to
// predict the spawn, and this is the only other check in the file with a spawn
// to predict. Its sibling half — whether a Go toolchain exists to compile that
// binary in the first place — is checkGoToolchain.
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
	// Every managed entry, not the first one found: cc fires all of them, so one
	// broken command breaks the gate however many working ones sit beside it.
	// InjectPermHook clears stale entries before adding its own, so more than one
	// is already a settings.json somebody else has edited.
	var managedEntries int
	var runnable []string
	for _, h := range list {
		hm, isObj := h.(map[string]any)
		if !isObj {
			continue
		}
		if managed, _ := hm[agent.TetherManagedKey].(bool); !managed {
			continue
		}
		managedEntries++
		command, named := managedHookCommand(hm)
		if !named {
			return fail("cc-settings-hooks", "the tether-managed PreToolUse entry in ~/.claude/settings.json names no command, so cc has nothing to run for it and tool calls are not gated — re-run `tether server` to rewrite the entry")
		}
		bin, lookErr := exec.LookPath(command)
		if lookErr != nil {
			return fail("cc-settings-hooks", fmt.Sprintf(
				"cc's tether PreToolUse hook points at %q, which will not run: %v. Every tool call in a cc session spawns it, and restarting the daemon does not repair it — cchook.EnsureHookBinary recompiles only when %s.hash is missing or stale, never when the binary alone has gone — so delete %s.hash and restart `tether server`",
				command, lookErr, command, command))
		}
		runnable = append(runnable, bin)
	}
	if managedEntries == 0 {
		return fail("cc-settings-hooks", "tether hook not found in settings.json — run `tether server` to inject")
	}
	r := ok("cc-settings-hooks", fmt.Sprintf("tether PreToolUse hook is active and the command it names is executable (%s)", strings.Join(runnable, ", ")))
	if verbose {
		r.Detail = path
	}
	return r
}

// managedHookCommand pulls the command out of a tether-managed PreToolUse entry
// — the field agent.InjectPermHook fills with ~/.tether/bin/tether-permission-hook
// (lifecycle.go Step 3), and the one part of the entry that has to resolve on
// disk before cc can gate anything.
//
// The string is returned whole, to be looked up whole rather than split on
// whitespace: what tether writes is a single absolute path, $HOME may contain a
// space, and splitting would turn a working hook under "/home/my user/" into a
// red mark. It is trimmed for the same reason mcpBearerToken trims — a trailing
// newline from a hand-edited file is not part of the path, and would otherwise
// fail the lookup on a host where the hook is fine.
//
// "type" is not gated on. command is the field that has to resolve, and a type
// string this build has not heard of is no reason to stop reading the entry that
// carries it.
func managedHookCommand(entry map[string]any) (string, bool) {
	nested, _ := entry["hooks"].([]any)
	for _, h := range nested {
		hm, isObj := h.(map[string]any)
		if !isObj {
			continue
		}
		if command, _ := hm["command"].(string); strings.TrimSpace(command) != "" {
			return strings.TrimSpace(command), true
		}
	}
	return "", false
}

// checkGoToolchain reports whether `go` is available to compile the permission
// hook, which the daemon does on the way up.
//
// This is the startup-fatal failure nothing in doctor looked for.
// cchook.EnsureHookBinary shells out to `go build` (embedded.go), and Step 3 of
// lifecycle.go returns its error as "perm hook compile: ..." straight out of
// Run — so on a host with no Go toolchain `tether server` exits rather than
// starting without a permission gate. docs/known-limitations.md records that as
// true of *every* install method, the release tarball included, whose pre-built
// hook the daemon never looks for. Meanwhile this file already checks that a
// binary it is about to spawn exists, twice (checkCCBinary, checkOpencodeBinary);
// the one whose absence stops the daemon dead was simply not among them.
//
// Absent is not automatically a failure, because the compile is conditional:
// EnsureHookBinary early-returns when the .hash file beside the binary already
// records the hash of the embedded source. Doctor cannot compute that hash — the
// source is unexported — so it reports the two states it can tell apart and says
// which one it is in:
//
//   - no .hash file: a compile is certain on the next start. A failure.
//   - .hash present: a compile happens only if this build's hook source differs
//     from the one that produced it, which doctor cannot know. A skip that says
//     so, rather than a verdict either way.
//
// A .hash whose binary has gone missing takes the same early-return and so is
// not this check's business; cc-settings-hooks is the one that catches it.
func checkGoToolchain(verbose bool) CheckResult {
	path, lookErr := exec.LookPath("go")
	if lookErr == nil {
		r := ok("go-toolchain", "go found — the daemon can compile its permission hook")
		if verbose {
			r.Detail = path
		}
		return r
	}
	hashPath, err := hookHashPath()
	if err != nil {
		return fail("go-toolchain", fmt.Sprintf(
			"go not usable (%v), and doctor cannot look for an already-compiled permission hook to say whether that matters: %v", lookErr, err))
	}
	if _, statErr := os.Stat(hashPath); statErr != nil {
		return fail("go-toolchain", fmt.Sprintf(
			"go not usable (%v) and %s does not exist, so `tether server` will try to compile its permission hook on the next start and exit with \"perm hook compile\" instead of starting — install a Go toolchain, or set TETHER_NO_PERMISSION_HOOK=1 to start without a permission gate",
			lookErr, hashPath))
	}
	r := skip("go-toolchain", fmt.Sprintf(
		"go not usable (%v), but %s exists, so a start needs the toolchain only if this build's embedded hook source has changed since that binary was compiled — doctor cannot compare the two",
		lookErr, hashPath))
	if verbose {
		r.Detail = hashPath
	}
	return r
}

// hookHashPath is where EnsureHookBinary records the source hash of the hook it
// compiled: binPath + ".hash", with binPath being ~/.tether/bin/tether-permission-hook
// (lifecycle.go Step 3).
//
// Rebuilt here rather than taken from server, whose tetherBinDir is unexported
// and creates the directory as a side effect — which a diagnostic must not do,
// since a check that repairs what it is measuring cannot report on it. That
// leaves one path literal in two packages; cc-settings-hooks needs no such copy,
// because the path it checks is the one cc records.
func hookHashPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tether", "bin", "tether-permission-hook.hash"), nil
}
