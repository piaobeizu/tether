package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const certRotateThreshold = 24 * time.Hour

// LoadOrGenCert loads the operator's cert from explicit PEM files, or falls
// back to the managed cert store at ~/.tether/{cert,key}.pem. On first run or
// when the stored managed cert expires within 24h, a new cert is generated and
// persisted (§10.B.2 #5 rotation contract).
//
// This runs once, on the way in. Keeping the cert current afterwards is
// certRenewalFor's job, and the two have to agree about which source is in
// play — hence the shared externalPEMSource predicate rather than the same
// condition written twice.
func LoadOrGenCert(certFile, keyFile string) (CertBundle, error) {
	if externalPEMSource(certFile, keyFile) {
		return loadExternalPEM(certFile, keyFile)
	}
	return loadOrRotateManaged()
}

// externalPEMSource reports whether the operator supplied their own cert.
//
// One predicate, two decisions: which cert LoadOrGenCert loads at startup, and
// how certRenewalFor keeps it current. Writing the condition out at both sites
// is what would let them drift, and a drift is silent in either direction — a
// reload armed with empty paths that fails every tick, or an operator cert that
// nothing ever re-reads, which is tether#73 itself.
func externalPEMSource(certFile, keyFile string) bool {
	return certFile != "" && keyFile != ""
}

// loadExternalPEM reads the operator's cert/key pair and marks the bundle
// External.
//
// The flag has to be re-applied on every load, not just the first: a reload
// replaces the whole bundle in the holder, and External is what keeps
// /cert-hash answering 404 (see CertBundle.External — advertising
// serverCertificateHashes for a CA-signed cert makes Chrome drop the
// connection with QUIC_NETWORK_IDLE_TIMEOUT).
//
// The error names both paths. crypto/tls only includes a filename when the
// failure was the open itself: a file that parses as no key, or a key that does
// not match the cert — which is exactly what a half-finished renewal looks
// like — produces "tls: failed to find any PEM data in key input" and
// "tls: private key does not match public key", neither of which says where
// (crypto/tls X509KeyPair, read against Go 1.26.3 sources on 2026-08-07;
// LoadX509KeyPair forwards a named *PathError only from its two ReadFiles).
// The loop logs err and nothing else about the source, so it has to be here.
func loadExternalPEM(certFile, keyFile string) (CertBundle, error) {
	b, err := loadPEMFiles(certFile, keyFile)
	if err != nil {
		return CertBundle{}, fmt.Errorf("load cert %s with key %s: %w", certFile, keyFile, err)
	}
	b.External = true
	return b, nil
}

func loadOrRotateManaged() (CertBundle, error) {
	dir, err := tetherDataDir()
	if err != nil {
		return CertBundle{}, err
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if bundle, err := loadPEMFiles(certPath, keyPath); err == nil {
		if certUsable(bundle.TLS.Leaf, time.Now()) {
			return bundle, nil
		}
	}

	bundle, err := GenerateCert()
	if err != nil {
		return CertBundle{}, fmt.Errorf("generate cert: %w", err)
	}
	if err := persistCert(bundle, certPath, keyPath); err != nil {
		return CertBundle{}, fmt.Errorf("persist cert: %w", err)
	}
	return bundle, nil
}

// certUsable reports whether a stored cert can keep serving for a while yet.
//
// NotBefore matters as much as NotAfter. A host that boots with a badly wrong
// clock — snapshot restore, dead RTC — mints and persists a cert dated years
// ahead; once NTP corrects the clock, every browser rejects it while
// time.Until(NotAfter) still reports years of headroom. Judging on NotAfter
// alone therefore left that cert in place forever, with no log line and with
// `tether doctor` reporting it healthy: a permanent outage with no signal.
//
// now is a parameter so the both-ends check is testable without a fake clock.
func certUsable(leaf *x509.Certificate, now time.Time) bool {
	if leaf == nil {
		return false
	}
	if now.Before(leaf.NotBefore) {
		return false
	}
	return leaf.NotAfter.Sub(now) >= certRotateThreshold
}

// leafNotAfter reports a bundle's expiry, or false when the leaf is missing.
// Callers use it instead of reaching for Leaf.NotAfter directly so that a nil
// leaf degrades a log line rather than panicking a background goroutine.
func leafNotAfter(b CertBundle) (time.Time, bool) {
	if b.TLS.Leaf == nil {
		return time.Time{}, false
	}
	return b.TLS.Leaf.NotAfter, true
}

// certHolder is the live certificate, readable from any goroutine.
//
// A managed cert lives 14 days and the browser pins its hash
// (serverCertificateHashes), so a rotation has to become visible in two places
// at once: the TLS handshake and /cert-hash. Neither used to be able to see one
// — the listener held a static Certificates slice and the hash endpoints closed
// over a string, both captured at startup. Rewriting the PEM files on disk
// would therefore have changed nothing that a client can observe. Both now read
// through this holder instead, which is what makes rotateCertLoop mean anything.
type certHolder struct {
	v atomic.Pointer[CertBundle]
}

func newCertHolder(b CertBundle) *certHolder {
	h := &certHolder{}
	h.Set(b)
	return h
}

// Get returns the current bundle. The zero value is never stored by
// newCertHolder, so callers get a usable cert from the first handshake on.
func (h *certHolder) Get() CertBundle {
	if b := h.v.Load(); b != nil {
		return *b
	}
	return CertBundle{}
}

func (h *certHolder) Set(b CertBundle) { h.v.Store(&b) }

// certTLSConfig builds the server TLS config for the managed-cert path.
//
// Certificates is deliberately left empty: the cert is resolved per handshake
// so a rotation reaches live traffic without rebinding the listener. Filling
// the slice instead — which is what this replaced — pins the process to
// whatever was on disk at startup, which is precisely how a 14-day cert lapses
// under a daemon that is never restarted.
func certTLSConfig(certs *certHolder, protos []string) *tls.Config {
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			b := certs.Get()
			if len(b.TLS.Certificate) == 0 {
				return nil, fmt.Errorf("no certificate available")
			}
			return &b.TLS, nil
		},
		NextProtos: protos,
	}
}

// certRotateInterval is how often rotateCertLoop re-checks. It must stay well
// below certRotateThreshold — that relationship is what gives the loop many
// chances to act instead of one, and TestCertRotateInterval_LeavesRoomToAct
// pins it.
//
// Polling on a fixed interval rather than arming a timer for NotAfter-24h is
// deliberate: a 13-day timer is a single point of failure across a wall-clock
// correction, whereas re-deciding from the cert's own dates every hour cannot
// drift. Note what polling does NOT buy: Go tickers run on CLOCK_MONOTONIC,
// which does not advance while the host is suspended, so a machine that sleeps
// through the threshold wakes up and keeps serving the stale cert for up to one
// awake interval. The interval is what bounds that exposure, which is the other
// reason to keep it short.
const certRotateInterval = time.Hour

// certReloadInterval is how often the operator-owned cert path re-reads its
// files. It is shorter than certRotateInterval deliberately: the managed loop
// can work out from the cert's own dates when it will have something to do,
// whereas nothing in this process knows when an operator — or their certbot
// timer — replaces a file, so the interval is the only bound on how long a
// renewed cert sits on disk unserved. Two small file reads a minute is not a
// cost worth weighing against that.
//
// The price is noise: a --cert-file path the operator got wrong now produces a
// WARN a minute instead of one an hour. That is the right direction for a bug
// whose entire symptom was silence — nothing in this process can mint a
// replacement for someone else's cert, so saying so is the only thing it can
// do — but it is why the ERROR escalation in rotateCertLoop is keyed to how
// close the cert is to lapsing rather than to this interval.
const certReloadInterval = time.Minute

// certRenewal is how a running process keeps its certificate current: what to
// call, how often, and whether that call can mint a new self-signed cert.
//
// mints exists for one check: rotation must never run over a cert this process
// did not create (the first constraint in tether#73). Before the operator-file
// path had a renewal at all, "external ⇒ no loop" enforced that structurally;
// now that two of the three paths do get a loop, the guarantee has to be stated
// rather than implied. See startCertRotation.
type certRenewal struct {
	reload func() (CertBundle, error)
	every  time.Duration
	mints  bool
}

// certRenewalFor picks the renewal for cfg's cert path. Three paths, three
// answers:
//
//   - managed (~/.tether): loadOrRotateManaged, which re-mints inside
//     certRotateThreshold. A 14-day cert under a daemon meant to run for weeks
//     is what tether#72 was about.
//   - --cert-file/--key-file: re-read the operator's files, never mint. Their
//     renewal rewrites the path at a moment this process cannot predict
//     (certbot: "During the renewal, /etc/letsencrypt/live is updated with the
//     latest necessary files" — eff-certbot.readthedocs.io/en/stable/using.html,
//     read 2026-08-07), and until tether#73 nothing re-read it: the daemon kept
//     presenting day-0 bytes until someone restarted it, then failed every TCP
//     and QUIC handshake once those bytes expired, with nothing in the log.
//     Reload re-opens the path by name each tick, so a file rewritten in place
//     and a path swapped underneath us both land. Minting here instead would be
//     worse than the bug — it would replace a CA-signed cert with a self-signed
//     one.
//   - --acme-domain: nothing. certmagic renews inside its own tls.Config, which
//     the listener uses directly (cfg.acmeTLSBase), bypassing the holder
//     entirely. Genuinely handled — but note what "handled" rests on. Renewal
//     happens while this process holds :443, so certmagic cannot bind the port
//     for its own challenge server and hands the TLS-ALPN-01 challenge back to
//     our listener. That only works because makeTLS in server.go keeps
//     acme-tls/1 in the TCP listener's ALPN list. Until tether#79 it did not,
//     and this comment asserted the conclusion anyway: the holder was correctly
//     bypassed, and renewal was correctly reached, and it still failed every
//     time at ALPN negotiation. Deleting that ALPN entry re-breaks renewal
//     without touching a line of this file.
//
// ACME is tested first because Run is ordered that way: Step 4b replaces the
// bundle with certmagic's when --acme-domain is set even if --cert-file was
// also passed, so on that combination the holder is not on the serving path at
// all and re-reading the files would keep a cert current that nobody presents.
func certRenewalFor(cfg *Config) certRenewal {
	switch {
	case cfg.AcmeDomain != "":
		return certRenewal{}
	case externalPEMSource(cfg.CertFile, cfg.KeyFile):
		// Copy the paths instead of closing over cfg. Not a fix for a live
		// race — every write Run makes to cfg happens before it starts this
		// loop, and none of them touch these two fields. It is that this
		// closure outlives the call, while Config belongs to the caller: Run is
		// exported and takes a *Config an embedder still holds, and nothing
		// documents it as frozen once Run is under way. Reading the paths once,
		// here, is what makes "which files does the loop watch" answerable from
		// this function alone.
		certFile, keyFile := cfg.CertFile, cfg.KeyFile
		return certRenewal{
			reload: func() (CertBundle, error) { return loadExternalPEM(certFile, keyFile) },
			every:  certReloadInterval,
		}
	default:
		return certRenewal{reload: loadOrRotateManaged, every: certRotateInterval, mints: true}
	}
}

// start launches r's loop and reports whether it started one.
//
// Separate from startCertRotation so a test can shrink every before starting:
// the production intervals are a minute and an hour, so a test forced to go
// through startCertRotation could not observe a single tick, and "we forgot to
// start the loop" would again be indistinguishable from "the loop is broken".
func (r certRenewal) start(ctx context.Context, holder *certHolder) bool {
	if r.reload == nil {
		return false
	}
	go rotateCertLoop(ctx, holder, r.every, r.reload)
	return true
}

// startCertRotation keeps holder's certificate current for whichever cert path
// cfg selects, and reports whether it started a loop. This is the one line in
// Run that decides whether the cert is maintained at all; certRenewalFor is
// where the per-path decisions live.
//
// bundle is here only for the refusal below. Deciding from cfg is what lets the
// decision be a pure function of the flags, but it also means the code no
// longer *cannot* pick the minting reload for a cert it did not create — it
// merely does not. A future third source of External=true (a keystore, a
// SPIFFE bundle) would land in certRenewalFor's default branch and, if this
// check were absent, quietly overwrite a CA-signed deployment with a
// self-signed cert. Unreachable today: only loadExternalPEM sets External, and
// externalPEMSource is the same predicate that routes it here. It refuses
// loudly rather than silently because at that point the process has a cert it
// cannot maintain, which an operator needs to know.
func startCertRotation(ctx context.Context, cfg *Config, bundle CertBundle, holder *certHolder) bool {
	r := certRenewalFor(cfg)
	if r.mints && bundle.External {
		slog.Error("refusing to rotate a certificate this process did not issue; it will not be renewed here",
			"remedy", "pass --cert-file/--key-file (re-read in place) or --acme-domain (renewed by certmagic)")
		return false
	}
	return r.start(ctx, holder)
}

// logCertMode reports at startup which certificate this process is serving and
// what, if anything, will keep it current.
//
// Split out of Run so it can be asserted: Run binds ports and spawns MCP, so
// anything left inside it is verified by reading. The operator-file line is the
// one that had to exist — that path logged nothing at all before tether#73, so
// a daemon that re-reads the file and one that never will looked identical
// until the cert lapsed weeks later.
//
// Ordered like certRenewalFor, and for the same reason: Run's Step 4b replaces
// the bundle with certmagic's when --acme-domain is set, even if --cert-file
// was also passed, so ACME is then what is actually being served.
func logCertMode(cfg *Config, bundle CertBundle) {
	switch {
	case cfg.AcmeDomain != "":
		slog.Info("cert mode", "acme", cfg.AcmeDomain)
	case bundle.External:
		attrs := []any{"source", "operator files", "cert_file", cfg.CertFile, "reload_every", certReloadInterval}
		if until, ok := leafNotAfter(bundle); ok {
			attrs = append(attrs, "not_after", until.UTC().Format(time.RFC3339))
		}
		slog.Info("cert mode", attrs...)
	default:
		slog.Info("cert DER hash", "hash", HashHex(bundle.DER))
	}
}

// rotateCertLoop keeps holder pointing at a cert that has not expired. It
// returns when ctx is done.
//
// reload comes from certRenewalFor: loadOrRotateManaged for the managed cert,
// a re-read of the operator's PEM files for --cert-file/--key-file. Each owns
// its own "is there anything to do?" decision — the managed one re-mints inside
// certRotateThreshold, the external one reports whatever is on disk — so the
// loop deliberately does not re-implement a predicate; it applies whatever
// reload hands back. When nothing has changed, sameChain below makes the tick a
// no-op: no swap, no log line. That is ~13 days of ticks for a managed cert and
// most of a certificate's life for an operator's.
//
// The external path swaps in whatever parses, including a cert that is already
// expired or not yet valid. Second-guessing that would mean deciding an
// operator's own file is wrong when the likelier reading is that this host's
// clock is; a bundle that fails to parse at all is a different matter and keeps
// the current cert (below).
//
// ACME never gets here: certmagic renews inside its own tls.Config, which the
// listener uses directly (see certRenewalFor).
func rotateCertLoop(ctx context.Context, holder *certHolder, every time.Duration, reload func() (CertBundle, error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			next, err := reload()
			if err != nil {
				// Keep serving the current cert and retry on the next tick: a
				// cert about to expire still beats tearing down a working
				// listener over a transient disk error.
				//
				// Escalate once the cert this loop is failing to replace is
				// itself about to lapse. Past that point an operator has to act
				// (full disk, root-owned cert.pem, a --cert-file path that
				// moved), and one WARN an hour inside a busy log is not a
				// signal — the loop knows both that it is failing and that the
				// cert is nearly gone, so it should say so together.
				//
				// The window is certRotateThreshold and not 2*every, which is
				// what it used to be. "Two more chances" only measures anything
				// when a retry could still fix it, which is the managed path;
				// for an operator's cert nothing here can ever fix it, and at
				// certReloadInterval the old rule bought two minutes of notice
				// for an outage that lasts until someone intervenes. The
				// threshold is the same one that defines "about to expire"
				// everywhere else in this file, and it does not move when the
				// interval does.
				//
				// The remedy line stays source-agnostic because this loop now
				// serves ~/.tether and an operator's own path; which one failed
				// is in err, whose file names loadExternalPEM adds — the error
				// from crypto/tls does not always carry them (a PEM that parses
				// as no key, or a key that does not match the cert, names
				// neither file).
				if until, ok := leafNotAfter(holder.Get()); ok && time.Until(until) < certRotateThreshold {
					slog.Error("cert rotation failing with the current cert about to expire; connections will start failing",
						"err", err,
						"expires_in", time.Until(until).Round(time.Minute),
						"remedy", "check that the cert and key files are readable and the filesystem has space")
				} else {
					slog.Warn("cert rotation check failed", "err", err)
				}
				continue
			}
			if sameChain(next, holder.Get()) {
				continue
			}
			holder.Set(next)
			attrs := []any{"hash", HashHex(next.DER)}
			if until, ok := leafNotAfter(next); ok {
				attrs = append(attrs, "not_after", until.UTC().Format(time.RFC3339))
			}
			slog.Info("cert rotated", attrs...)
		}
	}
}

// sameChain reports whether two bundles would put the same bytes on the wire.
//
// Not a DER comparison: CertBundle.DER hashes Certificate[0] alone, because
// that is what /cert-hash advertises and what the W3C pins. The handshake
// presents the whole chain, so judging "has anything changed?" on the leaf
// makes one class of renewal invisible — an operator repairing a fullchain.pem
// whose intermediate is wrong or expired, without reissuing the leaf, would
// have watched the loop skip every tick. That is the same silent no-op this
// file exists to remove, and it is specific to the operator path: a managed
// bundle is always a single self-signed cert, for which this is exactly the
// DER comparison it replaces.
func sameChain(a, b CertBundle) bool {
	if len(a.TLS.Certificate) != len(b.TLS.Certificate) {
		return false
	}
	for i := range a.TLS.Certificate {
		if !bytes.Equal(a.TLS.Certificate[i], b.TLS.Certificate[i]) {
			return false
		}
	}
	return true
}

func loadPEMFiles(certFile, keyFile string) (CertBundle, error) {
	tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return CertBundle{}, err
	}
	if tlsCert.Leaf == nil {
		tlsCert.Leaf, err = x509.ParseCertificate(tlsCert.Certificate[0])
		if err != nil {
			return CertBundle{}, fmt.Errorf("parse leaf: %w", err)
		}
	}
	der := tlsCert.Certificate[0]
	return CertBundle{
		TLS:  tlsCert,
		DER:  sha256Sum(der),
		SPKI: sha256Sum(tlsCert.Leaf.RawSubjectPublicKeyInfo),
	}, nil
}

func persistCert(b CertBundle, certPath, keyPath string) error {
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: b.TLS.Certificate[0],
	})
	keyPEM, err := marshalECKey(b.TLS.PrivateKey)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	if err := atomicWrite(certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func tetherDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	return dir, nil
}
