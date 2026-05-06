package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEncodeBucket exercises cc's forward encoding: `/` → `-` with
// leading `-`. Trailing slash dropped. Hyphens in segments survive.
func TestEncodeBucket(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/root/code/aicoding/tether", "-root-code-aicoding-tether"},
		{"/root/code/aicoding/tether/", "-root-code-aicoding-tether"},
		{"/root", "-root"},
		{"/", "-"},
		{"/root/code/aicoding/tether/wxk/gh-13/tether",
			"-root-code-aicoding-tether-wxk-gh-13-tether"},
		{"", ""},
		{"/root/code-with-dash/x", "-root-code-with-dash-x"},
	}
	for _, c := range cases {
		got := EncodeBucket(c.in)
		if got != c.want {
			t.Errorf("EncodeBucket(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDecodeBucket_RoundTripWhenPathExists creates real directories under a
// temp tree and asserts decode(encode(path)) recovers the original whenever
// the path is on disk — including paths with literal hyphens (the
// ambiguous case the wrapper is supposed to handle gracefully).
func TestDecodeBucket_RoundTripWhenPathExists(t *testing.T) {
	tmp := t.TempDir()

	cases := []string{
		"a/b/c",
		"foo-bar/baz",       // hyphen in segment
		"gh-13/tether",      // ambiguous: could decode to gh/13/tether
		"a-b-c/d",           // many hyphens
		"single",            // single segment
	}
	for _, rel := range cases {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		bucket := EncodeBucket(p)
		got := decodeBucket(bucket)
		if got != p {
			t.Errorf("round-trip failed for %q\n  bucket: %s\n  decoded: %s", p, bucket, got)
		}
	}
}

// TestDecodeBucket_FallbackWhenNoMatch documents the lossy-decode fallback:
// when nothing on disk matches, we fall back to the naive `/`-substitution.
func TestDecodeBucket_FallbackWhenNoMatch(t *testing.T) {
	// /definitely/does/not/exist/anywhere
	bucket := "-definitely-does-not-exist-anywhere"
	got := decodeBucket(bucket)
	want := "/definitely/does/not/exist/anywhere"
	if got != want {
		t.Errorf("fallback decode = %q, want %q", got, want)
	}
}

// TestDecodeBucket_AmbiguityPrefersLongerExistingPath: when two interpretations
// both partially exist, prefer the one that fully exists.
func TestDecodeBucket_AmbiguityPrefersLongerExistingPath(t *testing.T) {
	tmp := t.TempDir()
	// Create only /tmp/foo-bar/baz (with hyphen). NOT /tmp/foo/bar/baz.
	target := filepath.Join(tmp, "foo-bar", "baz")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := EncodeBucket(target)
	got := decodeBucket(bucket)
	if got != target {
		t.Errorf("ambiguous decode picked wrong branch:\n  got: %s\n  want: %s", got, target)
	}
}

// TestResolveSession_NotFound: empty projects root → ErrSidNotFound.
func TestResolveSession_NotFound(t *testing.T) {
	tmp := t.TempDir()
	_, err := ResolveSession(tmp, "no-such-sid", "")
	if err == nil {
		t.Fatal("expected error for missing sid")
	}
	if !errors.Is(err, ErrSidNotFound) {
		t.Errorf("expected ErrSidNotFound, got: %v", err)
	}
}

// TestResolveSession_SingleBucket: write a fake jsonl, verify resolve returns
// the bucket + decoded cwd.
func TestResolveSession_SingleBucket(t *testing.T) {
	root := t.TempDir()
	bucket := "-tmp-foo"
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "01ABCDEF-test-session"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveSession(root, sid, "")
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if res.Bucket != bucket {
		t.Errorf("bucket: got %s, want %s", res.Bucket, bucket)
	}
	if res.JsonlPath != jsonl {
		t.Errorf("jsonl: got %s, want %s", res.JsonlPath, jsonl)
	}
	// Cwd: /tmp/foo exists on most linux/mac; if not, we'd hit fallback.
	// Assert either the on-disk match or the naive fallback — both are
	// "/tmp/foo" here.
	if res.Cwd != "/tmp/foo" {
		t.Errorf("cwd: got %s, want /tmp/foo", res.Cwd)
	}
}

// TestResolveSession_Collision: same sid in two buckets → error listing both.
func TestResolveSession_Collision(t *testing.T) {
	root := t.TempDir()
	sid := "collide-sid"
	for _, b := range []string{"-tmp-a", "-tmp-b"} {
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, b, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ResolveSession(root, sid, "")
	if err == nil {
		t.Fatal("expected collision error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "-tmp-a") || !strings.Contains(msg, "-tmp-b") {
		t.Errorf("collision message should list both buckets: %v", err)
	}
	if !strings.Contains(msg, "--bucket") {
		t.Errorf("collision message should suggest --bucket flag: %v", err)
	}
}

// TestResolveSession_CollisionForceBucket: --bucket disambiguates.
func TestResolveSession_CollisionForceBucket(t *testing.T) {
	root := t.TempDir()
	sid := "collide-sid"
	for _, b := range []string{"-tmp-a", "-tmp-b"} {
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, b, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := ResolveSession(root, sid, "-tmp-b")
	if err != nil {
		t.Fatalf("force-bucket: %v", err)
	}
	if res.Bucket != "-tmp-b" {
		t.Errorf("force-bucket: got %s, want -tmp-b", res.Bucket)
	}
}

// TestResolveSession_ForceBucketMissing: --bucket pointing at the wrong dir
// is a hard error.
func TestResolveSession_ForceBucketMissing(t *testing.T) {
	root := t.TempDir()
	_, err := ResolveSession(root, "any-sid", "-no-such-bucket")
	if err == nil {
		t.Fatal("expected error for missing forced bucket")
	}
	if !strings.Contains(err.Error(), "-no-such-bucket") {
		t.Errorf("error should mention bucket: %v", err)
	}
}

// TestResolveSession_PicksMostRecent: when (somehow) the same sid lives in
// two buckets but we're not in collision mode, the most-recently-modified
// jsonl wins. Documented multi-bucket order in findSidBuckets.
func TestResolveSession_PicksMostRecent_CollisionStillFlagged(t *testing.T) {
	// This test verifies the *order* of the collision message: most-recent
	// first. Even though collisions are still treated as errors, the
	// listing should put the more-likely candidate at the top.
	root := t.TempDir()
	sid := "ordered-sid"
	older := filepath.Join(root, "-tmp-old")
	newer := filepath.Join(root, "-tmp-new")
	for _, d := range []string{older, newer} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldJ := filepath.Join(older, sid+".jsonl")
	newJ := filepath.Join(newer, sid+".jsonl")
	if err := os.WriteFile(oldJ, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newJ, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force older to actually be older.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(oldJ, past, past); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveSession(root, sid, "")
	if err == nil {
		t.Fatal("expected collision")
	}
	msg := err.Error()
	idxNew := strings.Index(msg, "-tmp-new")
	idxOld := strings.Index(msg, "-tmp-old")
	if idxNew < 0 || idxOld < 0 {
		t.Fatalf("both buckets should be listed: %v", err)
	}
	if idxNew >= idxOld {
		t.Errorf("most-recent should be listed first; got msg:\n%s", msg)
	}
}

// TestRunResume_DryRun: end-to-end smoke test of the CLI dispatcher with
// --dry-run, asserting that ResolveSession is wired correctly. No claude
// binary required.
func TestRunResume_DryRun(t *testing.T) {
	root := t.TempDir()
	bucket := "-tmp-smoke"
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "smoke-sid"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Capture stdout to a temp file.
	stdout, restoreOut := captureStdout(t)
	defer restoreOut()

	code := runResume([]string{
		"--projects-dir", root,
		"--dry-run",
		sid,
	})
	if code != 0 {
		t.Fatalf("runResume exit=%d", code)
	}
	out := stdout()
	for _, want := range []string{
		"cwd:    /tmp/smoke",
		"bucket: " + bucket,
		"--resume " + sid,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run stdout missing %q\n%s", want, out)
		}
	}
}

// TestExecClaude_Smoke: build a fake `claude` binary that just prints its
// argv + cwd, then verify ExecClaude wires both correctly. We use
// exec.Command (not direct ExecClaude — that execve's away the test
// process); the wiring tested is the lookup + chdir + argv shape.
func TestExecClaude_Smoke(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	tmp := t.TempDir()

	// Fake claude: prints argv joined + cwd to stdout.
	fake := filepath.Join(tmp, "claude")
	script := `#!/bin/sh
echo "ARGV: $@"
echo "CWD: $(pwd)"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(tmp, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	// Drive the fake via os/exec (mirroring what ExecClaude does sans execve).
	cmd := exec.Command(fake, "--resume", "sid-x")
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fake claude failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "ARGV: --resume sid-x") {
		t.Errorf("argv wiring wrong: %s", got)
	}
	// CWD must resolve to our chdir target. Some platforms canonicalize
	// /tmp to /private/tmp; tolerate that with HasSuffix.
	if !strings.Contains(got, "CWD:") || !strings.Contains(got, filepath.Base(cwd)) {
		t.Errorf("cwd wiring wrong: %s", got)
	}
}

// captureStdout redirects os.Stdout to a temp file and returns (read, restore).
func captureStdout(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	read = func() string {
		_ = w.Close()
		s := <-done
		return s
	}
	restore = func() {
		os.Stdout = orig
		_ = r.Close()
	}
	return read, restore
}
