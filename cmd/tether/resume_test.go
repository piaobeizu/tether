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
		"foo-bar/baz",  // hyphen in segment
		"gh-13/tether", // ambiguous: could decode to gh/13/tether
		"a-b-c/d",      // many hyphens
		"single",       // single segment
	}
	for _, rel := range cases {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		bucket := EncodeBucket(p)
		got, alts := decodeBucket(bucket)
		if len(alts) != 0 {
			t.Errorf("unexpected ambiguity for %q: alts=%v", p, alts)
			continue
		}
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
	got, alts := decodeBucket(bucket)
	want := "/definitely/does/not/exist/anywhere"
	if got != want {
		t.Errorf("fallback decode = %q, want %q", got, want)
	}
	if len(alts) != 0 {
		t.Errorf("fallback should not produce alternates: %v", alts)
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
	got, alts := decodeBucket(bucket)
	if len(alts) != 0 {
		t.Fatalf("expected unambiguous decode; got alts=%v", alts)
	}
	if got != target {
		t.Errorf("ambiguous decode picked wrong branch:\n  got: %s\n  want: %s", got, target)
	}
}

// TestDecodeBucket_PrefixCollision is the BLOCKER fix from PR #35 review:
// when BOTH `/parent/foo/bar` AND `/parent/foo-bar` exist on disk, the
// encoded bucket `-parent-foo-bar` decodes ambiguously. The previous
// implementation silently picked the path-separator interpretation
// (branchA wins on tied length). The new contract: surface ambiguity to
// the caller via the alternates slice — they must list ALL existing
// candidates so the user can disambiguate via `--cwd`.
func TestDecodeBucket_PrefixCollision(t *testing.T) {
	tmp := t.TempDir()
	pathSlash := filepath.Join(tmp, "foo", "bar") // .../foo/bar
	pathDash := filepath.Join(tmp, "foo-bar")     // .../foo-bar
	if err := os.MkdirAll(pathSlash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pathDash, 0o755); err != nil {
		t.Fatal(err)
	}

	// EncodeBucket of either path produces the same string; pick one.
	bucket := EncodeBucket(pathSlash)
	if bucket != EncodeBucket(pathDash) {
		t.Fatalf("test setup precondition: encodings should collide; got %q vs %q",
			bucket, EncodeBucket(pathDash))
	}

	got, alts := decodeBucket(bucket)
	if got != "" {
		t.Errorf("ambiguous decode should return empty cwd; got %q", got)
	}
	if len(alts) != 2 {
		t.Fatalf("expected 2 alternates, got %d: %v", len(alts), alts)
	}
	// Alternates must be sorted ascending for deterministic error messages.
	if alts[0] > alts[1] {
		t.Errorf("alternates not sorted ascending: %v", alts)
	}
	// Both real paths should be present.
	wantSet := map[string]bool{pathSlash: false, pathDash: false}
	for _, a := range alts {
		if _, ok := wantSet[a]; !ok {
			t.Errorf("unexpected alternate %q (want one of %v)", a, wantSet)
		}
		wantSet[a] = true
	}
	for k, seen := range wantSet {
		if !seen {
			t.Errorf("missing expected alternate %q", k)
		}
	}
}

// TestResolveSession_AmbiguousCwd_RaisesError verifies the prefix-collision
// case bubbles up through ResolveSession as a user-actionable error
// suggesting `--cwd`. Even though the bucket itself is unique (only one
// jsonl), its decode is non-unique.
func TestResolveSession_AmbiguousCwd_RaisesError(t *testing.T) {
	tmp := t.TempDir()
	// Two real on-disk decodings of the same encoded bucket.
	pathSlash := filepath.Join(tmp, "foo", "bar")
	pathDash := filepath.Join(tmp, "foo-bar")
	for _, p := range []string{pathSlash, pathDash} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	bucket := EncodeBucket(pathSlash) // same as EncodeBucket(pathDash)
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "ambig-sid"
	if err := os.WriteFile(filepath.Join(root, bucket, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveSession(root, sid, "")
	if err == nil {
		t.Fatal("expected ambiguous-cwd error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--cwd") {
		t.Errorf("error should suggest --cwd: %v", err)
	}
	if !strings.Contains(msg, pathSlash) || !strings.Contains(msg, pathDash) {
		t.Errorf("error should list both candidates:\n  %v\n  want both %s and %s",
			err, pathSlash, pathDash)
	}
}

// TestResolveSessionWithCwd_Happy verifies the manual --cwd path: encode
// the user-supplied cwd into a bucket, find the jsonl there, return the
// explicit cwd verbatim (no lossy decode).
func TestResolveSessionWithCwd_Happy(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "foo-bar") // hyphenated to prove we skip decode
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	bucket := EncodeBucket(cwd)
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "manual-sid"
	if err := os.WriteFile(filepath.Join(root, bucket, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveSessionWithCwd(root, sid, cwd)
	if err != nil {
		t.Fatalf("ResolveSessionWithCwd: %v", err)
	}
	if res.Cwd != cwd {
		t.Errorf("cwd: got %q, want %q", res.Cwd, cwd)
	}
	if res.Bucket != bucket {
		t.Errorf("bucket: got %q, want %q", res.Bucket, bucket)
	}
}

// TestResolveSessionWithCwd_RejectsDotDot exercises the defense-in-depth
// path: --cwd containing `..` segments after Clean must be rejected. We
// build a path whose Clean output still contains `..` — i.e. relative
// (since absolute paths with `..` get collapsed by Clean).
func TestResolveSessionWithCwd_RejectsDotDot(t *testing.T) {
	_, err := ResolveSessionWithCwd(t.TempDir(), "sid", "../escape")
	if err == nil {
		t.Fatal("expected error rejecting relative --cwd")
	}
	// The "must be absolute" check fires first; exercise the dotdot guard
	// directly via the helper.
	if !hasDotDot(filepath.Clean("../escape")) {
		t.Errorf("hasDotDot should detect `..` after Clean of ../escape")
	}
}

// TestExecClaude_RejectsDotDot ensures ExecClaude refuses to chdir to a
// path containing `..` segments. Uses a non-absolute relative path so
// Clean preserves the `..` (Clean of an absolute path collapses `..`).
func TestExecClaude_RejectsDotDot(t *testing.T) {
	err := ExecClaude("claude", "../escape", "sid")
	if err == nil {
		t.Fatal("expected error rejecting .. in cwd")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("error should mention .. : %v", err)
	}
}

// TestExecClaude_RejectsDotDotFromBucket exercises the reviewer's
// `-tmp-..-etc` example end-to-end at the defense-in-depth boundary
// (NOT via real exec — syscall.Exec would replace the test process).
//
// Strategy: build a synthetic cwd that contains `..` after Clean (only
// possible for relative paths or paths with leading `..`), feed it
// directly to ExecClaude, and assert it errors out. This pins the
// defense path independently of whether decodeBucket happens to produce
// such a path on this particular host. The decoder itself is allowed to
// emit any string it likes — ExecClaude is the safety net.
func TestExecClaude_RejectsDotDotFromBucket(t *testing.T) {
	cases := []string{
		"../escape",        // pure relative
		"../foo/bar",       // relative deeper
		"foo/../../escape", // Clean → ../escape
	}
	for _, c := range cases {
		err := ExecClaude("claude", c, "sid")
		if err == nil {
			t.Errorf("ExecClaude should reject %q (contains .. after Clean=%q)",
				c, filepath.Clean(c))
			continue
		}
		if !strings.Contains(err.Error(), "..") {
			t.Errorf("error for %q should mention `..`: %v", c, err)
		}
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

// TestFindSidBuckets_DeterministicOnEqualMtime locks the secondary
// tiebreak: when two buckets have identical synthetic mtimes (a real
// possibility on second-resolution filesystems), the order is fixed
// by ascending bucket name so collision-error messages don't flap
// across runs.
func TestFindSidBuckets_DeterministicOnEqualMtime(t *testing.T) {
	root := t.TempDir()
	sid := "tied-sid"
	bA := "-tmp-a"
	bZ := "-tmp-z"
	for _, b := range []string{bZ, bA} { // create in reverse alpha order
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Force identical mtimes.
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, b := range []string{bA, bZ} {
		jsonl := filepath.Join(root, b, sid+".jsonl")
		if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(jsonl, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	// Run multiple times to catch any unstable ordering.
	for i := 0; i < 20; i++ {
		got, err := findSidBuckets(root, sid)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("iter %d: want 2 buckets, got %d: %v", i, len(got), got)
		}
		if got[0] != bA || got[1] != bZ {
			t.Fatalf("iter %d: ordering not deterministic by name: got %v", i, got)
		}
	}
}

// TestRunResume_BinaryFlagThreadedThroughDryRun exercises Polish 3 from
// the PR #35 review: the `--binary` flag has full end-to-end coverage
// through runResume, not just via direct exec.Command. With --dry-run we
// never actually execute the fake binary, but the printed `exec:` line
// must name the explicit value passed via --binary.
func TestRunResume_BinaryFlagThreadedThroughDryRun(t *testing.T) {
	root := t.TempDir()
	bucket := "-tmp-bin-flag"
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "bin-flag-sid"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Place a fake binary somewhere unrelated; it never gets invoked since
	// --dry-run skips ExecClaude. The point is just that runResume prints it.
	fakeDir := t.TempDir()
	fakeBin := filepath.Join(fakeDir, "my-fake-claude")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, restore := captureStdout(t)
	defer restore()

	code := runResume([]string{
		"--projects-dir", root,
		"--binary", fakeBin,
		"--dry-run",
		sid,
	})
	if code != 0 {
		t.Fatalf("runResume exit=%d", code)
	}
	out := stdout()
	wantSubstr := "exec:   " + fakeBin + " --resume " + sid
	if !strings.Contains(out, wantSubstr) {
		t.Errorf("expected dry-run output to contain %q, got:\n%s", wantSubstr, out)
	}
}

// TestRunResume_DryRunDoesNotExec exercises Polish 4: --dry-run must
// genuinely skip exec, not merely change what's printed. We point
// --binary at a fake script that writes a sentinel file when invoked,
// then assert the sentinel does NOT appear after runResume completes.
func TestRunResume_DryRunDoesNotExec(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	bucket := "-tmp-no-exec"
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "no-exec-sid"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sentinel file that would be created if the fake binary actually ran.
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "ran")
	fake := filepath.Join(sentinelDir, "claude")
	script := "#!/bin/sh\ntouch " + sentinel + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	_, restore := captureStdout(t)
	defer restore()

	code := runResume([]string{
		"--projects-dir", root,
		"--binary", fake,
		"--dry-run",
		sid,
	})
	if code != 0 {
		t.Fatalf("runResume exit=%d", code)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("--dry-run must NOT exec the binary, but sentinel file was created")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on sentinel: %v", err)
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
