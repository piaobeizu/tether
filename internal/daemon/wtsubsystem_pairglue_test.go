// wtsubsystem_pairglue_test.go — slice #4 daemon ↔ pair integration
// e2e tests. Boots the daemon with WTListener=:0 + a hermetic pair
// registry/audit dir, then exercises:
//
//   - happy path: pair from a Go-side wt.Client, reconnect with
//     SessionIDHeader{SessionID, DeviceID}, push envelopes, decrypt
//     using the long-term key persisted into the registry.
//   - legacy peer (no DeviceID) fallback to wt.DevSharedKey.
//   - re-pair attempt rejected with pair.abort{dup-deviceid}.

package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/daemon"
	"github.com/piaobeizu/tether/internal/pair"
	"github.com/piaobeizu/tether/internal/transport/wt"
)

// runPairClient drives a pair.Client.Run against the freshly-opened
// control stream. The wt.Client is the stream owner; this helper just
// blocks until the pair flow returns.
func runPairClient(t *testing.T, ctx context.Context, cli *wt.Client, deviceID pair.DeviceID, displayName string) pair.Result {
	t.Helper()
	ctrl, err := cli.OpenControl(ctx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	pc := pair.NewClient(pair.ClientConfig{
		Identity: pair.Identity{
			DeviceID:    deviceID,
			Kind:        pair.KindMobile,
			DisplayName: displayName,
		},
		Confirmer: pair.AutoConfirm,
	})
	res, err := pc.Run(ctx, ctrl)
	if err != nil {
		t.Fatalf("pair.Client.Run: %v", err)
	}
	return res
}

// runPairClientExpectingErr runs the pair flow and returns the error
// it produced (used by the re-pair rejection test).
func runPairClientExpectingErr(t *testing.T, ctx context.Context, cli *wt.Client, deviceID pair.DeviceID) error {
	t.Helper()
	ctrl, err := cli.OpenControl(ctx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	pc := pair.NewClient(pair.ClientConfig{
		Identity: pair.Identity{
			DeviceID:    deviceID,
			Kind:        pair.KindMobile,
			DisplayName: "retry phone",
		},
		Confirmer: pair.AutoConfirm,
	})
	_, err = pc.Run(ctx, ctrl)
	return err
}

// TestDaemonGlue_PairThenSession_E2E — boot daemon with no pre-paired
// devices. Connect, run pair flow → daemon persists deviceId + ltk.
// Disconnect, reconnect with SessionIDHeader{SessionID, DeviceID},
// inject envelopes, decrypt on the client using the persisted key.
func TestDaemonGlue_PairThenSession_E2E(t *testing.T) {
	t.Parallel()

	const sid = "test-pair-glue-sid"
	const deviceID = pair.DeviceID("device-mobile-pairtest1")

	tmp := t.TempDir()
	pairRoot := filepath.Join(tmp, "pair-devices")
	pairAudit := filepath.Join(tmp, "audit.log")

	url, em, cancel := bootDaemonWithWT(t, daemon.Config{
		PairRegistryRoot: pairRoot,
		PairAuditLogPath: pairAudit,
	})
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer dialCancel()

	// 1. Run pair from a fresh wt.Client. Daemon's responder side
	// persists the device record into pairRoot + audits pair.success.
	pairCli := dialDaemonWT(t, dialCtx, url)
	res := runPairClient(t, dialCtx, pairCli, deviceID, "PairTest Phone")
	if res.SAS == "" {
		t.Fatalf("pair returned empty SAS")
	}
	if len(res.LongTermKey) != wt.SharedKeySize {
		t.Fatalf("LongTermKey size %d want %d", len(res.LongTermKey), wt.SharedKeySize)
	}
	pairCli.Close()

	// 2. Verify the daemon's registry has the new record with a key
	// matching what the client derived. (Reading directly from disk
	// — the daemon's Registry is internal but the on-disk shape is
	// the contract.)
	verifyReg, err := pair.NewRegistry(pair.RegistryConfig{Root: pairRoot})
	if err != nil {
		t.Fatalf("verify registry: %v", err)
	}
	persisted, err := verifyReg.Load(deviceID)
	if err != nil {
		t.Fatalf("registry.Load %s: %v", deviceID, err)
	}
	if len(persisted.LongTermKey) != wt.SharedKeySize {
		t.Fatalf("persisted ltk size %d want %d", len(persisted.LongTermKey), wt.SharedKeySize)
	}
	// The daemon (responder) and the client (initiator) derive the
	// same long-term key from the shared transcript hash; if these
	// diverge the pair handshake is broken.
	if string(persisted.LongTermKey) != string(res.LongTermKey) {
		t.Fatalf("daemon-persisted ltk != client-derived ltk")
	}

	// 3. Reconnect with SessionIDHeader{SessionID, DeviceID}, accept
	// events, inject envelopes, decrypt on the client side using the
	// long-term key from the client-side pair Result.
	sessCli := dialDaemonWT(t, dialCtx, url)
	defer sessCli.Close()

	ctrl, err := sessCli.OpenControl(dialCtx)
	if err != nil {
		t.Fatalf("OpenControl session: %v", err)
	}
	if err := wt.WriteSessionHeader(ctrl, wt.SessionIDHeader{SessionID: sid, DeviceID: string(deviceID)}); err != nil {
		t.Fatalf("WriteSessionHeader: %v", err)
	}

	events, err := sessCli.AcceptEvents(dialCtx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}

	want := agent.LocalEnvelope{
		Kind:              "output.agent-event",
		SessionID:         sid,
		ProviderType:      "claude-code",
		PlaintextMetadata: map[string]any{"i": 1},
	}
	for retry := 0; retry < 5; retry++ {
		delivered, err := em.Inject(want)
		if err != nil {
			t.Fatalf("Inject: %v", err)
		}
		if delivered == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	er, err := wt.NewEnvelopeFrameReader(events, res.LongTermKey, sid)
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader: %v", err)
	}
	type result struct {
		env *wt.WireEnvelope
		pt  []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		env, pt, err := er.Next()
		ch <- result{env, pt, err}
	}()
	var r result
	select {
	case r = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("frame read timeout")
	}
	if r.err != nil {
		t.Fatalf("Next: %v", r.err)
	}
	if r.env.Kind != want.Kind {
		t.Errorf("kind=%q want %q", r.env.Kind, want.Kind)
	}
	if r.env.ToDeviceID != string(deviceID) {
		t.Errorf("toDeviceId=%q want %q", r.env.ToDeviceID, deviceID)
	}
	var inner agent.LocalEnvelope
	if err := json.Unmarshal(r.pt, &inner); err != nil {
		t.Fatalf("inner unmarshal: %v", err)
	}
	if inner.SessionID != sid {
		t.Errorf("inner.SessionID=%q want %q", inner.SessionID, sid)
	}
}

// TestDaemonGlue_LegacyPeer_DevKeyFallback — peer skips pair entirely
// and sends SessionIDHeader without DeviceID. Daemon must fall back to
// wt.DevSharedKey so the existing cross-stack flows keep working.
func TestDaemonGlue_LegacyPeer_DevKeyFallback(t *testing.T) {
	t.Parallel()

	const sid = "test-legacy-fallback"

	tmp := t.TempDir()
	pairRoot := filepath.Join(tmp, "pair-devices")
	pairAudit := filepath.Join(tmp, "audit.log")

	url, em, cancel := bootDaemonWithWT(t, daemon.Config{
		PairRegistryRoot: pairRoot,
		PairAuditLogPath: pairAudit,
	})
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer dialCancel()

	cli := dialDaemonWT(t, dialCtx, url)
	defer cli.Close()

	ctrl, err := cli.OpenControl(dialCtx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	// No DeviceID — legacy v0.1 path.
	if err := wt.WriteSessionIDHeader(ctrl, sid); err != nil {
		t.Fatalf("WriteSessionIDHeader: %v", err)
	}
	events, err := cli.AcceptEvents(dialCtx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}

	for retry := 0; retry < 5; retry++ {
		delivered, err := em.Inject(agent.LocalEnvelope{
			Kind:              "output.agent-event",
			SessionID:         sid,
			ProviderType:      "claude-code",
			PlaintextMetadata: map[string]any{"i": 1},
		})
		if err != nil {
			t.Fatalf("Inject: %v", err)
		}
		if delivered == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	er, err := wt.NewEnvelopeFrameReader(events, wt.DevSharedKey[:], sid)
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader: %v", err)
	}
	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, _, err := er.Next()
		ch <- result{err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Next with DevSharedKey: %v", r.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("legacy fallback did not deliver frame within 4s")
	}
}

// TestDaemonGlue_RepeatPair_Rejects — pair once, disconnect, try to
// pair again with the same deviceId. Daemon must emit
// pair.abort{reason:dup-deviceid} (spec §14 Q2 default reject).
func TestDaemonGlue_RepeatPair_Rejects(t *testing.T) {
	t.Parallel()

	const deviceID = pair.DeviceID("device-mobile-rejecttest1")

	tmp := t.TempDir()
	pairRoot := filepath.Join(tmp, "pair-devices")
	pairAudit := filepath.Join(tmp, "audit.log")

	url, _, cancel := bootDaemonWithWT(t, daemon.Config{
		PairRegistryRoot: pairRoot,
		PairAuditLogPath: pairAudit,
	})
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer dialCancel()

	// 1. First pair — succeeds.
	cli1 := dialDaemonWT(t, dialCtx, url)
	res := runPairClient(t, dialCtx, cli1, deviceID, "FirstPair Phone")
	if res.SAS == "" {
		t.Fatalf("first pair returned empty SAS")
	}
	cli1.Close()

	// 2. Second pair with the same deviceID — daemon rejects with
	// pair.abort{dup-deviceid}. The client's pair.Client.Run surfaces
	// this as an error (it expects pair.accept, gets pair.abort).
	cli2 := dialDaemonWT(t, dialCtx, url)
	defer cli2.Close()
	err := runPairClientExpectingErr(t, dialCtx, cli2, deviceID)
	if err == nil {
		t.Fatal("second pair returned nil error; expected dup-deviceid abort")
	}
	// Verify the error mentions the reject path. We don't pin to an
	// exact string because the client may surface it via FSM/wire
	// layers; spot-check substrings.
	msg := err.Error()
	if !contains(msg, "abort") && !contains(msg, "dup") && !contains(msg, "expected pair.accept") {
		t.Errorf("error %q does not look like a dup-deviceid reject", msg)
	}

	// 3. Verify only ONE record persisted (the original pair was not
	// overwritten).
	verifyReg, err := pair.NewRegistry(pair.RegistryConfig{Root: pairRoot})
	if err != nil {
		t.Fatalf("verify registry: %v", err)
	}
	ids, err := verifyReg.List()
	if err != nil {
		t.Fatalf("registry.List: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("registry has %d records, want 1: %v", len(ids), ids)
	}
	rec, err := verifyReg.Load(deviceID)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if string(rec.LongTermKey) != string(res.LongTermKey) {
		t.Errorf("re-pair somehow overwrote ltk; first-pair=%x persisted=%x", res.LongTermKey, rec.LongTermKey)
	}
}

// contains is a tiny helper to keep the test file dependency-free
// (avoids strings.Contains import collision pattern).
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// suppress unused-import warning (filepath imported in case future
// helpers need it); cheap no-op.
var _ = fmt.Sprintf
