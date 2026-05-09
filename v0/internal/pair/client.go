package pair

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"
)

// Result is what Client.Run / Server.Run return on `paired`. Long-term
// keys cross the API boundary (caller persists / uses them) but the
// FSM-internal shared_secret + sas_key are zeroized before return.
type Result struct {
	// LocalDeviceID / PeerDeviceID identify the two sides of this pair.
	LocalDeviceID DeviceID
	PeerDeviceID  DeviceID
	PeerKind      DeviceKind
	PeerName      string
	PeerPushToken string

	// LongTermKey is the §11.C wrap_key input. 32B.
	LongTermKey []byte
	// TransportBindingKey is reserved for v0.1.x channel-binding. 32B.
	TransportBindingKey []byte

	// TranscriptHash is the hash that anchors the audit-log line.
	TranscriptHash []byte

	// SAS is the 6-character display string both endpoints saw. Used
	// in tests + UX confirmation; not security-relevant after pairing.
	SAS string

	// LongTermKeyID is the daemon-assigned record id from pair.complete
	// (spec §3.4). Echoed back to the UI for display. Empty on the
	// responder side (the responder ASSIGNS it; the initiator OBSERVES it).
	LongTermKeyID string
}

// SASConfirmer is the user-confirm hook. The driver calls Confirm with
// the locally-derived SAS string; the implementation blocks until the
// user clicks "match" / "do not match" on whatever UI is wired and
// returns nil (match) or a non-nil error (rejection). For headless
// tests, AutoConfirm always returns nil.
type SASConfirmer interface {
	Confirm(sas string) error
}

// SASConfirmFunc adapts a function value to SASConfirmer.
type SASConfirmFunc func(sas string) error

func (f SASConfirmFunc) Confirm(sas string) error { return f(sas) }

// AutoConfirm is a SASConfirmer that always accepts. Used by tests.
var AutoConfirm SASConfirmer = SASConfirmFunc(func(string) error { return nil })

// Identity carries the local device's stable metadata that gets baked
// into pair.invite / pair.accept frames.
//
// Model / OSVersion / AppVersion are spec §3.1/§3.2 OPTIONAL deviceMetadata
// fields. They MUST be included in the canonical body (and thus the
// transcript) when non-empty. The Rust client already populates these on
// the wire (e.g. tether-app's UI passes appVersion="tether 0.1.0-dev"
// through), so the Go side needs to (a) accept them on inbound and (b)
// include them in canonical body for cross-stack transcript-hash parity.
type Identity struct {
	DeviceID    DeviceID
	Kind        DeviceKind
	DisplayName string
	// Optional spec §3.1/§3.2 deviceMetadata fields. Empty string =>
	// omit from canonical body / wire.
	Model      string
	OSVersion  string
	AppVersion string
	// PushToken is meaningful only when Kind == KindMobile (responder).
	PushToken string
}

// Client is the initiator-side driver. Concurrency: one Run call per
// Client; Run is not safe to invoke from multiple goroutines.
type Client struct {
	identity  Identity
	confirmer SASConfirmer
	now       func() time.Time
	rand      io.Reader
}

// ClientConfig is the explicit-config form of NewClient. Now lets
// tests inject a fake clock; Rand lets tests inject a deterministic
// random source for ephemeral keys + nonces.
type ClientConfig struct {
	Identity  Identity
	Confirmer SASConfirmer
	Now       func() time.Time
	Rand      io.Reader
}

// NewClient constructs an initiator-side driver. Confirmer defaults
// to AutoConfirm.
func NewClient(cfg ClientConfig) *Client {
	c := &Client{identity: cfg.Identity, confirmer: cfg.Confirmer, now: cfg.Now, rand: cfg.Rand}
	if c.confirmer == nil {
		c.confirmer = AutoConfirm
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if c.rand == nil {
		c.rand = rand.Reader
	}
	return c
}

// Run drives the initiator-side pairing flow against the given control
// stream. Returns Result on `paired`, error on any failure. The caller
// is responsible for persisting Result via Registry.Save (or
// ForceSave).
func (c *Client) Run(ctx context.Context, stream io.ReadWriter) (Result, error) {
	if err := ValidateDeviceID(c.identity.DeviceID); err != nil {
		return Result{}, err
	}
	r := newStreamReader(stream)
	w := newStreamWriter(stream)
	fsm := NewFSM(RoleInitiator)

	priv, pub, err := genX25519(c.rand)
	if err != nil {
		return Result{}, fmt.Errorf("pair: client gen ephemeral: %w", err)
	}
	defer zero(priv)

	transcript := NewTranscript()

	// 1. Compose + send pair.invite.
	invite := InviteFrame{
		ProtocolVersion: ProtocolVersion,
		InitiatorPubkey: pub,
		DeviceID:        c.identity.DeviceID,
		Kind_:           c.identity.Kind,
		DisplayName:     c.identity.DisplayName,
		Model:           c.identity.Model,
		OSVersion:       c.identity.OSVersion,
		AppVersion:      c.identity.AppVersion,
		TS_:             c.now().UnixMilli(),
		Nonce:           mustRandomNonce(c.rand, 16),
	}
	state, out, err := fsm.Step(Event{Kind: EventStartInvite, Frame: invite})
	if err != nil {
		return Result{}, err
	}
	if err := emitAll(w, out); err != nil {
		return Result{}, fmt.Errorf("pair: client send invite: %w", err)
	}
	if err := transcript.Append(invite); err != nil {
		return Result{}, err
	}

	// 2. Receive pair.accept (with awaiting-pubkey 30s timeout).
	acceptCtx, acceptCancel := context.WithTimeout(ctx, TimeoutAwaitingPubkey)
	frame, err := readFrameWithCtx(acceptCtx, r)
	acceptCancel()
	if err != nil {
		// Timeout / IO error → emit pair.abort{timeout|protocol-violation}.
		ab := abortNow(reasonForCtxErr(err), err.Error())
		ab.TS_ = c.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: client recv accept: %w", err)
	}
	state, _, err = fsm.Step(Event{Kind: EventRecvFrame, Frame: frame})
	if err != nil {
		return Result{}, err
	}
	accept, ok := frame.(AcceptFrame)
	if !ok {
		return Result{}, fmt.Errorf("pair: expected pair.accept, got %T", frame)
	}
	if err := transcript.Append(accept); err != nil {
		return Result{}, err
	}

	// 3. Derive shared secret + sas_key + display SAS.
	shared, err := ComputeSharedSecret(priv, accept.ResponderPubkey)
	if err != nil {
		return Result{}, fmt.Errorf("pair: client ECDH: %w", err)
	}
	defer zero(shared)
	thash := transcript.Hash()
	sasKey, err := DeriveSASKey(shared, thash)
	if err != nil {
		return Result{}, err
	}
	defer zero(sasKey)
	sas, err := ComputeSAS(sasKey)
	if err != nil {
		return Result{}, err
	}

	// 4. User confirms SAS.
	if err := c.confirmer.Confirm(sas); err != nil {
		ab := abortNow(ReasonSASMismatch, err.Error())
		ab.TS_ = c.now().UnixMilli()
		_, out, _ = fsm.Step(Event{Kind: EventUserRejectsSAS, Frame: ab})
		_ = emitAll(w, out)
		return Result{}, fmt.Errorf("pair: user rejected SAS: %w", err)
	}

	// 5. Emit our pair.sas-confirm{ok:true,role:initiator}.
	myMAC := ConfirmMAC(sasKey, RoleInitiator, thash)
	myConfirm := SASConfirmFrame{
		ProtocolVersion: ProtocolVersion,
		OK:              true,
		Role:            RoleInitiator,
		MAC:             myMAC,
		TS_:             c.now().UnixMilli(),
	}
	state, out, err = fsm.Step(Event{Kind: EventUserConfirmsSAS, Frame: myConfirm})
	if err != nil {
		return Result{}, err
	}
	if err := emitAll(w, out); err != nil {
		return Result{}, fmt.Errorf("pair: client send sas-confirm: %w", err)
	}
	if err := transcript.Append(myConfirm); err != nil {
		return Result{}, err
	}

	// 6. Receive peer's pair.sas-confirm. After step (5) the FSM is
	//    already in completing for the initiator role, but the peer's
	//    sas-confirm is allowed in BOTH sas-confirm AND completing
	//    (driver responsibility — the FSM matrix only allows it in
	//    sas-confirm, so we defer the FSM step until we have the
	//    peer's MAC verified, then synthesize a no-op state push).
	confirmCtx, confirmCancel := context.WithTimeout(ctx, TimeoutSASConfirm)
	frame, err = readFrameWithCtx(confirmCtx, r)
	confirmCancel()
	if err != nil {
		ab := abortNow(reasonForCtxErr(err), err.Error())
		ab.TS_ = c.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: client recv peer sas-confirm: %w", err)
	}
	peerConfirm, ok := frame.(SASConfirmFrame)
	if !ok {
		ab := abortNow(ReasonProtocolViolation, fmt.Sprintf("expected sas-confirm, got %s", frame.Kind()))
		ab.TS_ = c.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: expected pair.sas-confirm, got %T", frame)
	}
	if !peerConfirm.OK {
		return Result{}, ErrSASMismatch
	}
	if peerConfirm.Role != RoleResponder {
		return Result{}, fmt.Errorf("pair: peer sas-confirm role mismatch %q", peerConfirm.Role)
	}
	if err := VerifyConfirmMAC(sasKey, RoleResponder, thash, peerConfirm.MAC); err != nil {
		ab := abortNow(ReasonSASMismatch, "peer MAC mismatch")
		ab.TS_ = c.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, err
	}
	if err := transcript.Append(peerConfirm); err != nil {
		return Result{}, err
	}
	_ = state // FSM is now in completing (initiator)

	// 7. Receive pair.complete (10s timeout). Server-driven path
	//    where the responder's daemon emits the ack — see server.go.
	completeCtx, completeCancel := context.WithTimeout(ctx, TimeoutCompleting)
	frame, err = readFrameWithCtx(completeCtx, r)
	completeCancel()
	if err != nil {
		ab := abortNow(reasonForCtxErr(err), err.Error())
		ab.TS_ = c.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: client recv complete: %w", err)
	}
	if _, _, err := fsm.Step(Event{Kind: EventRecvFrame, Frame: frame}); err != nil {
		return Result{}, err
	}
	complete, ok := frame.(CompleteFrame)
	if !ok {
		return Result{}, fmt.Errorf("pair: expected pair.complete, got %T", frame)
	}

	// 8. Derive long-term keys, then verify the §3.4 AEAD tag. A rogue
	//    daemon (or any in-path actor that survived TLS) cannot forge
	//    this tag without sharing our derived ltk + the same
	//    transcript_hash. Mismatch ⇒ pair.abort{cert-error}, do NOT
	//    persist the registry record.
	ltk, tbk, err := DeriveLongTermKey(shared, thash)
	if err != nil {
		return Result{}, err
	}
	if err := OpenCompleteTag(ltk, thash, complete.Nonce, complete.Tag); err != nil {
		ab := abortNow(ReasonCertError, "pair.complete AEAD tag verification failed")
		ab.TS_ = c.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, err
	}
	return Result{
		LocalDeviceID:       c.identity.DeviceID,
		PeerDeviceID:        accept.DeviceID,
		PeerKind:            accept.Kind_,
		PeerName:            accept.DisplayName,
		PeerPushToken:       accept.PushToken,
		LongTermKey:         ltk,
		TransportBindingKey: tbk,
		TranscriptHash:      thash,
		SAS:                 sas,
		LongTermKeyID:       complete.LongTermKeyID,
	}, nil
}

// emitAll writes every frame in out to the wire. The first error
// short-circuits.
func emitAll(w *streamWriter, out []Frame) error {
	for _, f := range out {
		if err := w.writeFrame(f); err != nil {
			return err
		}
	}
	return nil
}

// reasonForCtxErr maps a context.DeadlineExceeded → ReasonTimeout,
// anything else → ReasonProtocolViolation. Used by drivers when an
// IO read fails.
func reasonForCtxErr(err error) AbortReason {
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}
	return ReasonProtocolViolation
}

// genX25519 produces an ephemeral keypair. Wraps internal/crypto's
// generator but takes an io.Reader so tests can inject determinism.
func genX25519(r io.Reader) (priv, pub []byte, err error) {
	priv = make([]byte, 32)
	if _, err := io.ReadFull(r, priv); err != nil {
		return nil, nil, fmt.Errorf("pair: read private: %w", err)
	}
	pub, err = basepointMulPub(priv)
	if err != nil {
		return nil, nil, err
	}
	return priv, pub, nil
}

// mustRandomNonce reads n random bytes from r. The driver code paths
// that use it would not function with a corrupt random source, so a
// failure here means something has gone catastrophically wrong (e.g.
// the OS entropy source).
func mustRandomNonce(r io.Reader, n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		// No way to recover; return zeros and let the peer reject the
		// resulting transcript via SAS divergence (defense in depth).
		// Real callers pass crypto/rand which doesn't fail.
		return b
	}
	return b
}

// zero overwrites b with zeros. Used to scrub keys + secrets from
// stack/heap on driver exit.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
