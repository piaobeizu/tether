package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mholt/acmez/v3"

	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
)

// The bug these cover (tether#79): with --acme-domain, newServer cloned
// certmagic's tls.Config and REPLACED its NextProtos, dropping acme-tls/1.
//
// What makes that bug hard to see is that it cannot fail at startup. The first
// issuance happens in lifecycle.go Step 4b, before any listener binds, and
// certmagic's own solver binds :443 for it — so a daemon with the wrong ALPN
// list starts, serves, and looks healthy. The list is only consulted ~30 days
// before expiry, when certmagic tries to renew, finds :443 already held by this
// process, and (certmagic solvers.go robustTryListen) silently hands the
// challenge back to us on the assumption that whoever holds the socket can
// answer it. From there the CA's handshake dies at ALPN negotiation with
// `tls: no application protocol`, renewal fails on every retry, and the
// symptom surfaces at day 90 as an expired cert.
//
// So these tests carry the whole guarantee: there is no startup failure, no log
// line, and nothing else in the tree that would notice. Two things have to hold
// together, and asserting only the first is what the original version of this
// file did — offering acme-tls/1 while serving a certificate that did not come
// from certmagic reads as a clean pass and still fails validation at the CA:
//
//  1. the TCP listener OFFERS acme-tls/1, and
//  2. the certificate it serves comes from certmagic's config, not the
//     self-signed holder.

// acmeBase mirrors the shape of what certmagic.TLS() returns: a config whose
// certificate comes from GetCertificate and whose NextProtos holds only the
// challenge protocol (certmagic config.go TLSConfig). It also returns the
// SHA-256 of the cert it will serve, so a test can tell that certificate apart
// from the holder's.
//
// GetCertificate is a stub rather than certmagic's, deliberately. certmagic
// serves the challenge cert only when ClientHello.SupportedProtos is exactly
// [acme-tls/1] (certmagic handshake.go), but the challenge memory it reads has
// no exported writer, so no external package can arm one. That lookup is the
// library's to get right; what tether owns is which config the listener draws
// its certificate and ALPN list from.
func acmeBase(t *testing.T) (*tls.Config, [32]byte) {
	t.Helper()
	b := mustGenCert(t)
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &b.TLS, nil },
		NextProtos:     []string{acmez.ACMETLS1Protocol},
	}, b.DER
}

// newALPNTestServer builds a Server through the real constructor. base nil
// selects the self-signed path, non-nil the ACME path — the same branch
// production takes on --acme-domain. The holder is always seeded with a
// DIFFERENT cert, so "served the holder's cert" and "served certmagic's cert"
// are distinguishable.
func newALPNTestServer(t *testing.T, base *tls.Config) *Server {
	t.Helper()
	cfg := &Config{
		Port:         0,
		Registry:     session.NewRegistry(),
		MCPLifecycle: mcplifecycle.New(),
		acmeTLSBase:  base,
	}
	return newServer(cfg, newCertHolder(mustGenCert(t)), nil, nil, nil, nil, nil)
}

func TestNewServer_ACMEListenersOfferExactlyTheRightALPN(t *testing.T) {
	base, _ := acmeBase(t)
	srv := newALPNTestServer(t, base)

	// Appended, not prepended. Go picks the first *server* protocol the client
	// also offers (crypto/tls negotiateALPN) and RFC 8737 requires a CA to
	// offer acme-tls/1 alone, so position cannot change any real negotiation;
	// prepending is an equivalent mutation. The order is pinned only to keep
	// the pre-fix list a prefix of this one.
	wantTCP := []string{"h2", "http/1.1", acmez.ACMETLS1Protocol}
	if got := srv.tcp.TLSConfig.NextProtos; !slices.Equal(got, wantTCP) {
		t.Fatalf("TCP ALPN = %q, want %q", got, wantTCP)
	}

	// The UDP listener must NOT carry it. TLS-ALPN-01 is defined over TCP
	// (RFC 8737 §3: "This connection MUST use TCP port 443"); a CA never
	// validates over QUIC, so offering it here would be dead configuration
	// that reads as a second solving path.
	wantH3 := []string{"h3"}
	if got := srv.h3.TLSConfig.NextProtos; !slices.Equal(got, wantH3) {
		t.Fatalf("UDP/h3 ALPN = %q, want %q", got, wantH3)
	}
}

func TestNewServer_SelfSignedListenersNeverOfferTheChallengeProtocol(t *testing.T) {
	srv := newALPNTestServer(t, nil)

	// Asserted as exact lists rather than "does not contain acme-tls/1": the
	// weaker form also passes when the protocols are dropped altogether, and
	// on the UDP side nothing else would notice. h3.TLSConfig is what QUIC
	// actually gets — webtransport-go hands it straight to quic.ListenEarly,
	// so unlike the TCP side there is no net/http pass to repair a bad list.
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{"TCP", srv.tcp.TLSConfig.NextProtos, []string{"h2", "http/1.1"}},
		{"UDP/h3", srv.h3.TLSConfig.NextProtos, []string{"h3"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Fatalf("%s ALPN without ACME = %q, want %q", tc.name, tc.got, tc.want)
		}
		if slices.Contains(tc.got, acmez.ACMETLS1Protocol) {
			t.Fatalf("%s advertises %q without ACME — nothing can answer a challenge on this path, "+
				"so offering it only invites handshakes we will fail", tc.name, acmez.ACMETLS1Protocol)
		}
	}
}

// serveTLS runs the server through http.Server.ServeTLS — the same entry point
// lifecycle.go uses (ListenAndServeTLS is ServeTLS plus a Listen) — and returns
// its address.
//
// Inspecting srv.tcp.TLSConfig is not sufficient on its own: ServeTLS clones
// the config and rewrites NextProtos through adjustNextProtos, which both
// deletes ("h2"/"http/1.1" when that protocol is disabled on the Server) and
// appends (either one, when enabled but missing). It leaves protocols it does
// not recognise alone, which is the only reason acme-tls/1 survives to the
// wire — a property of net/http, not of tether, and one the field assertions
// above would keep passing without.
func serveTLS(t *testing.T, srv *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.tcp.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.tcp.Close() })
	return ln.Addr().String()
}

// negotiate performs a real TLS handshake offering clientProtos and returns the
// resulting connection state — the negotiated protocol AND the certificate the
// server actually presented. InsecureSkipVerify because every cert here is
// self-signed; the chain is not the subject, its identity is.
func negotiate(t *testing.T, addr string, clientProtos []string) (tls.ConnectionState, error) {
	t.Helper()
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "tether.test",
		NextProtos:         clientProtos,
	})
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer c.Close()
	return c.ConnectionState(), nil
}

func servedCert(t *testing.T, cs tls.ConnectionState) [32]byte {
	t.Helper()
	if len(cs.PeerCertificates) == 0 {
		t.Fatal("server presented no certificate")
	}
	return sha256.Sum256(cs.PeerCertificates[0].Raw)
}

// The renewal handshake, for real: this is the exact exchange that failed
// against the pre-fix build, reported by Let's Encrypt as
// `remote error: tls: no application protocol`.
func TestServeTLS_ACMEListenerNegotiatesTheChallengeProtocol(t *testing.T) {
	base, wantDER := acmeBase(t)
	addr := serveTLS(t, newALPNTestServer(t, base))

	cs, err := negotiate(t, addr, []string{acmez.ACMETLS1Protocol})
	if err != nil {
		t.Fatalf("a CA validating this domain would see: %v", err)
	}
	if cs.NegotiatedProtocol != acmez.ACMETLS1Protocol {
		t.Fatalf("negotiated %q, want %q — renewal fails for the life of the daemon",
			cs.NegotiatedProtocol, acmez.ACMETLS1Protocol)
	}
	// Offering the protocol is only half of it. If the listener answers with
	// the self-signed holder's cert instead of certmagic's, the CA rejects the
	// challenge as an incorrect validation certificate — same silent, 60-day
	// failure, reached by a different route.
	if got := servedCert(t, cs); got != wantDER {
		t.Fatalf("the ACME listener served a certificate that did not come from acmeTLSBase; "+
			"a CA would reject this challenge (got %x, want %x)", got[:8], wantDER[:8])
	}
}

func TestServeTLS_ACMEListenerStillNegotiatesH2ForBrowsers(t *testing.T) {
	base, wantDER := acmeBase(t)
	addr := serveTLS(t, newALPNTestServer(t, base))

	cs, err := negotiate(t, addr, []string{"h2", "http/1.1"})
	if err != nil {
		t.Fatalf("browser handshake: %v", err)
	}
	if cs.NegotiatedProtocol != "h2" {
		t.Fatalf("negotiated %q, want h2 — making renewal work must not cost the browsers",
			cs.NegotiatedProtocol)
	}
	if got := servedCert(t, cs); got != wantDER {
		t.Fatalf("browsers were served a certificate that did not come from acmeTLSBase "+
			"(got %x, want %x) — --acme-domain would silently fall back to the self-signed cert",
			got[:8], wantDER[:8])
	}
}

// The negative control. Without it the two tests above cannot be told apart
// from a listener that accepts every protocol offered: this pins that the
// self-signed path still refuses, and that the refusal looks exactly like the
// failure tether#79 started from.
func TestServeTLS_SelfSignedListenerRejectsTheChallengeProtocol(t *testing.T) {
	addr := serveTLS(t, newALPNTestServer(t, nil))

	cs, err := negotiate(t, addr, []string{acmez.ACMETLS1Protocol})
	if err == nil {
		t.Fatalf("handshake succeeded with %q on the self-signed path; the assertions above prove nothing",
			cs.NegotiatedProtocol)
	}
	if !strings.Contains(err.Error(), "no application protocol") {
		t.Fatalf("rejected with %v, want a `no application protocol` alert — "+
			"if the shape of this failure changed, so did what the CA sees", err)
	}
}

// --- the wiring hop -------------------------------------------------------
//
// Everything above tests newServer given an acmeTLSBase. These test that Run
// ever puts one there. That step has no observable failure of its own: drop the
// assignment and --acme-domain quietly serves the self-signed cert instead,
// with the same logs and the same exit code.

func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	var out syncBuf
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &out
}

func TestApplyACME_WiresCertmagicsConfigIntoTheListener(t *testing.T) {
	captureLogs(t)

	want, _ := acmeBase(t)
	wantBundle := CertBundle{External: true, DER: [32]byte{7}}
	cfg := &Config{Port: acmeChallengePort, AcmeDomain: "tether.test", AcmeEmail: "ops@example.com"}

	var gotDomain, gotEmail string
	got, err := applyACME(context.Background(), cfg,
		func(_ context.Context, domain, email string) (*tls.Config, CertBundle, error) {
			gotDomain, gotEmail = domain, email
			return want, wantBundle, nil
		})
	if err != nil {
		t.Fatalf("applyACME: %v", err)
	}

	// Domain and email are both strings, so transposing them compiles.
	if gotDomain != cfg.AcmeDomain || gotEmail != cfg.AcmeEmail {
		t.Fatalf("SetupACME called with (%q, %q), want (%q, %q)",
			gotDomain, gotEmail, cfg.AcmeDomain, cfg.AcmeEmail)
	}
	if cfg.acmeTLSBase != want {
		t.Fatal("cfg.acmeTLSBase is not the config certmagic returned; " +
			"the listener would serve the self-signed cert while --acme-domain claims otherwise")
	}
	// CertBundle embeds a tls.Certificate and is not comparable; DER and
	// External are the two fields Run acts on.
	if got.DER != wantBundle.DER || got.External != wantBundle.External {
		t.Fatalf("returned bundle = {DER:%x External:%v}, want {DER:%x External:%v} — External=false "+
			"would re-arm cert rotation and start advertising /cert-hash for a CA cert",
			got.DER[:8], got.External, wantBundle.DER[:8], wantBundle.External)
	}
}

func TestApplyACME_LeavesNothingWiredWhenIssuanceFails(t *testing.T) {
	captureLogs(t)

	cfg := &Config{Port: acmeChallengePort, AcmeDomain: "tether.test"}
	boom := errors.New("challenge failed")

	// The fake returns a NON-nil config alongside the error on purpose.
	// SetupACME happens to return nil there today, which makes "assign, then
	// check err" and "check err, then assign" indistinguishable — a mutation
	// that reorders them survived until this input existed. Run turns the
	// error into a startup failure either way, so the ordering only shows up
	// if SetupACME ever starts returning a partially-built config.
	stillborn, _ := acmeBase(t)

	if _, err := applyACME(context.Background(), cfg,
		func(context.Context, string, string) (*tls.Config, CertBundle, error) {
			return stillborn, CertBundle{External: true}, boom
		}); !errors.Is(err, boom) {
		t.Fatalf("applyACME error = %v, want %v — Run turns this into a startup failure", err, boom)
	}
	if cfg.acmeTLSBase != nil {
		t.Fatal("a failed issuance still wired a base config into the listener")
	}
}

func TestWarnACMEPortMismatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		port     int
		wantWarn bool
	}{
		// The CA connects to :443 by fiat (RFC 8737), so on any other port
		// this daemon is not on the challenge path at all and its ALPN list is
		// inert. Saying nothing is what produced the wrong diagnosis this wi
		// started from.
		{"non-standard port: the challenge will not reach us", 8898, true},
		{"443: we are the one the CA reaches", acmeChallengePort, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLogs(t)
			warnACMEPortMismatch("tether.test", tc.port)
			if logged := strings.Contains(out.String(), "level=WARN"); logged != tc.wantWarn {
				t.Fatalf("WARN in log = %v, want %v; log was: %s", logged, tc.wantWarn, out.String())
			}
		})
	}
}

func TestApplyACME_WarnsWhenTheChallengeCannotReachThisDaemon(t *testing.T) {
	base, _ := acmeBase(t)
	for _, tc := range []struct {
		name     string
		port     int
		wantWarn bool
	}{
		{"non-standard port", 8898, true},
		{"443", acmeChallengePort, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLogs(t)
			cfg := &Config{Port: tc.port, AcmeDomain: "tether.test"}
			if _, err := applyACME(context.Background(), cfg,
				func(context.Context, string, string) (*tls.Config, CertBundle, error) {
					return base, CertBundle{External: true}, nil
				}); err != nil {
				t.Fatalf("applyACME: %v", err)
			}
			if logged := strings.Contains(out.String(), "level=WARN"); logged != tc.wantWarn {
				t.Fatalf("WARN in log = %v, want %v; the check exists but nothing calls it. Log: %s",
					logged, tc.wantWarn, out.String())
			}
		})
	}
}

// --acme-domain silently wins over --cert-file: Step 4b replaces the bundle and
// the listener takes its config from certmagic, so the files are neither served
// nor re-read. Since tether#73 the flag help promises they ARE re-read every
// minute — true of the flag on its own, false of this combination — so the
// contradiction has to be said out loud rather than left for the operator to
// infer from a cert that never changes.
func TestApplyACME_WarnsThatCertFilesAreIgnored(t *testing.T) {
	base, _ := acmeBase(t)
	certPath, keyPath := seedOperatorPEM(t, mustGenCert(t))
	for _, tc := range []struct {
		name             string
		cert, key        string
		wantCertFileWarn bool
	}{
		{"both cert flags", certPath, keyPath, true},
		{"cert flag alone is not an operator cert", certPath, "", false},
		{"no cert flags", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLogs(t)
			// Port 443 so the unrelated challenge-port warning stays quiet and
			// this assertion cannot pass on the wrong line.
			cfg := &Config{Port: acmeChallengePort, AcmeDomain: "tether.test", CertFile: tc.cert, KeyFile: tc.key}
			if _, err := applyACME(context.Background(), cfg,
				func(context.Context, string, string) (*tls.Config, CertBundle, error) {
					return base, CertBundle{External: true}, nil
				}); err != nil {
				t.Fatalf("applyACME: %v", err)
			}
			logged := strings.Contains(out.String(), "overrides --cert-file")
			if logged != tc.wantCertFileWarn {
				t.Fatalf("cert-file override warning = %v, want %v. Log: %s", logged, tc.wantCertFileWarn, out.String())
			}
		})
	}
}
