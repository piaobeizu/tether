package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
)

// The bug these cover: a managed cert lives 14 days, but LoadOrGenCert ran once
// at startup and nothing re-checked it, so a long-lived daemon served an expired
// cert. The fix is only real if a rotation reaches BOTH observable surfaces —
// the TLS handshake and /cert-hash — because the browser pins the hash from the
// latter and validates it against the former. Rotating one without the other is
// worse than not rotating at all, so most of these tests are about the wiring
// rather than the timer.

func mustGenCert(t *testing.T) CertBundle {
	t.Helper()
	b, err := GenerateCert()
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	return b
}

// genCertExpiringIn builds a self-signed P-256 cert with an explicit lifetime.
// GenerateCert hardcodes 14 days, so it cannot produce the near-expiry input
// that the rotation predicate is supposed to react to.
func genCertExpiringIn(t *testing.T, d time.Duration) CertBundle {
	t.Helper()
	return genCertDated(t, time.Now().Add(-time.Hour), time.Now().Add(d))
}

// genCertDated is genCertExpiringIn with both ends explicit, so a cert can be
// dated entirely in the future — the wrong-clock case.
func genCertDated(t *testing.T, notBefore, notAfter time.Time) CertBundle {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tether"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	tlsCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return CertBundle{
		TLS:  tlsCert,
		DER:  sha256.Sum256(der),
		SPKI: sha256.Sum256(parsed.RawSubjectPublicKeyInfo),
	}
}

// handshakeDER completes a real TLS handshake against cfg and returns the
// SHA-256 of the certificate the server actually presented.
//
// A real handshake rather than "call GetCertificate directly": the point of the
// change is that crypto/tls consults the holder on every connection, and only
// driving the handshake proves the callback is on the path that matters.
func handshakeDER(t *testing.T, cfg *tls.Config) [32]byte {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	deadline := time.Now().Add(10 * time.Second)
	_ = clientConn.SetDeadline(deadline)
	_ = serverConn.SetDeadline(deadline)

	server := tls.Server(serverConn, cfg)
	client := tls.Client(clientConn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the test inspects the cert itself
		NextProtos:         cfg.NextProtos,
	})

	errCh := make(chan error, 1)
	go func() { errCh <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	peers := client.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		t.Fatal("server presented no certificate")
	}
	return sha256.Sum256(peers[0].Raw)
}

func waitFor(t *testing.T, cond func() bool, limit time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestCertHolder_GetReturnsWhatWasSet(t *testing.T) {
	first := mustGenCert(t)
	h := newCertHolder(first)
	if got := h.Get().DER; got != first.DER {
		t.Fatalf("seeded bundle not returned: got %s want %s", HashHex(got), HashHex(first.DER))
	}
	second := mustGenCert(t)
	h.Set(second)
	if got := h.Get().DER; got != second.DER {
		t.Fatalf("after Set: got %s want %s", HashHex(got), HashHex(second.DER))
	}
}

// The listener used to hold `Certificates: []tls.Certificate{bundle.TLS}`,
// which is exactly the shape that survives a rotation untouched. If someone
// reintroduces it, this fails.
func TestCertTLSConfig_ResolvesPerHandshakeNotFromAStaticSlice(t *testing.T) {
	cfg := certTLSConfig(newCertHolder(mustGenCert(t)), []string{"h3"})
	if len(cfg.Certificates) != 0 {
		t.Errorf("Certificates must stay empty so GetCertificate is consulted; got %d entries", len(cfg.Certificates))
	}
	if cfg.GetCertificate == nil {
		t.Error("GetCertificate is nil — the cert would be pinned for the life of the listener")
	}
	if want := []string{"h3"}; len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != want[0] {
		t.Errorf("NextProtos = %v, want %v", cfg.NextProtos, want)
	}
}

// The wiring hop: one tls.Config, two handshakes, a rotation in between.
func TestCertTLSConfig_HandshakeSeesRotatedCert(t *testing.T) {
	first := mustGenCert(t)
	second := mustGenCert(t)
	if first.DER == second.DER {
		t.Fatal("fixture certs are identical; the test could not distinguish them")
	}

	holder := newCertHolder(first)
	cfg := certTLSConfig(holder, []string{"h2"})

	if got := handshakeDER(t, cfg); got != first.DER {
		t.Fatalf("before rotation: served %s want %s", HashHex(got), HashHex(first.DER))
	}

	holder.Set(second)

	if got := handshakeDER(t, cfg); got != second.DER {
		t.Fatalf("after rotation the listener still served the old cert: got %s want %s",
			HashHex(got), HashHex(second.DER))
	}
}

func certHashBody(t *testing.T, h http.HandlerFunc) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/cert-hash", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// /cert-hash used to close over a precomputed string. After a rotation it would
// have kept advertising a hash for a cert the server no longer presents, and
// the browser pins that value — so the WT handshake would fail outright.
func TestHandleCertHash_ReflectsRotation(t *testing.T) {
	first := mustGenCert(t)
	second := mustGenCert(t)
	holder := newCertHolder(first)

	der := handleCertHash(holder, func(b CertBundle) [32]byte { return b.DER })
	spki := handleCertHash(holder, func(b CertBundle) [32]byte { return b.SPKI })

	if code, body := certHashBody(t, der); code != http.StatusOK || body != HashHex(first.DER) {
		t.Fatalf("before rotation: code=%d body=%s want 200/%s", code, body, HashHex(first.DER))
	}

	holder.Set(second)

	if code, body := certHashBody(t, der); code != http.StatusOK || body != HashHex(second.DER) {
		t.Fatalf("DER hash after rotation: code=%d body=%s want 200/%s", code, body, HashHex(second.DER))
	}
	if code, body := certHashBody(t, spki); code != http.StatusOK || body != HashHex(second.SPKI) {
		t.Fatalf("SPKI hash after rotation: code=%d body=%s want 200/%s", code, body, HashHex(second.SPKI))
	}
}

// The composite invariant the browser actually depends on: whatever /cert-hash
// advertises must be the cert the handshake presents, at every point in time.
// Either surface rotating alone is a broken connection.
func TestRotation_HashEndpointAndHandshakeStayInAgreement(t *testing.T) {
	holder := newCertHolder(mustGenCert(t))
	cfg := certTLSConfig(holder, []string{"h2"})
	der := handleCertHash(holder, func(b CertBundle) [32]byte { return b.DER })

	for round := range 3 {
		_, advertised := certHashBody(t, der)
		served := HashHex(handshakeDER(t, cfg))
		if advertised != served {
			t.Fatalf("round %d: /cert-hash says %s but the handshake served %s", round, advertised, served)
		}
		holder.Set(mustGenCert(t))
	}
}

func TestHandleCertHash_404ForExternalCert(t *testing.T) {
	external := mustGenCert(t)
	external.External = true
	h := handleCertHash(newCertHolder(external), func(b CertBundle) [32]byte { return b.DER })
	if code, _ := certHashBody(t, h); code != http.StatusNotFound {
		t.Fatalf("external cert must not advertise a hash: code=%d want 404", code)
	}
}

// newServer and buildMux are where production actually reads the holder, and
// each has exactly one caller — Run. Testing the helpers in isolation was not
// enough: review reintroduced the original bug at both call sites (a static
// Certificates slice in newServer, a precomputed hash string in buildMux) and
// every other test in this file stayed green. Neither function binds a port, so
// there was never a reason to leave them uncovered.
func newTestServer(t *testing.T, holder *certHolder) *Server {
	t.Helper()
	cfg := &Config{
		Port:         0,
		Registry:     session.NewRegistry(),
		MCPLifecycle: mcplifecycle.New(),
	}
	return newServer(cfg, holder, nil, nil, nil, nil, nil)
}

func certDERFrom(t *testing.T, cfg *tls.Config) [32]byte {
	t.Helper()
	if cfg.GetCertificate == nil {
		t.Fatal("GetCertificate is nil — the cert is fixed for the life of the listener")
	}
	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got == nil || len(got.Certificate) == 0 {
		t.Fatal("GetCertificate returned no certificate")
	}
	return sha256.Sum256(got.Certificate[0])
}

func TestNewServer_BothListenersResolveThroughTheHolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		pick func(*Server) *tls.Config
	}{
		{"TCP", func(s *Server) *tls.Config { return s.tcp.TLSConfig }},
		{"UDP/h3", func(s *Server) *tls.Config { return s.h3.TLSConfig }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := mustGenCert(t)
			second := mustGenCert(t)
			holder := newCertHolder(first)
			cfg := tc.pick(newTestServer(t, holder))

			if n := len(cfg.Certificates); n != 0 {
				t.Fatalf("listener pins %d static certificate(s); a rotation could never reach it", n)
			}
			if got := certDERFrom(t, cfg); got != first.DER {
				t.Fatalf("before rotation: %s want %s", HashHex(got), HashHex(first.DER))
			}

			holder.Set(second)

			if got := certDERFrom(t, cfg); got != second.DER {
				t.Fatalf("listener did not follow the rotation: %s want %s", HashHex(got), HashHex(second.DER))
			}
		})
	}
}

func TestBuildMux_CertHashRoutesReadTheHolder(t *testing.T) {
	first := mustGenCert(t)
	second := mustGenCert(t)
	holder := newCertHolder(first)

	cfg := &Config{Port: 0, Registry: session.NewRegistry(), MCPLifecycle: mcplifecycle.New()}
	mux := buildMux(cfg, holder, nil, cfg.Registry, nil, nil, nil, nil, nil, cfg.MCPLifecycle)

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}

	for _, r := range []struct {
		path string
		want func(CertBundle) [32]byte
	}{
		{"/cert-hash", func(b CertBundle) [32]byte { return b.DER }},
		{"/cert-hash-spki", func(b CertBundle) [32]byte { return b.SPKI }},
	} {
		if code, body := get(r.path); code != http.StatusOK || body != HashHex(r.want(first)) {
			t.Fatalf("%s before rotation: code=%d body=%s want 200/%s", r.path, code, body, HashHex(r.want(first)))
		}
	}

	holder.Set(second)

	for _, r := range []struct {
		path string
		want func(CertBundle) [32]byte
	}{
		{"/cert-hash", func(b CertBundle) [32]byte { return b.DER }},
		{"/cert-hash-spki", func(b CertBundle) [32]byte { return b.SPKI }},
	} {
		if code, body := get(r.path); code != http.StatusOK || body != HashHex(r.want(second)) {
			t.Fatalf("%s still advertises the pre-rotation cert: code=%d body=%s want 200/%s",
				r.path, code, body, HashHex(r.want(second)))
		}
	}
}

// The pairing that matters end to end: the hash the mux advertises must match
// the cert the listener presents, across a rotation, through the real
// constructors rather than the helpers.
func TestNewServerAndBuildMux_AgreeAcrossARotation(t *testing.T) {
	holder := newCertHolder(mustGenCert(t))
	cfg := &Config{Port: 0, Registry: session.NewRegistry(), MCPLifecycle: mcplifecycle.New()}
	srv := newServer(cfg, holder, nil, nil, nil, nil, nil)
	mux := buildMux(cfg, holder, nil, cfg.Registry, nil, nil, nil, nil, nil, cfg.MCPLifecycle)

	for round := range 3 {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cert-hash", nil))
		advertised := strings.TrimSpace(rec.Body.String())
		for name, tlsCfg := range map[string]*tls.Config{"TCP": srv.tcp.TLSConfig, "UDP/h3": srv.h3.TLSConfig} {
			if served := HashHex(certDERFrom(t, tlsCfg)); served != advertised {
				t.Fatalf("round %d: /cert-hash says %s but the %s listener serves %s", round, advertised, name, served)
			}
		}
		holder.Set(mustGenCert(t))
	}
}

// The one line in Run that decides whether rotation happens at all.
func TestStartCertRotation_RunsForManagedCertOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		external   bool
		wantsStart bool
	}{
		{"managed cert rotates", false, true},
		{"external cert is left to its owner", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := mustGenCert(t)
			bundle.External = tc.external
			holder := newCertHolder(bundle)
			replacement := mustGenCert(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			got := startCertRotation(ctx, bundle, holder, time.Millisecond, func() (CertBundle, error) {
				return replacement, nil
			})
			if got != tc.wantsStart {
				t.Fatalf("startCertRotation = %v, want %v", got, tc.wantsStart)
			}

			if tc.wantsStart {
				waitFor(t, func() bool { return holder.Get().DER == replacement.DER }, 5*time.Second,
					"reported that it started, but nothing ever rotated")
				return
			}
			// Give a loop that should not exist time to prove it does.
			time.Sleep(50 * time.Millisecond)
			if holder.Get().DER != bundle.DER {
				t.Fatal("external cert was rotated — a CA-signed cert would be replaced by a self-signed one")
			}
		})
	}
}

func TestRotateCertLoop_SwapsWhenReloadReturnsANewCert(t *testing.T) {
	first := mustGenCert(t)
	second := mustGenCert(t)
	holder := newCertHolder(first)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rotateCertLoop(ctx, holder, time.Millisecond, func() (CertBundle, error) { return second, nil })
	}()

	waitFor(t, func() bool { return holder.Get().DER == second.DER }, 5*time.Second,
		"rotateCertLoop never swapped in the cert that reload returned")
	cancel()
	<-done
}

// "Did not swap" and "swapped in an identical value" look the same from the
// outside, so the seeded bundle carries a marker that only a Set can clear.
func TestRotateCertLoop_SkipsSwapWhenDERUnchanged(t *testing.T) {
	base := mustGenCert(t)
	marked := base
	marked.External = true // marker only; a real managed bundle is never External
	holder := newCertHolder(marked)

	calls := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rotateCertLoop(ctx, holder, time.Millisecond, func() (CertBundle, error) {
			select {
			case calls <- struct{}{}:
			default:
			}
			return base, nil
		})
	}()

	// Let the loop actually run several times before judging it.
	for range 5 {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal("rotateCertLoop did not call reload")
		}
	}
	cancel()
	<-done

	if !holder.Get().External {
		t.Fatal("loop overwrote the holder even though the DER was unchanged")
	}
}

func TestRotateCertLoop_KeepsGoingAfterAReloadError(t *testing.T) {
	first := mustGenCert(t)
	second := mustGenCert(t)
	holder := newCertHolder(first)

	failures := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rotateCertLoop(ctx, holder, time.Millisecond, func() (CertBundle, error) {
			if failures < 3 {
				failures++
				return CertBundle{}, os.ErrPermission
			}
			return second, nil
		})
	}()

	waitFor(t, func() bool { return holder.Get().DER == second.DER }, 5*time.Second,
		"a transient reload error killed the loop; a disk hiccup would strand the cert forever")
	cancel()
	<-done

	if failures != 3 {
		t.Fatalf("expected the loop to retry past 3 failures, got %d", failures)
	}
}

func TestRotateCertLoop_ReturnsWhenContextIsCancelled(t *testing.T) {
	holder := newCertHolder(mustGenCert(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rotateCertLoop(ctx, holder, time.Hour, func() (CertBundle, error) { return mustGenCert(t), nil })
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rotateCertLoop ignored context cancellation — it would outlive shutdown")
	}
}

// The production reload. loadOrRotateManaged owns the actual "is it time?"
// decision, so the loop is only as correct as this is.
func TestLoadOrRotateManaged_RegeneratesAndPersistsNearExpiry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	stale := genCertExpiringIn(t, certRotateThreshold/2)
	if err := persistCert(stale, certPath, keyPath); err != nil {
		t.Fatalf("seed cert: %v", err)
	}

	got, err := loadOrRotateManaged()
	if err != nil {
		t.Fatalf("loadOrRotateManaged: %v", err)
	}
	if got.DER == stale.DER {
		t.Fatal("cert inside the rotation threshold was reused instead of replaced")
	}
	if left := time.Until(got.TLS.Leaf.NotAfter); left < certRotateThreshold {
		t.Fatalf("replacement expires in %v, still inside the %v threshold", left, certRotateThreshold)
	}

	// Persisted too — otherwise the next process start silently regresses.
	onDisk, err := loadPEMFiles(certPath, keyPath)
	if err != nil {
		t.Fatalf("reload persisted cert: %v", err)
	}
	if onDisk.DER != got.DER {
		t.Fatalf("on-disk cert %s does not match the returned one %s", HashHex(onDisk.DER), HashHex(got.DER))
	}
}

func TestLoadOrRotateManaged_ReusesACertThatIsStillFresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	fresh := genCertExpiringIn(t, 10*24*time.Hour)
	if err := persistCert(fresh, certPath, keyPath); err != nil {
		t.Fatalf("seed cert: %v", err)
	}

	got, err := loadOrRotateManaged()
	if err != nil {
		t.Fatalf("loadOrRotateManaged: %v", err)
	}
	if got.DER != fresh.DER {
		t.Fatal("a cert far from expiry was rotated anyway — every tick would mint a new one and the browser would never keep up")
	}
}

// --- guards added after review ---

// The success log reads Leaf.NotAfter. tls.X509KeyPair only populates Leaf when
// GODEBUG x509keypairleaf is not "0" — an operator-settable compat knob — so
// relying on it made a background goroutine panic-able. GenerateCert now sets
// Leaf from the cert it already parsed.
//
// The GODEBUG must be set for this to test anything: under the default the
// keypair fills Leaf on its own and the assertion passes whether or not
// GenerateCert does its part. Verified by mutation — without the setenv,
// deleting `tlsCert.Leaf = parsed` leaves this test green.
func TestGenerateCert_PopulatesLeafWithoutRelyingOnGODEBUG(t *testing.T) {
	t.Setenv("GODEBUG", "x509keypairleaf=0")
	b := mustGenCert(t)
	if b.TLS.Leaf == nil {
		t.Fatal("Leaf is nil with x509keypairleaf=0; rotateCertLoop's log would panic the daemon")
	}
	if got := sha256.Sum256(b.TLS.Leaf.Raw); got != b.DER {
		t.Fatalf("Leaf is not the cert in this bundle: %s vs %s", HashHex(got), HashHex(b.DER))
	}
}

// A nil leaf must degrade a log line, not kill the loop.
func TestRotateCertLoop_SurvivesABundleWithNoLeaf(t *testing.T) {
	first := mustGenCert(t)
	second := mustGenCert(t)
	second.TLS.Leaf = nil
	holder := newCertHolder(first)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rotateCertLoop(ctx, holder, time.Millisecond, func() (CertBundle, error) { return second, nil })
	}()
	waitFor(t, func() bool { return holder.Get().DER == second.DER }, 5*time.Second,
		"loop did not swap in a leafless bundle (or panicked doing so)")
	cancel()
	<-done
}

// A cert dated in the future is unusable now but looks like it has years of
// headroom, so judging on NotAfter alone wedged the daemon permanently.
func TestCertUsable_RejectsBothEnds(t *testing.T) {
	now := time.Now()
	mk := func(nb, na time.Duration) *x509.Certificate {
		return genCertDated(t, now.Add(nb), now.Add(na)).TLS.Leaf
	}
	for _, tc := range []struct {
		name string
		leaf *x509.Certificate
		want bool
	}{
		{"fresh", mk(-time.Hour, 10*24*time.Hour), true},
		{"inside the rotation threshold", mk(-time.Hour, certRotateThreshold/2), false},
		{"already expired", mk(-48*time.Hour, -time.Hour), false},
		{"dated in the future (wrong clock at mint time)", mk(365*24*time.Hour, 380*24*time.Hour), false},
		{"nil leaf", nil, false},
	} {
		if got := certUsable(tc.leaf, now); got != tc.want {
			t.Errorf("%s: certUsable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoadOrRotateManaged_ReplacesAFutureDatedCert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	now := time.Now()
	future := genCertDated(t, now.Add(365*24*time.Hour), now.Add(380*24*time.Hour))
	if err := persistCert(future, certPath, keyPath); err != nil {
		t.Fatalf("seed cert: %v", err)
	}

	got, err := loadOrRotateManaged()
	if err != nil {
		t.Fatalf("loadOrRotateManaged: %v", err)
	}
	if got.DER == future.DER {
		t.Fatal("a cert that is not yet valid was reused; every browser rejects it and the loop would never replace it")
	}
	if !certUsable(got.TLS.Leaf, time.Now()) {
		t.Fatal("replacement is not usable right now")
	}
}

// syncBuf lets the test read what the loop's goroutine logged.
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A rotation that keeps failing is only actionable if it says so before the
// cert lapses. One WARN an hour in a busy log is not that.
func TestRotateCertLoop_EscalatesWhenFailureBecomesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		validFor    time.Duration
		wantLevel   string
		unwantLevel string
	}{
		{"far from expiry: warn only", 10 * 24 * time.Hour, "level=WARN", "level=ERROR"},
		{"retrying can no longer save it: escalate", 30 * time.Millisecond, "level=ERROR", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out syncBuf
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))

			holder := newCertHolder(genCertExpiringIn(t, tc.validFor))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				// every = 10ms, so "2*every" is 20ms of headroom.
				rotateCertLoop(ctx, holder, 10*time.Millisecond, func() (CertBundle, error) {
					return CertBundle{}, os.ErrPermission
				})
			}()
			waitFor(t, func() bool { return strings.Contains(out.String(), tc.wantLevel) }, 5*time.Second,
				"expected "+tc.wantLevel+" in the log, got: "+out.String())
			cancel()
			<-done

			if tc.unwantLevel != "" && strings.Contains(out.String(), tc.unwantLevel) {
				t.Fatalf("unexpected %s while the cert still had %v left: %s", tc.unwantLevel, tc.validFor, out.String())
			}
		})
	}
}

// The loop's whole premise is that it re-checks many times inside the window.
// Nothing else enforces that; bump the interval past the threshold and the fix
// silently stops working.
func TestCertRotateInterval_LeavesRoomToAct(t *testing.T) {
	if certRotateInterval >= certRotateThreshold {
		t.Fatalf("interval %v must stay below threshold %v", certRotateInterval, certRotateThreshold)
	}
	if chances := certRotateThreshold / certRotateInterval; chances < 4 {
		t.Fatalf("only %d checks inside the threshold; too few to survive a transient failure", chances)
	}
}

// The hash is no longer fixed for the life of the process, so a cached copy is
// a hash for a cert that is no longer served.
func TestHandleCertHash_ForbidsCaching(t *testing.T) {
	h := handleCertHash(newCertHolder(mustGenCert(t)), func(b CertBundle) [32]byte { return b.DER })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/cert-hash", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-store")
	}
}
