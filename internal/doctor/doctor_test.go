package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/server"
)

// writeJSON writes v as JSON to path, creating parent dirs.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMCPSettingsInject_Injected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"type":                 "http",
				"url":                  "http://127.0.0.1:8899/mcp",
			},
		},
	})
	// Point home to temp dir by temporarily overriding UserHomeDir via env.
	t.Setenv("HOME", dir)
	r := checkMCPSettingsInject(false)
	if r.Status != StatusOK {
		t.Errorf("expected ok, got %s message=%q", r.Status, r.Message)
	}
}

func TestCheckMCPSettingsInject_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := checkMCPSettingsInject(false)
	if !r.Failed() {
		t.Errorf("expected fail when settings.json absent, got %s message=%q", r.Status, r.Message)
	}
}

func TestCheckMCPSettingsInject_NotInjected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"url": "http://example.com"},
		},
	})
	t.Setenv("HOME", dir)
	r := checkMCPSettingsInject(false)
	if !r.Failed() {
		t.Errorf("expected fail when no tether entry, got %s message=%q", r.Status, r.Message)
	}
}

func TestCheckMCPAPITokens_NoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := checkMCPAPITokens(false)
	if r.Failed() {
		t.Errorf("missing api-tokens.json should not be an error: %q", r.Message)
	}
}

func TestCheckMCPAPITokens_WithTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tether", "api-tokens.json")
	writeJSON(t, path, map[string]any{
		"tokens": []map[string]any{
			{"id": "tok1", "name": "cursor", "hash": "abc"},
			{"id": "tok2", "name": "goose", "hash": "def"},
		},
	})
	t.Setenv("HOME", dir)
	r := checkMCPAPITokens(false)
	if r.Status != StatusOK {
		t.Errorf("expected ok, got %s: %q", r.Status, r.Message)
	}
	if r.Message != "2 external API token(s) configured" {
		t.Errorf("unexpected message: %q", r.Message)
	}
}

func TestCheckMCPAPITokens_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tether", "api-tokens.json")
	writeJSON(t, path, map[string]any{"tokens": []any{}})
	t.Setenv("HOME", dir)
	r := checkMCPAPITokens(false)
	if r.Failed() {
		t.Errorf("empty store should not be an error: %q", r.Message)
	}
}

func TestCheckMCPLoopback_NotRunning(t *testing.T) {
	// Point the check at a port nothing holds, rather than letting it fall back
	// to the default 8899: a developer box running a real tether daemon has
	// that port open, and this test would then assert the opposite of what it
	// is named for. (It passed either way before tether#84 — the check returned
	// OK=true whether or not it connected — so the dependency was invisible.)
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", closedPort(t)),
			},
		},
	})
	t.Setenv("HOME", dir)

	// Unreachable is a skip, not a pass and not a failure: doctor runs before
	// the server exists, so a refused connection asserts nothing about the
	// endpoint's health.
	r := checkMCPLoopback(false)
	if r.Status != StatusSkip {
		t.Errorf("unreachable loopback should skip, got %s: %q", r.Status, r.Message)
	}
}

// closedPort returns a port that was bindable a moment ago and is now free.
// Not a guarantee — nothing can reserve a port without holding it — but the
// kernel does not hand the same ephemeral port straight back out.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCheckMCPLoopback_Running(t *testing.T) {
	// Start a throwaway TCP listener on an ephemeral port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	// Inject settings pointing at our listener.
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url": fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			},
		},
	})
	t.Setenv("HOME", dir)

	r := checkMCPLoopback(false)
	if r.Status != StatusOK {
		t.Errorf("expected ok with running listener, got %s: %q", r.Status, r.Message)
	}
	if r.Message == "" || contains(r.Message, "not reachable") {
		t.Errorf("expected reachable message, got: %q", r.Message)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// The bug these cover (tether#84): checkCertState read ~/.tether/cert.pem for
// every deployment, so it reported "cert not found" against a healthy
// --cert-file host and reported on the wrong file entirely against an ACME one.
// Each test below is written so that the pre-fix code fails it.

// writeCertPEM writes a self-signed cert expiring at notAfter. Just the
// certificate — the managed and ACME paths are judged on it alone, since both
// re-create the pair themselves when it is incomplete. The operator path is
// not; see writeOperatorPair.
func writeCertPEM(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	certPEM, _ := genCertPEM(t, notAfter)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// genCertPEM returns a self-signed cert and the key that signed it.
//
// The key comes back rather than being discarded because the pair has to stay a
// pair: an earlier version of writeOperatorPair generated a second, unrelated
// key, so every "healthy operator deployment" fixture was in fact a cert and a
// key that crypto/tls refuses to load together. Every assertion still passed,
// because nothing checked. The check that found it is the one this file gained
// for exactly that failure — a fixture bug is what a false pass looks like from
// the inside.
func genCertPEM(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tether-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// writeOperatorPair writes a matching cert and key into a fresh directory, the
// way certbot's live/ layout holds them.
func writeOperatorPair(t *testing.T, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	certPEM, keyPEM := genCertPEM(t, notAfter)
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// managedCert seeds ~/.tether/cert.pem — the file the old check read no matter
// what, and the one the operator-file and ACME tests below must not read.
func managedCert(t *testing.T, home string, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".tether", "cert.pem")
	writeCertPEM(t, path, notAfter)
	return path
}

func TestCheckCertState_ManagedCertIsStillChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{}, false)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "managed") {
		t.Errorf("message does not say which cert was checked: %q", r.Message)
	}
}

// A missing managed cert is not a failure, and the message has to leave room
// for the other reading: no cert flags were passed, which is also the state of
// knowing nothing about a host that may well be serving --cert-file.
func TestCheckCertState_MissingManagedCertSkipsRatherThanFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	r := checkCertState(&server.Config{}, false)
	if r.Status != StatusSkip {
		t.Fatalf("status = %s, want skip: %q", r.Status, r.Message)
	}
	for _, want := range []string{"--cert-file", "--acme-domain"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message does not mention %s, so the reader cannot tell what to re-run: %q", want, r.Message)
		}
	}
}

// The headline case: a --cert-file deployment with no managed cert at all. The
// old check reported "cert not found — run `tether server` to generate" here.
func TestCheckCertState_OperatorFilesAreCheckedWhereTheyLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, keyPath := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, true)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Detail, certPath) {
		t.Errorf("verbose detail does not name the file that was read: %q", r.Detail)
	}
}

// A named file that is not there is a real fault on this path, and a different
// one from the managed case: LoadOrGenCert returns the read error and Run
// propagates it, so the server does not start.
func TestCheckCertState_MissingOperatorCertFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A healthy managed cert is present precisely so a check that fell back to
	// it would pass and this test would catch it.
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: "/nope/fullchain.pem", KeyFile: "/nope/privkey.pem"}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "/nope/fullchain.pem") {
		t.Errorf("message does not name the missing file: %q", r.Message)
	}
}

// A valid cert next to a missing key is the false pass this check could most
// easily have shipped: the cert parses, the expiry is months out, and the
// server still refuses to start because crypto/tls loads the pair or nothing.
func TestCheckCertState_OperatorCertWithoutItsKeyFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, keyPath := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, keyPath) {
		t.Errorf("message does not name the missing key: %q", r.Message)
	}
}

// The managed path is excluded on purpose: loadOrRotateManaged re-mints the
// whole pair when it cannot load, so a missing key.pem is repaired rather than
// fatal, and failing the check would be the same false alarm in a new place.
func TestCheckCertState_ManagedCertWithoutItsKeyIsNotAFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{}, false)
	if r.Failed() {
		t.Errorf("status = fail with no key.pem present: %q", r.Message)
	}
}

// seedACMECert writes a cert into certmagic's layout under ~/.tether/acme.
// The path literals mirror certmagic v0.25.3's storage keys and are written out
// here rather than borrowed from the implementation, so that a wrong layout in
// the implementation cannot make this test agree with it.
func seedACMECert(t *testing.T, home, domain string, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".tether", "acme", "certificates",
		"acme-v02.api.letsencrypt.org-directory", domain, domain+".crt")
	writeCertPEM(t, path, notAfter)
	return path
}

// An ACME host has a ~/.tether/cert.pem too — Run mints it at Step 4 before
// Step 4b discards it for certmagic's — and nothing rotates it afterwards. So
// the old check did not merely look in the wrong place: given a daemon up for
// more than 14 days it went red, naming an expiry that had no bearing on what
// the daemon was serving. The expired managed cert here is what makes this test
// fail against the old code rather than pass by accident.
func TestCheckCertState_ACMEReadsTheStoredCertNotTheUnservedManagedOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(-time.Hour))
	want := seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, true)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Detail, want) {
		t.Errorf("verbose detail = %q, want the stored ACME cert %q", r.Detail, want)
	}
}

func TestCheckCertState_ACMEWithAnEmptyStoreSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
	if r.Status != StatusSkip {
		t.Fatalf("status = %s, want skip: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "example.com") {
		t.Errorf("message does not name the domain: %q", r.Message)
	}
}

// Expiry is still a failure — the fix is about which file is read, not about
// softening the verdict. What changes with the source is the remedy, because
// who is supposed to renew differs: the managed loop re-mints, an operator's
// certbot does not know about tether, and certmagic renews on its own.
func TestCheckCertState_NearExpiryFailsWithARemedyForThatCertPath(t *testing.T) {
	soon := time.Now().Add(30 * time.Minute)

	t.Run("managed", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		managedCert(t, home, soon)

		r := checkCertState(&server.Config{}, false)
		if !r.Failed() {
			t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
		}
		// Kept verbatim from before tether#84: managed certs really do rotate
		// within the hour since tether#72, so this sentence is true here — and
		// only here.
		if !strings.Contains(r.Message, "a running server rotates within the hour") {
			t.Errorf("managed remedy lost: %q", r.Message)
		}
	})

	t.Run("operator files", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		certPath, keyPath := writeOperatorPair(t, soon)

		r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, false)
		if !r.Failed() {
			t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "rotates within the hour") {
			t.Errorf("told the operator tether will rotate a cert it cannot issue: %q", r.Message)
		}
		if !strings.Contains(r.Message, certPath) {
			t.Errorf("remedy does not name the file to renew: %q", r.Message)
		}
	})

	t.Run("acme", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		seedACMECert(t, home, "example.com", soon)

		r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
		if !r.Failed() {
			t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "~/.tether writability") {
			t.Errorf("managed remedy leaked onto the ACME path: %q", r.Message)
		}
		if !strings.Contains(r.Message, "certmagic") {
			t.Errorf("remedy does not point at the thing that renews this cert: %q", r.Message)
		}
	})
}

// --cert-file without --key-file is ignored by the loader, which serves the
// managed cert instead. Reporting on the managed cert is therefore correct, and
// saying nothing about the ignored flag is how an operator ends up believing
// doctor inspected their cert.
func TestCheckCertState_LonePEMFlagIsCalledOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	for _, cfg := range []*server.Config{{CertFile: "/c.pem"}, {KeyFile: "/k.pem"}} {
		r := checkCertState(cfg, false)
		if r.Status != StatusOK {
			t.Fatalf("%+v: status = %s, want ok: %q", cfg, r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "only take effect together") {
			t.Errorf("%+v: ignored flag not reported: %q", cfg, r.Message)
		}
	}
}

// Both flags set means the pair is complete, so there is nothing to warn about
// and the hint must not fire.
func TestCheckCertState_NoLoneFlagNoiseWhenThePairIsComplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, keyPath := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, false)
	if strings.Contains(r.Message, "only take effect together") {
		t.Errorf("hint fired for a complete pair: %q", r.Message)
	}
}

// cc-binary asked PATH directly while the server asks ResolveClaudePath, so a
// host with $TETHER_CC_PATH set — or cc installed somewhere the caller's PATH
// does not cover — failed a check about a spawn that would have worked.
func TestCheckCCBinary_HonoursTheResolutionTheServerUses(t *testing.T) {
	dir := t.TempDir()
	ccPath := filepath.Join(dir, "cc-somewhere-else")
	if err := os.WriteFile(ccPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty PATH is what makes this a test of the resolver rather than of
	// the machine it runs on.
	t.Setenv("PATH", "")
	t.Setenv("TETHER_CC_PATH", ccPath)

	r := checkCCBinary(true)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if r.Detail != ccPath {
		t.Errorf("detail = %q, want the resolved path %q", r.Detail, ccPath)
	}
}

func TestCheckCCBinary_FailsWhenTheResolvedPathIsNotExecutable(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "cc-not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	t.Setenv("TETHER_CC_PATH", notExec)

	r := checkCCBinary(false)
	if !r.Failed() {
		t.Errorf("status = %s, want fail for a non-executable cc: %q", r.Status, r.Message)
	}
}

// A skip is not a failure anywhere it is read: not in Report.OK, and not in the
// derived ok field that --json still emits for pre-tether#84 consumers.
func TestCheckResultJSON_SkipSerialisesAsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		in     CheckResult
		wantOK bool
	}{
		{CheckResult{Name: "a", Status: StatusOK, Message: "all good", Detail: "d1"}, true},
		{CheckResult{Name: "b", Status: StatusSkip, Message: "could not look"}, true},
		{CheckResult{Name: "c", Status: StatusFail, Message: "broken", Detail: "d3"}, false},
	} {
		raw, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Name    string `json:"name"`
			OK      bool   `json:"ok"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.OK != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.in.Status, got.OK, tc.wantOK)
		}
		if got.Status != string(tc.in.Status) {
			t.Errorf("status = %q, want %q", got.Status, tc.in.Status)
		}
		// MarshalJSON restates the field list, so every field it forwards has
		// to be asserted here or it can be dropped without any test noticing —
		// which is what a hand-written marshaller costs. (Mutating Message and
		// Detail to "" left the suite green until this was added.)
		if got.Name != tc.in.Name || got.Message != tc.in.Message || got.Detail != tc.in.Detail {
			t.Errorf("payload lost content: got name=%q message=%q detail=%q, want name=%q message=%q detail=%q",
				got.Name, got.Message, got.Detail, tc.in.Name, tc.in.Message, tc.in.Detail)
		}
	}
}

// Report.OK is the exit code, so it has to mean "nothing failed" and not
// "everything passed" — otherwise a skip would still fail the command and the
// third state would have bought nothing.
//
// The host built here is the one the bug was reported from, minus the cert
// flags: everything doctor can check is in order, and the two things it cannot
// reach — which cert is served, and a daemon that is not running — skip. It has
// to exit 0. Asserting instead that report.OK agrees with the count of failures
// looks equivalent and is not: on a host that has both failures and skips, both
// readings say false, and a Run that failed the report over a skip passes that
// version of this test (verified by mutation).
func TestRun_SkipsDoNotFailTheReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".tether"), 0o700); err != nil {
		t.Fatal(err)
	}
	cc := filepath.Join(home, "cc")
	if err := os.WriteFile(cc, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_CC_PATH", cc)
	// One settings.json satisfies both the hook check and the MCP inject check;
	// its url points nowhere, which is what makes mcp-loopback skip.
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{agent.TetherManagedKey: true}},
		},
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", closedPort(t)),
			},
		},
	})

	report := Run(&server.Config{Port: closedPort(t)}, false)

	var skipped int
	for _, c := range report.Checks {
		if c.Status == StatusSkip {
			skipped++
		}
		if c.Failed() {
			t.Errorf("%s failed on a host with nothing wrong: %s", c.Name, c.Message)
		}
	}
	if skipped == 0 {
		t.Fatal("nothing skipped, so this proves nothing about skips")
	}
	if !report.OK {
		t.Errorf("report.OK = false with %d skips and no failures", skipped)
	}
}

// A cert whose location cannot be established is a skip: doctor has not learnt
// anything about the certificate, so saying it is broken would be inventing a
// verdict — the habit this whole change is about.
func TestCheckCertState_UnlocatableCertSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	acme := filepath.Join(home, ".tether", "acme")
	if err := os.MkdirAll(acme, 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the certificates directory belongs: unreadable as a store,
	// and unlike a permission bit it still reproduces when the tests run as root.
	if err := os.WriteFile(filepath.Join(acme, "certificates"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
	if r.Status != StatusSkip {
		t.Errorf("status = %s, want skip: %q", r.Status, r.Message)
	}
}

// The remaining false alarm after the flags were added: an ACME host run
// through a bare `tether doctor`. Its ~/.tether/cert.pem is real (Step 4 mints
// it) and nothing renews it (certRenewalFor gives ACME no loop), so past day 14
// doctor goes red about a file the daemon does not serve. The verdict on that
// file stands — doctor was not told otherwise — but the reader gets the flag
// that settles it.
func TestCheckCertState_ExpiredManagedCertPointsAtAnACMEStoreIfThereIsOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(-time.Hour))
	seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail — the managed cert really has expired: %q", r.Status, r.Message)
	}
	for _, want := range []string{"example.com", "--acme-domain"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message does not mention %s: %q", want, r.Message)
		}
	}
}

// No store, no note. A managed-only host must not be told to go and look for an
// ACME deployment it does not have.
func TestCheckCertState_NoACMENoteWithoutAStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(-time.Hour))

	r := checkCertState(&server.Config{}, false)
	if strings.Contains(r.Message, "--acme-domain") && strings.Contains(r.Message, "ACME cert store") {
		t.Errorf("invented an ACME store: %q", r.Message)
	}
}

// The note is for the managed branch only. Under --acme-domain the store is the
// subject of the check, not a hint about it.
func TestCheckCertState_NoACMENoteWhenACMEIsWhatIsBeingChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
	if strings.Contains(r.Message, "ACME cert store") {
		t.Errorf("hint fired while checking the ACME cert itself: %q", r.Message)
	}
}

// --acme-domain with an unreadable --cert-file is a host that does not boot,
// and the check used to walk straight past it: the served cert is certmagic's,
// so nothing looked at the files. Run's Step 4 loads them anyway, before Step
// 4b hands the listener to certmagic, and returns the error.
func TestCheckCertState_UnloadableOperatorPairFailsEvenUnderACME(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{
		AcmeDomain: "example.com",
		CertFile:   "/nope/fullchain.pem",
		KeyFile:    "/nope/privkey.pem",
	}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "/nope/fullchain.pem") {
		t.Errorf("message does not name the file that stops startup: %q", r.Message)
	}
}

// A key that exists but does not belong to the cert is the other way this pair
// fails to load, and os.Stat cannot see it — crypto/tls matches the public keys.
func TestCheckCertState_MismatchedOperatorKeyFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, _ := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))
	_, otherKey := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: otherKey}, false)
	if !r.Failed() {
		t.Errorf("status = %s for a key that does not match the cert: %q", r.Status, r.Message)
	}
}

// A host where $HOME cannot be resolved — a systemd unit without it is the
// usual way — is one where `tether server` does not start at all: Step 2's
// tetherDataDir returns this same error out of Run. Turning every home-dependent
// check into a skip made `tether doctor` exit 0 there, which is a worse lie
// than the false alarm this wi removed. data-dir is the check that owns saying
// so.
func TestCheckDataDir_UndeterminableHomeIsAFailure(t *testing.T) {
	t.Setenv("HOME", "")
	if r := checkDataDir(false); !r.Failed() {
		t.Errorf("status = %s, want fail: %q", r.Status, r.Message)
	}
}

func TestRun_UndeterminableHomeFailsTheReport(t *testing.T) {
	t.Setenv("HOME", "")
	if report := Run(&server.Config{Port: closedPort(t)}, false); report.OK {
		t.Error("report.OK = true on a host where the server cannot start")
	}
}
