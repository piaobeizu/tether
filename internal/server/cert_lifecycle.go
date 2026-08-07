package server

import (
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

// LoadOrGenCert loads a cert from explicit PEM files (bypassing rotation),
// or falls back to the managed cert store at ~/.tether/{cert,key}.pem.
// On first run or when the stored cert expires within 24h, a new cert is
// generated and persisted (§10.B.2 #5 rotation contract).
func LoadOrGenCert(certFile, keyFile string) (CertBundle, error) {
	if certFile != "" && keyFile != "" {
		bundle, err := loadPEMFiles(certFile, keyFile)
		if err != nil {
			return bundle, err
		}
		bundle.External = true
		return bundle, nil
	}
	return loadOrRotateManaged()
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

// startCertRotation launches rotateCertLoop for a managed cert and reports
// whether it started one.
//
// Split out of Run so the skip rule is reachable from a test — "we forgot to
// start the loop" is otherwise indistinguishable from "the loop is broken".
//
// External bundles are skipped, but for two different reasons and with
// different consequences:
//
//   - --acme-domain: certmagic renews inside its own tls.Config, which the
//     listener uses directly (cfg.acmeTLSBase), bypassing the holder entirely.
//     Genuinely handled — but note what "handled" rests on. Renewal happens
//     while this process holds :443, so certmagic cannot bind the port for its
//     own challenge server and hands the TLS-ALPN-01 challenge back to our
//     listener. That only works because makeTLS in server.go keeps acme-tls/1
//     in the TCP listener's ALPN list. Until tether#79 it did not, and this
//     comment asserted the conclusion anyway: the holder was correctly
//     bypassed, and renewal was correctly reached, and it still failed every
//     time at ALPN negotiation. Deleting that ALPN entry re-breaks renewal
//     without touching a line of this file.
//   - --cert-file/--key-file: NOT handled. The file is read once at startup and
//     never re-read, so an operator renewing it on disk (certbot at day 60)
//     changes nothing until tether restarts. Rotating here is not the answer —
//     it would replace their CA-signed cert with a self-signed one — but this
//     is the same class of bug as tether#72 on the other branch, and the
//     machinery to fix it properly (poll the file's mtime, reload through the
//     holder) is now in place. Tracked separately rather than smuggled in here.
func startCertRotation(ctx context.Context, bundle CertBundle, holder *certHolder, every time.Duration, reload func() (CertBundle, error)) bool {
	if bundle.External {
		return false
	}
	go rotateCertLoop(ctx, holder, every, reload)
	return true
}

// rotateCertLoop keeps holder pointing at a cert that has not expired. It
// returns when ctx is done.
//
// reload is loadOrRotateManaged in production. That function already owns the
// "within certRotateThreshold of expiry => generate and persist a fresh one"
// decision, so the loop deliberately does not re-implement the predicate; it
// applies whatever reload hands back. For the ~13 days where nothing is due,
// reload returns the same cert and the unchanged-DER check below makes the tick
// a no-op — no swap, no log line.
//
// Only managed certs get here. --cert-file/--key-file is operator-owned and
// ACME is renewed by certmagic inside its own tls.Config, so lifecycle.go skips
// both (see CertBundle.External).
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
				// Escalate once retrying can no longer save it. Past that
				// point an operator has to act (full disk, root-owned
				// cert.pem), and one WARN an hour inside a busy log is not a
				// signal — the loop knows both that it is failing and that the
				// cert is nearly gone, so it should say so together.
				if until, ok := leafNotAfter(holder.Get()); ok && time.Until(until) < 2*every {
					slog.Error("cert rotation failing with the current cert about to expire; connections will start failing",
						"err", err,
						"expires_in", time.Until(until).Round(time.Minute),
						"remedy", "check ~/.tether writability and free space")
				} else {
					slog.Warn("cert rotation check failed", "err", err)
				}
				continue
			}
			if next.DER == holder.Get().DER {
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
