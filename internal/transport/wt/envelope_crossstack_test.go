package wt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

// TestCrossStack_GoServer_GoClient_EventsDispatch — the slice #3
// "real prize". Wire up:
//
//   - a real wt.Server on a loopback port (auto dev cert, channel
//     router).
//   - a SessionHandler that takes a fixed list of LocalEnvelopes and
//     pushes them via PushEnvelopeStream onto the events channel.
//   - a Go-side wt.Client that AcceptEvents + EnvelopeFrameReader
//     reads the same number of frames + verifies decrypted bodies.
//
// This is the cross-stack smoke proof that:
//
//   1. Channel-id 0x02 (events) routes properly with §3.3.1
//      length-prefixed JSON envelopes carried inside.
//   2. Sender-side Seal + receiver-side Open agree byte-exactly on
//      the AD construction.
//   3. The dispatcher cleanly exits when its input channel closes.
//
// The corresponding Rust-side cross-stack test is in tether-app's
// `wt_envelope_smoke` env-gated harness (see PR body).
func TestCrossStack_GoServer_GoClient_EventsDispatch(t *testing.T) {
	t.Parallel()

	want := []fakeLocalEnvelope{
		{Kind: "output.agent-event", SessionID: "sess-X", Body: "one"},
		{Kind: "output.agent-event", SessionID: "sess-X", Body: "two"},
		{Kind: "output.hook-event", SessionID: "sess-X", Body: "three"},
	}

	// Server-side: open events bidi, push 3 envelopes via dispatcher,
	// then close the stream-write side so the client's reader sees a
	// clean EOF at the next frame boundary. We sit on sess.Context()
	// after that so the session itself stays open while the client
	// drains — Server.handleUpgrade calls closeWithError as soon as
	// this handler returns, which would otherwise race the client's
	// AcceptEvents on slow CI.
	handler := func(sess *Session) {
		ctx, cancel := context.WithTimeout(sess.Context(), 5*time.Second)
		defer cancel()
		evStream, err := sess.Events(ctx)
		if err != nil {
			return
		}

		in := make(chan fakeLocalEnvelope, len(want))
		for _, e := range want {
			in <- e
		}
		close(in)

		_ = PushEnvelopeStream(ctx, evStream, in, PushEnvelopeOptions{
			SharedKey:    DevSharedKey[:],
			FromDeviceID: "device-cli-1",
			ToDeviceID:   "device-app-2",
		})
		// Half-close the stream so the client's reader observes EOF.
		_ = evStream.Close()

		// Block until the client tears the WT session down (or this
		// session-level context is cancelled by test teardown). Without
		// this, the handler returning closes the WT session itself,
		// which can race the client's AcceptEvents (slow CI: client may
		// not have called AcceptEvents yet when handler exits).
		<-sess.Context().Done()
	}

	url, pool, cancel := bootTestServer(t, handler)
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	stream, err := cli.AcceptEvents(ctx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}
	er, err := NewEnvelopeFrameReader(stream, DevSharedKey[:], "sess-X")
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader: %v", err)
	}

	got := make([]fakeLocalEnvelope, 0, len(want))
	for i := 0; i < len(want); i++ {
		env, pt, err := er.Next()
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		if env.KeyVersion != CurrentKeyVersion {
			t.Errorf("frame[%d] keyVersion=%d", i, env.KeyVersion)
		}
		if env.FromDeviceID != "device-cli-1" || env.ToDeviceID != "device-app-2" {
			t.Errorf("frame[%d] device IDs wrong", i)
		}
		var inner fakeLocalEnvelope
		if err := json.Unmarshal(pt, &inner); err != nil {
			t.Fatalf("frame[%d] inner unmarshal: %v", i, err)
		}
		// Match the kind round-trip too (inner kind = wire kind).
		if inner.Kind != env.Kind {
			t.Errorf("frame[%d] inner.Kind=%q != env.Kind=%q", i, inner.Kind, env.Kind)
		}
		got = append(got, inner)
	}

	// Stream should be at EOF after all envelopes consumed.
	if _, _, err := er.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after %d frames, got %v", len(want), err)
	}

	for i := range want {
		if got[i].Body != want[i].Body {
			t.Errorf("frame[%d] body=%q want %q", i, got[i].Body, want[i].Body)
		}
		if got[i].Kind != want[i].Kind {
			t.Errorf("frame[%d] kind=%q want %q", i, got[i].Kind, want[i].Kind)
		}
	}
}
