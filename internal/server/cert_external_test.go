package server

import (
	"context"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
)

// The bug these cover (tether#73): --cert-file/--key-file was read once at
// startup and never again, so an operator renewing the file on disk changed
// nothing until the daemon was restarted — and once the loaded bytes expired,
// every TCP and QUIC handshake failed while the process kept running and the
// log said nothing.
//
// Two properties have to hold together, and neither is worth much alone: a file
// replaced on disk has to reach the TLS handshake, and /cert-hash has to keep
// answering 404 across that reload. Advertising a CA-signed cert's hash makes
// Chrome drop the WebTransport connection with QUIC_NETWORK_IDLE_TIMEOUT, so a
// reload that lost CertBundle.External would trade an outage in ninety days for
// an outage now.

// seedOperatorPEM writes b to a fresh directory under the names certbot's
// live/ layout uses, and returns the two paths. Nothing depends on the names;
// they are there so a reader recognises what is being simulated.
func seedOperatorPEM(t *testing.T, b CertBundle) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	writeOperatorPEM(t, b, certPath, keyPath)
	return certPath, keyPath
}

// writeOperatorPEM replaces the pair in place, which is what a renewal does.
// persistCert writes to a temp file and renames, so the paths are never half
// written — the reload sees the old cert or the new one, never a mixture.
func writeOperatorPEM(t *testing.T, b CertBundle, certPath, keyPath string) {
	t.Helper()
	if err := persistCert(b, certPath, keyPath); err != nil {
		t.Fatalf("write operator cert files: %v", err)
	}
}

// writeOperatorChain writes leaf followed by extra into the cert file, the way
// a fullchain.pem carries an intermediate. crypto/tls keeps every CERTIFICATE
// block in Certificate[] and only matches the key against the first, so the
// pair stays valid and only the chain changes.
func writeOperatorChain(t *testing.T, leaf, extra CertBundle, certPath, keyPath string) {
	t.Helper()
	var buf []byte
	for _, der := range [][]byte{leaf.TLS.Certificate[0], extra.TLS.Certificate[0]} {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyPEM, err := marshalECKey(leaf.TLS.PrivateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := atomicWrite(certPath, buf, 0o600); err != nil {
		t.Fatalf("write chain: %v", err)
	}
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// managedHome points HOME at a temp dir so the managed cert store is a
// throwaway, and returns the paths inside it.
func managedHome(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// mustReload calls r.reload and fails the test if there is none.
func mustReload(t *testing.T, r certRenewal) CertBundle {
	t.Helper()
	if r.reload == nil {
		t.Fatal("no reload for this cert path")
	}
	b, err := r.reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return b
}

func TestLoadOrGenCert_ReadsTheOperatorsFilesAndMarksThemExternal(t *testing.T) {
	// A managed cert store is present and fresh, so a fallback would silently
	// succeed with the wrong cert rather than error.
	managedCertPath, managedKeyPath := managedHome(t)
	managed := genCertExpiringIn(t, 10*24*time.Hour)
	writeOperatorPEM(t, managed, managedCertPath, managedKeyPath)

	operator := genCertExpiringIn(t, 60*24*time.Hour)
	certPath, keyPath := seedOperatorPEM(t, operator)

	got, err := LoadOrGenCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenCert: %v", err)
	}
	if got.DER != operator.DER {
		t.Fatalf("loaded %s, want the operator's cert %s", HashHex(got.DER), HashHex(operator.DER))
	}
	if !got.External {
		t.Fatal("operator cert is not marked External — /cert-hash would advertise a hash for a CA cert")
	}
}

// Either half of the pair alone is not an operator cert. The two functions that
// answer that question have to answer it the same way, or the daemon loads one
// cert and maintains another.
func TestLoadOrGenCert_HalfAPairFallsBackToTheManagedCert(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cert, key      bool
		wantExternal   bool
		wantSameAsFile bool
	}{
		{"both", true, true, true, true},
		{"cert only", true, false, false, false},
		{"key only", false, true, false, false},
		{"neither", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			managedCertPath, managedKeyPath := managedHome(t)
			writeOperatorPEM(t, genCertExpiringIn(t, 10*24*time.Hour), managedCertPath, managedKeyPath)

			operator := genCertExpiringIn(t, 60*24*time.Hour)
			certPath, keyPath := seedOperatorPEM(t, operator)
			cfg := &Config{}
			if tc.cert {
				cfg.CertFile = certPath
			}
			if tc.key {
				cfg.KeyFile = keyPath
			}

			bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
			if err != nil {
				t.Fatalf("LoadOrGenCert(%q, %q): %v", cfg.CertFile, cfg.KeyFile, err)
			}
			if bundle.External != tc.wantExternal {
				t.Fatalf("External = %v, want %v", bundle.External, tc.wantExternal)
			}
			if same := bundle.DER == operator.DER; same != tc.wantSameAsFile {
				t.Fatalf("loaded the operator's file = %v, want %v", same, tc.wantSameAsFile)
			}

			// The pin: startup and renewal must agree about who owns this cert.
			// While both go through externalPEMSource this cannot fail — that is
			// the point. It fails the moment someone writes the condition out a
			// second time and the two copies drift, which is silent in both
			// directions (a reload armed with empty paths, or an operator cert
			// nothing re-reads).
			if got := mustReload(t, certRenewalFor(cfg)).External; got != bundle.External {
				t.Fatalf("reload says External=%v but startup loaded External=%v", got, bundle.External)
			}
		})
	}
}

func TestCertRenewalFor_OperatorFilesGetAFileReloadOnTheShorterInterval(t *testing.T) {
	// A managed store that would answer if the wrong reload were picked.
	managedCertPath, managedKeyPath := managedHome(t)
	writeOperatorPEM(t, genCertExpiringIn(t, 10*24*time.Hour), managedCertPath, managedKeyPath)

	operator := genCertExpiringIn(t, 60*24*time.Hour)
	certPath, keyPath := seedOperatorPEM(t, operator)

	r := certRenewalFor(&Config{CertFile: certPath, KeyFile: keyPath})
	if r.every != certReloadInterval {
		t.Fatalf("every = %v, want certReloadInterval %v", r.every, certReloadInterval)
	}
	got := mustReload(t, r)
	if got.DER != operator.DER {
		t.Fatalf("reload returned %s, want the operator's file %s", HashHex(got.DER), HashHex(operator.DER))
	}
	if !got.External {
		t.Fatal("reload dropped External — /cert-hash would start advertising a CA cert's hash")
	}
}

func TestCertRenewalFor_ManagedGetsTheMintingReload(t *testing.T) {
	managedHome(t)

	r := certRenewalFor(&Config{})
	if r.every != certRotateInterval {
		t.Fatalf("every = %v, want certRotateInterval %v", r.every, certRotateInterval)
	}
	got := mustReload(t, r)
	if got.External {
		t.Fatal("the managed reload marked its own cert External — /cert-hash would stop working")
	}
	if !certUsable(got.TLS.Leaf, time.Now()) {
		t.Fatal("the managed reload returned a cert that is not usable now")
	}
}

// certmagic renews an ACME cert inside its own tls.Config, which the listener
// uses directly (cfg.acmeTLSBase), so the holder is not on that serving path at
// all. Run's Step 4b applies that override even when --cert-file was also
// passed, so the combination has to answer the same way — re-reading files here
// would keep a cert current that nobody presents, and picking the managed
// reload would mint a self-signed cert over a CA-signed deployment.
func TestCertRenewalFor_ACMEGetsNoRenewalEvenAlongsideCertFiles(t *testing.T) {
	certPath, keyPath := seedOperatorPEM(t, mustGenCert(t))
	for _, tc := range []struct {
		name string
		cfg  *Config
	}{
		{"acme alone", &Config{AcmeDomain: "tether.example"}},
		{"acme with cert files", &Config{AcmeDomain: "tether.example", CertFile: certPath, KeyFile: keyPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := certRenewalFor(tc.cfg); r.reload != nil {
				t.Fatal("ACME must not be renewed through the holder")
			}
		})
	}
}

// The reload loop for the managed cert, reached through the new plumbing rather
// than by handing rotateCertLoop a stub — the tether#72 property has to survive
// the tether#73 refactor.
func TestCertRenewal_StartRotatesTheManagedCertNearExpiry(t *testing.T) {
	certPath, keyPath := managedHome(t)
	stale := genCertExpiringIn(t, certRotateThreshold/2)
	writeOperatorPEM(t, stale, certPath, keyPath)

	holder := newCertHolder(stale)
	r := certRenewalFor(&Config{})
	r.every = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.start(ctx, holder) {
		t.Fatal("start reported no loop for the managed cert")
	}
	waitFor(t, func() bool { return holder.Get().DER != stale.DER }, 5*time.Second,
		"a managed cert inside the rotation threshold was never replaced")
	if holder.Get().External {
		t.Fatal("the replacement is marked External; /cert-hash would 404 for a self-signed cert")
	}
}

// The tether#73 fix itself: a file replaced on disk becomes the live cert.
func TestCertRenewal_StartReloadsTheOperatorsFileAndKeepsItExternal(t *testing.T) {
	first := genCertExpiringIn(t, 60*24*time.Hour)
	renewed := genCertExpiringIn(t, 90*24*time.Hour)
	if first.DER == renewed.DER {
		t.Fatal("fixture certs are identical; the test could not distinguish them")
	}
	certPath, keyPath := seedOperatorPEM(t, first)

	cfg := &Config{CertFile: certPath, KeyFile: keyPath}
	bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatalf("LoadOrGenCert: %v", err)
	}
	holder := newCertHolder(bundle)

	r := certRenewalFor(cfg)
	r.every = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.start(ctx, holder) {
		t.Fatal("start reported no loop for an operator's cert files")
	}

	writeOperatorPEM(t, renewed, certPath, keyPath)

	waitFor(t, func() bool { return holder.Get().DER == renewed.DER }, 5*time.Second,
		"the renewed file on disk never became the live cert — this is tether#73")
	if !holder.Get().External {
		t.Fatal("the reloaded bundle lost External; /cert-hash would advertise a hash for a CA cert")
	}
}

// Nothing here may mint a cert for the operator's path, and a cert already
// inside the managed rotation threshold is the input that would tempt it: the
// managed reload would answer such a cert by generating a self-signed
// replacement and persisting it, which for a CA-signed deployment is a worse
// outage than the one this wi fixes.
func TestCertRenewal_StartNeverMintsOverAnOperatorsNearExpiryCert(t *testing.T) {
	// A writable managed store, so a mistakenly managed reload would succeed
	// rather than error out and look like a passing test.
	managedHome(t)

	expiring := genCertExpiringIn(t, certRotateThreshold/2)
	certPath, keyPath := seedOperatorPEM(t, expiring)

	r := certRenewalFor(&Config{CertFile: certPath, KeyFile: keyPath})
	r.every = time.Millisecond
	// Count ticks through the production reload instead of sleeping for a fixed
	// time: "it never minted" is only evidence if the loop actually ran.
	inner := r.reload
	calls := make(chan struct{}, 64)
	r.reload = func() (CertBundle, error) {
		select {
		case calls <- struct{}{}:
		default:
		}
		return inner()
	}

	bundle, err := LoadOrGenCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenCert: %v", err)
	}
	holder := newCertHolder(bundle)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.start(ctx, holder) {
		t.Fatal("start reported no loop for an operator's cert files")
	}
	for range 5 {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal("the loop never called reload")
		}
	}
	cancel()

	if got := holder.Get(); got.DER != expiring.DER {
		t.Fatalf("the operator's near-expiry cert was replaced with %s — a self-signed cert over a CA-signed deployment",
			HashHex(got.DER))
	}
	if !holder.Get().External {
		t.Fatal("the holder lost External")
	}
}

// The wiring hop, through the real constructors: newServer's two listeners and
// buildMux's routes both read the holder, so a reload has to land on the
// handshake while /cert-hash stays 404 the whole way. Testing the reload in
// isolation would not show either.
func TestExternalCertReload_ReachesBothListenersAndCertHashStays404(t *testing.T) {
	first := genCertExpiringIn(t, 60*24*time.Hour)
	renewed := genCertExpiringIn(t, 90*24*time.Hour)
	certPath, keyPath := seedOperatorPEM(t, first)

	cfg := &Config{
		Port:         0,
		CertFile:     certPath,
		KeyFile:      keyPath,
		Registry:     session.NewRegistry(),
		MCPLifecycle: mcplifecycle.New(),
	}
	bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatalf("LoadOrGenCert: %v", err)
	}
	holder := newCertHolder(bundle)
	srv := newServer(cfg, holder, nil, nil, nil, nil, nil)
	mux := buildMux(cfg, holder, nil, cfg.Registry, nil, nil, nil, nil, nil, cfg.MCPLifecycle)

	certHash := func() (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cert-hash", nil))
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}

	if code, body := certHash(); code != http.StatusNotFound {
		t.Fatalf("/cert-hash before the reload: code=%d body=%s, want 404 for an operator cert", code, body)
	}
	// A real handshake, not just a GetCertificate call: the claim is that
	// crypto/tls consults the holder per connection.
	if got := handshakeDER(t, srv.tcp.TLSConfig); got != first.DER {
		t.Fatalf("TCP served %s before the reload, want %s", HashHex(got), HashHex(first.DER))
	}

	r := certRenewalFor(cfg)
	r.every = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.start(ctx, holder) {
		t.Fatal("start reported no loop for an operator's cert files")
	}

	writeOperatorPEM(t, renewed, certPath, keyPath)
	waitFor(t, func() bool { return holder.Get().DER == renewed.DER }, 5*time.Second,
		"the renewed file never reached the holder")

	if got := handshakeDER(t, srv.tcp.TLSConfig); got != renewed.DER {
		t.Fatalf("the TCP listener still serves %s after the reload, want %s", HashHex(got), HashHex(renewed.DER))
	}
	if got := certDERFrom(t, srv.h3.TLSConfig); got != renewed.DER {
		t.Fatalf("the UDP/h3 listener still serves %s after the reload, want %s", HashHex(got), HashHex(renewed.DER))
	}
	if code, body := certHash(); code != http.StatusNotFound {
		t.Fatalf("/cert-hash after the reload: code=%d body=%s — a CA cert's hash would make Chrome drop the WT connection", code, body)
	}
}

// certReloadInterval is the only bound on how long a renewed cert sits on disk
// unserved, since nothing in this process knows when an operator replaces the
// file. Zero would also panic time.NewTicker. Strictly shorter than the managed
// check, which is what the constant's doc claims: the managed loop can compute
// its own deadline from the cert's dates and this one cannot.
func TestCertReloadInterval_IsPositiveAndStrictlyShorterThanTheManagedCheck(t *testing.T) {
	if certReloadInterval <= 0 {
		t.Fatalf("certReloadInterval = %v; a non-positive tick panics time.NewTicker", certReloadInterval)
	}
	if certReloadInterval >= certRotateInterval {
		t.Fatalf("certReloadInterval %v is not shorter than the managed check %v; an operator's renewal would be picked up no sooner than a cert this process mints itself",
			certReloadInterval, certRotateInterval)
	}
}

// Renewing the chain without reissuing the leaf is a real operation — repairing
// a fullchain.pem whose intermediate is wrong or expired — and it is invisible
// to a comparison on CertBundle.DER, which hashes the leaf alone. The handshake
// presents the whole chain, so "nothing changed" has to be judged on the whole
// chain.
func TestExternalCertReload_PicksUpAChainOnlyRenewal(t *testing.T) {
	leaf := genCertExpiringIn(t, 60*24*time.Hour)
	intermediate := genCertExpiringIn(t, 90*24*time.Hour)
	certPath, keyPath := seedOperatorPEM(t, leaf)

	cfg := &Config{CertFile: certPath, KeyFile: keyPath}
	bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		t.Fatalf("LoadOrGenCert: %v", err)
	}
	if n := len(bundle.TLS.Certificate); n != 1 {
		t.Fatalf("fixture starts with %d certs, want 1", n)
	}
	holder := newCertHolder(bundle)

	r := certRenewalFor(cfg)
	r.every = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.start(ctx, holder) {
		t.Fatal("start reported no loop for an operator's cert files")
	}

	writeOperatorChain(t, leaf, intermediate, certPath, keyPath)

	waitFor(t, func() bool { return len(holder.Get().TLS.Certificate) == 2 }, 5*time.Second,
		"the repaired chain never reached the holder — the leaf did not change, so a DER comparison sees nothing")
	got := holder.Get()
	if got.DER != leaf.DER {
		t.Fatalf("the leaf changed (%s want %s); the fixture is not testing a chain-only renewal",
			HashHex(got.DER), HashHex(leaf.DER))
	}
	if !got.External {
		t.Fatal("the reloaded bundle lost External")
	}
}

// crypto/tls names a file only when the open itself failed. A half-written
// renewal — the cert already replaced, the key not yet — fails with "private
// key does not match public key", which names neither path, and the loop logs
// nothing about the source but err.
func TestLoadExternalPEM_ErrorNamesTheFilesCryptoTLSDoesNot(t *testing.T) {
	certPath, keyPath := seedOperatorPEM(t, mustGenCert(t))
	// Someone else's key: parses, does not match the cert.
	_, otherKeyPath := seedOperatorPEM(t, mustGenCert(t))
	other, err := os.ReadFile(otherKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, other, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = loadExternalPEM(certPath, keyPath)
	if err == nil {
		t.Fatal("mismatched key pair loaded without error")
	}
	if !strings.Contains(err.Error(), certPath) || !strings.Contains(err.Error(), keyPath) {
		t.Fatalf("error does not name both files, so the log cannot say which cert failed: %v", err)
	}
}

func TestLogCertMode_NamesWhatIsServedAndWhatMaintainsIt(t *testing.T) {
	certPath, keyPath := seedOperatorPEM(t, mustGenCert(t))
	managed := mustGenCert(t)
	operator, err := LoadOrGenCert(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenCert: %v", err)
	}

	for _, tc := range []struct {
		name     string
		cfg      *Config
		bundle   CertBundle
		want     []string
		unwanted []string
	}{
		{
			name:     "managed",
			cfg:      &Config{},
			bundle:   managed,
			want:     []string{"cert DER hash", HashHex(managed.DER)},
			unwanted: []string{"operator files"},
		},
		{
			// The line that did not exist before tether#73: without it, a
			// daemon that re-reads the operator's file and one that never will
			// look identical until the cert lapses.
			name:     "operator files",
			cfg:      &Config{CertFile: certPath, KeyFile: keyPath},
			bundle:   operator,
			want:     []string{"cert mode", "operator files", certPath, "reload_every=1m0s"},
			unwanted: []string{HashHex(operator.DER)},
		},
		{
			// Run's Step 4b has already replaced the bundle by this point, so
			// the cert files are not what is being served.
			name:     "acme wins over cert files",
			cfg:      &Config{AcmeDomain: "tether.example", CertFile: certPath, KeyFile: keyPath},
			bundle:   CertBundle{External: true},
			want:     []string{"acme=tether.example"},
			unwanted: []string{"operator files", certPath},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLogs(t)
			logCertMode(tc.cfg, tc.bundle)
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Fatalf("log does not mention %q: %s", w, out.String())
				}
			}
			for _, u := range tc.unwanted {
				if strings.Contains(out.String(), u) {
					t.Fatalf("log mentions %q, which is not what this daemon is serving: %s", u, out.String())
				}
			}
		})
	}
}

// The one hop no unit test can execute: Run binds two listeners and spawns MCP,
// so nothing here calls it. What breaks silently is not the functions but the
// wiring between them — deleting the startCertRotation call, or handing it a
// Config that is not the one the flags populated, leaves a daemon that builds
// its cert correctly and then never maintains it, which is tether#73 restored
// in full and invisible to every other test in this package.
//
// So parse Run instead. The precedent is internal/wire/errors_test.go, which
// reads its own package's AST for the same reason: a guard that cannot go stale
// because it looks at whatever the source currently says.
func TestRun_WiresTheCertHolderIntoBothTheListenersAndTheRenewal(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the server package: %v", err)
	}

	var run *ast.FuncDecl
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "Run" {
					run = fd
				}
			}
		}
	}
	if run == nil {
		t.Fatal("no func Run in package server")
	}

	// callArgs["newServer"] = the identifier names Run passes, "" for anything
	// that is not a bare identifier (a literal, a field, a call).
	callArgs := map[string][]string{}
	ast.Inspect(run, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		args := make([]string, 0, len(call.Args))
		for _, a := range call.Args {
			if id, ok := a.(*ast.Ident); ok {
				args = append(args, id.Name)
			} else {
				args = append(args, "")
			}
		}
		callArgs[fn.Name] = args
		return true
	})

	server, ok := callArgs["newServer"]
	if !ok || len(server) < 2 {
		t.Fatalf("Run does not call newServer with the shape this test assumes: %v", server)
	}
	holder := server[1]
	if holder == "" {
		t.Fatal("Run passes newServer a cert holder that is not a plain variable; this guard cannot follow it")
	}

	renewal, ok := callArgs["startCertRotation"]
	if !ok {
		t.Fatal("Run never calls startCertRotation: the cert is loaded once and never maintained again — tether#73")
	}
	if len(renewal) != 4 {
		t.Fatalf("startCertRotation called with %d args, want 4", len(renewal))
	}
	if renewal[1] != "cfg" {
		t.Fatalf("startCertRotation gets %q, not the cfg the flags populated; every cert path would look managed", renewal[1])
	}
	if renewal[3] != holder {
		t.Fatalf("startCertRotation updates %q but the listeners serve %q; a reload would never reach a handshake",
			renewal[3], holder)
	}

	mode, ok := callArgs["logCertMode"]
	if !ok {
		t.Fatal("Run never calls logCertMode: the operator-cert path is silent again")
	}
	if len(mode) != 2 || mode[0] != "cfg" {
		t.Fatalf("logCertMode called with %v, want (cfg, <bundle>)", mode)
	}
	if renewal[2] != mode[1] {
		t.Fatalf("startCertRotation judges %q but the log describes %q; they must be the same bundle",
			renewal[2], mode[1])
	}
}
