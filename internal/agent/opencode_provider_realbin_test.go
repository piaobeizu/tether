//go:build opencode_real

// This file is excluded from the default build on purpose. Everything in it
// needs a real `opencode` binary on PATH and spawns real subprocesses.
//
// Why a build tag and not the `if LookPath fails { t.Skip }` this test used to
// carry (tether#160): a skip is indistinguishable from a pass in every place a
// merge decision is made. `go test ./...` printed ok, the summary counted the
// test, and nobody could tell that CI — which has no opencode — had never once
// executed it, while a developer box, which does, ran a real `opencode serve`
// inside the -race suite and went red under parallel load. That is the worst
// pairing available: the green means nothing and the red is indistinguishable
// from a real regression.
//
// Behind a tag the default suite does not list this test at all, so its result
// cannot be read as coverage it does not provide. To make the same point from
// the other side, nothing in here skips: if you asked for the tag and the
// binary is missing, that is a failure, not a pass. A gate that can go green
// without executing is not a gate.
//
// Run it:
//
//	GOWORK=off go test -tags=opencode_real -count=1 ./internal/agent/ -run RealBinary -v
//
// CI compiles this file (a `go vet -tags=opencode_real` step) but does not run
// it, so it cannot rot into something that no longer builds — the failure mode
// build tags normally invite.

package agent

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestOpenCodeServeCrash_RealBinary is the TestOpenCodeServeCrash regression
// against the genuine `opencode serve`, covering the wiring the fixtures in
// opencode_provider_test.go stub out: that startServe really arms the watcher,
// and that a SIGKILL from outside the session really ends it.
//
// Unlike TestOpenCodeInterrupt_Integration it needs no credentials and makes no
// model call — it starts a serve, kills it, and watches the session react.
func TestOpenCodeServeCrash_RealBinary(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Fatalf("opencode binary not found in PATH: %v\n"+
			"-tags=opencode_real asks for the real-binary test; a missing binary "+
			"is a setup failure, not a reason to report success", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := NewOpenCodeProvider().Spawn(ctx, SpawnConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer sess.Close()
	oc := sess.(*opencodeSession)

	if !sess.Alive() {
		t.Fatal("Alive() = false for a freshly spawned session")
	}

	// Drain like Registry.fanOut does, and record when the stream ends.
	streamEnded := make(chan struct{})
	go func() {
		defer close(streamEnded)
		for range sess.Events() {
		}
	}()

	oc.mu.RLock()
	proc := oc.serve.Process
	oc.mu.RUnlock()
	t.Logf("killing real `opencode serve` pid=%d from outside the session", proc.Pid)
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill serve: %v", err)
	}

	select {
	case <-streamEnded:
	case <-time.After(15 * time.Second):
		t.Fatal("event stream never ended after the serve was killed (tether#58)")
	}
	if sess.Alive() {
		t.Error("Alive() = true after the real serve was killed")
	}
}
