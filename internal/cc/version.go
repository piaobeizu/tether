package cc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// detectWaitDelay forces exec.CommandContext to close subprocess pipes
// this long after ctx cancel even if a stuck grandchild keeps stdin/stdout
// fds open. Without this, `cmd.Output()` can wait indefinitely for EOF
// even after SIGKILL on the bash wrapper. Go 1.20+ feature.
const detectWaitDelay = 200 * time.Millisecond

// Version is a parsed semver triple from `claude --version`. Raw preserves
// the original output for diagnostics (cc decorates with " (Claude Code)").
type Version struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

// String renders the parsed triple (without Raw decoration).
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// SupportedRange is the cc compatibility window. The data plane (stream-json
// keys, hook callback shape, JSONL torn-write tolerance) is verified within
// this range; outside it we hard-fail at startup rather than risk silent
// drift. Currently a single-minor window: cc's contract churns at minor
// bumps in practice.
type SupportedRange struct {
	Major    int // exact match required
	Minor    int // exact match required
	MinPatch int // inclusive floor (no upper bound — patches are usually safe)
}

// DefaultSupported is the cc range the v0.1 daemon is verified against.
// Per gh-13 retro § "Held assumptions": stream-json data plane verified
// across cc 2.1.120 → 2.1.123; .120 set as the floor. Bumping the floor
// requires a fresh PoC-1 run to confirm hook payload + JSONL keys.
var DefaultSupported = SupportedRange{
	Major:    2,
	Minor:    1,
	MinPatch: 120,
}

// ErrVersionUnsupported is returned by Verify (and the convenience
// DetectAndVerify) when the detected cc version is outside the supported
// range. errors.Is matches the chain.
var ErrVersionUnsupported = errors.New("cc: detected version is outside supported range")

// ErrVersionUnparseable is returned when the output of `claude --version`
// doesn't contain a recognizable major.minor.patch triple.
var ErrVersionUnparseable = errors.New("cc: failed to parse `claude --version` output")

// versionRegex extracts the first major.minor.patch occurrence in the
// `claude --version` output. cc currently emits "X.Y.Z (Claude Code)";
// the regex tolerates leading "v" / whitespace / trailing build metadata
// without coupling to the exact format.
var versionRegex = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// ParseVersion parses a cc --version output line into a Version. The Raw
// field preserves the input (trimmed) for diagnostic reporting.
func ParseVersion(s string) (Version, error) {
	trimmed := strings.TrimSpace(s)
	m := versionRegex.FindStringSubmatch(trimmed)
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q", ErrVersionUnparseable, trimmed)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch, Raw: trimmed}, nil
}

// Detect runs `claude --version` (or the binary at binaryPath if non-empty)
// and parses its output. Subprocess errors propagate verbatim; parse errors
// chain to ErrVersionUnparseable.
func Detect(ctx context.Context, binaryPath string) (Version, error) {
	bin := binaryPath
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.WaitDelay = detectWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return Version{}, fmt.Errorf("cc: %s --version failed: %w", bin, err)
	}
	return ParseVersion(string(out))
}

// Verify hard-fails when v is outside r. Returns nil when v is within
// the range.
//
// The contract is exact-major + exact-minor + min-patch: a different major
// or minor is unsupported even if numerically newer. Patch versions ≥ floor
// are accepted. This matches the gh-13 verification shape (one minor
// stream verified as a unit; bumps must be re-verified).
func Verify(v Version, r SupportedRange) error {
	if v.Major != r.Major || v.Minor != r.Minor {
		return fmt.Errorf("%w: detected %s, supported %d.%d.x (≥ %d.%d.%d)",
			ErrVersionUnsupported, v.String(), r.Major, r.Minor, r.Major, r.Minor, r.MinPatch)
	}
	if v.Patch < r.MinPatch {
		return fmt.Errorf("%w: detected %s, supported ≥ %d.%d.%d",
			ErrVersionUnsupported, v.String(), r.Major, r.Minor, r.MinPatch)
	}
	return nil
}

// DetectAndVerify is the convenience wrapper for daemon startup: runs
// Detect then Verify against r, returning the parsed version on success
// or a wrapped error suitable for hard-failing the process.
func DetectAndVerify(ctx context.Context, binaryPath string, r SupportedRange) (Version, error) {
	v, err := Detect(ctx, binaryPath)
	if err != nil {
		return v, err
	}
	if err := Verify(v, r); err != nil {
		return v, err
	}
	return v, nil
}
