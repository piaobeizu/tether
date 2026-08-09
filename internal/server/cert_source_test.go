package server

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// What these cover (tether#84): `tether doctor` read ~/.tether/cert.pem no
// matter which of the three cert paths a deployment used, so it called a
// healthy --cert-file host broken and read a file an ACME host does not serve.
// The check now asks this package where the cert is, which only helps if the
// answer tracks what the loader actually does — hence tests that tie
// LocateCert to loadOrRotateManaged and to certRenewalFor rather than to a
// literal path.

func TestCertSourceFor_PicksThePathTheLoaderWouldTake(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want CertSource
	}{
		{"no flags is the managed store", Config{}, CertSourceManaged},
		{
			"both PEM flags select the operator's files",
			Config{CertFile: "/c.pem", KeyFile: "/k.pem"},
			CertSourceOperatorFiles,
		},
		{
			// externalPEMSource needs both, so LoadOrGenCert silently serves
			// the managed cert here. A doctor that reported "operator files"
			// would name a cert nobody presents.
			"--cert-file alone falls back to managed",
			Config{CertFile: "/c.pem"},
			CertSourceManaged,
		},
		{"--key-file alone falls back to managed", Config{KeyFile: "/k.pem"}, CertSourceManaged},
		{"--acme-domain selects acme", Config{AcmeDomain: "example.com"}, CertSourceACME},
		{
			// Run's Step 4b overwrites the bundle with certmagic's, so the
			// operator's files stop being served or re-read.
			"--acme-domain wins over the PEM flags",
			Config{AcmeDomain: "example.com", CertFile: "/c.pem", KeyFile: "/k.pem"},
			CertSourceACME,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CertSourceFor(&tc.cfg); got != tc.want {
				t.Errorf("CertSourceFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// The renewal and the location are two answers to one question — which cert
// path is in play — and this pins them to the same answer.
//
// Note what it does not catch: writing the conditions out again inside
// certRenewalFor instead of calling CertSourceFor passes this, because a
// faithful copy agrees. It fails on the tick after the copy stops being
// faithful, which is the only moment at which the duplication has cost
// anything — a doctor that names a cert the loader does not serve.
func TestCertRenewalFor_MatchesCertSourceFor(t *testing.T) {
	cases := []Config{
		{},
		{CertFile: "/c.pem", KeyFile: "/k.pem"},
		{CertFile: "/c.pem"},
		{AcmeDomain: "example.com"},
		{AcmeDomain: "example.com", CertFile: "/c.pem", KeyFile: "/k.pem"},
	}
	for _, cfg := range cases {
		src := CertSourceFor(&cfg)
		r := certRenewalFor(&cfg)
		if got, want := r.reload == nil, src == CertSourceACME; got != want {
			t.Errorf("%+v: no renewal loop = %v, want %v (source %v)", cfg, got, want, src)
		}
		if got, want := r.mints, src == CertSourceManaged; got != want {
			t.Errorf("%+v: mints = %v, want %v (source %v)", cfg, got, want, src)
		}
	}
}

// Asserted against the file loadOrRotateManaged really writes, not against a
// second copy of "cert.pem": a rename of the managed store that missed
// LocateCert would otherwise leave doctor reporting on a path that no longer
// exists, and reporting it as an absence rather than as an error.
func TestLocateCert_ManagedNamesTheFileTheManagedLoaderPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := loadOrRotateManaged(); err != nil {
		t.Fatalf("loadOrRotateManaged: %v", err)
	}

	loc, err := LocateCert(&Config{})
	if err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if loc.Source != CertSourceManaged {
		t.Errorf("source = %v, want managed", loc.Source)
	}
	if _, err := os.Stat(loc.Path); err != nil {
		t.Errorf("LocateCert pointed at %s, which loadOrRotateManaged did not write: %v", loc.Path, err)
	}
}

func TestLocateCert_ManagedNamesThePathEvenWithNoFileThere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	loc, err := LocateCert(&Config{})
	if err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if loc.Path == "" {
		t.Fatal("managed path is empty; a caller cannot tell the operator which file is missing")
	}
}

// LocateCert is called by a diagnostic, so it must not create the directory it
// reports on — otherwise doctor's "~/.tether does not exist" finding survives
// exactly one run.
func TestLocateCert_DoesNotCreateTheDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := LocateCert(&Config{}); err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".tether")); !os.IsNotExist(err) {
		t.Errorf("~/.tether exists after LocateCert (err=%v); the diagnostic created its own subject", err)
	}
}

func TestLocateCert_OperatorFilesNameTheOperatorsCert(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{CertFile: "/etc/letsencrypt/live/h/fullchain.pem", KeyFile: "/etc/letsencrypt/live/h/privkey.pem"}
	loc, err := LocateCert(cfg)
	if err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if loc.Source != CertSourceOperatorFiles || loc.Path != cfg.CertFile {
		t.Errorf("LocateCert = %v %q, want operator files at %q", loc.Source, loc.Path, cfg.CertFile)
	}
}

// seedACMEStore writes data into certmagic's on-disk layout by hand.
//
// The path is spelled out rather than built with certmagic.StorageKeys, which
// is what acmeStoredCert uses: sharing the builder would make this test pass
// for any layout at all, including a wrong one. These literals come from
// certmagic v0.25.3 (FileStorage.Filename joins the root with the key;
// StorageKeys.SiteCert = certificates/<issuer>/<domain>/<domain>.crt), so if a
// future certmagic moves its assets this test is supposed to break.
func seedACMEStore(t *testing.T, home, issuer, domain string, data []byte) string {
	t.Helper()
	dir := filepath.Join(home, ".tether", "acme", "certificates", issuer, domain)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, domain+".crt")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The issuer directory is named after whichever CA certmagic's package default
// points at, which this repo never sets — so the lookup enumerates rather than
// hard-coding today's value. The fixture uses a CA that is not Let's Encrypt
// precisely so that writing "acme-v02.api.letsencrypt.org-directory" into
// acmeStoredCert would fail this test.
//
// It is not a claim that a real store can hold two issuers; it cannot (see
// acmeStoredCert). It is a claim that the name is not this code's to know.
//
// The fixture name is lower-case because certmagic's own names are: every
// directory in a real store was written through KeyBuilder.Safe, which
// lower-cases. A mixed-case fixture would be testing a store certmagic cannot
// produce — and would fail, since acmeStoredCert re-applies Safe to the name it
// reads back.
func TestLocateCert_ACMEFindsACertUnderAnyIssuerDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := seedACMEStore(t, home, "acme.zerossl.com-v2-dv90", "example.com", []byte("pem"))

	loc, err := LocateCert(&Config{AcmeDomain: "example.com"})
	if err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if loc.Source != CertSourceACME {
		t.Errorf("source = %v, want acme", loc.Source)
	}
	if loc.Path != want {
		t.Errorf("LocateCert path = %q, want %q", loc.Path, want)
	}
}

func TestLocateCert_ACMEIgnoresOtherDomainsInTheSameStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedACMEStore(t, home, "acme-v02.api.letsencrypt.org-directory", "other.example", []byte("pem"))

	loc, err := LocateCert(&Config{AcmeDomain: "example.com"})
	if err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if loc.Path != "" {
		t.Errorf("path = %q, want empty: the store holds no cert for example.com", loc.Path)
	}
}

// An empty store is the state before the first start — certmagic obtains the
// cert during startup — so it is reported as "nothing stored", not as an error
// the caller would have to translate.
func TestLocateCert_ACMEReportsNoPathWhenNothingIsStored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	loc, err := LocateCert(&Config{AcmeDomain: "example.com"})
	if err != nil {
		t.Fatalf("LocateCert on an empty store: %v", err)
	}
	if loc.Source != CertSourceACME || loc.Path != "" {
		t.Errorf("LocateCert = %v %q, want acme with an empty path", loc.Source, loc.Path)
	}
}

// The managed cert is the one file an ACME deployment has but does not serve:
// Run's Step 4 mints it before Step 4b replaces the bundle, and certRenewalFor
// starts no loop to keep it current. Answering with it here is the bug.
func TestLocateCert_ACMEDoesNotAnswerWithTheManagedCert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := loadOrRotateManaged(); err != nil {
		t.Fatalf("loadOrRotateManaged: %v", err)
	}
	managed, _ := managedCertFiles(filepath.Join(home, ".tether"))

	loc, err := LocateCert(&Config{AcmeDomain: "example.com"})
	if err != nil {
		t.Fatalf("LocateCert: %v", err)
	}
	if loc.Path == managed {
		t.Errorf("LocateCert answered with the managed cert %q for an ACME deployment", managed)
	}
}

// "I could not read the store" must not come back as "the store is empty".
// Folding one into the other is the same mistake tether#84 is about, one level
// down: the caller would report a cert that has not been issued yet, on a host
// whose cert may be sitting right there unreadable.
//
// The store is corrupted by putting a file where the directory belongs, rather
// than by removing permissions: these tests run as root often enough (CI
// containers, this development box) that a 0000 directory is still readable and
// the fixture would quietly stop reproducing anything.
func TestLocateCert_ACMEReportsAStoreItCannotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	acme := filepath.Join(home, ".tether", "acme")
	if err := os.MkdirAll(acme, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acme, "certificates"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LocateCert(&Config{AcmeDomain: "example.com"}); err == nil {
		t.Error("LocateCert returned no error for a store it cannot list")
	}
}

func TestACMEStoredDomains_ListsWhatIsInTheStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The same domain under two issuers, plus a second domain: the caller uses
	// this to name a deployment, so a duplicate would read as two of them.
	seedACMEStore(t, home, "acme-v02.api.letsencrypt.org-directory", "one.example", []byte("pem"))
	seedACMEStore(t, home, "acme.zerossl.com-v2-dv90", "one.example", []byte("pem"))
	seedACMEStore(t, home, "acme-v02.api.letsencrypt.org-directory", "two.example", []byte("pem"))

	got, err := ACMEStoredDomains()
	if err != nil {
		t.Fatalf("ACMEStoredDomains: %v", err)
	}
	sort.Strings(got)
	if want := []string{"one.example", "two.example"}; !slices.Equal(got, want) {
		t.Errorf("ACMEStoredDomains = %v, want %v", got, want)
	}
}

// No store is the normal answer on the two other cert paths, and it must be
// empty rather than an error: the caller appends a note when there is one and
// says nothing when there is not.
func TestACMEStoredDomains_EmptyWithoutAStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := ACMEStoredDomains()
	if err != nil {
		t.Fatalf("ACMEStoredDomains on a host that never ran ACME: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ACMEStoredDomains = %v, want none", got)
	}
}

func TestACMEStoredDomains_ReportsAStoreItCannotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	acme := filepath.Join(home, ".tether", "acme")
	if err := os.MkdirAll(acme, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acme, "certificates"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ACMEStoredDomains(); err == nil {
		t.Error("ACMEStoredDomains returned no error for a store it cannot list")
	}
}
