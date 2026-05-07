package pair

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"
)

// Server is the responder-side driver — the device that receives the
// invite (typically mobile, scanning a QR / receiving an inbound).
//
// The slice-#4 daemon-relay role described in spec §2.4 ("daemon emits
// pair.complete after observing both endpoints' SAS-confirms") is
// modeled here as: the responder side is the one that emits
// pair.complete after sending its own pair.sas-confirm. This keeps the
// 2-actor test surface (client + server, no daemon as a third process)
// — the daemon-as-relay split lands in a follow-up PR after this and
// daemon-wt-wire merge.
type Server struct {
	identity  Identity
	confirmer SASConfirmer
	now       func() time.Time
	rand      io.Reader

	registry *Registry
	audit    *AuditLog
}

// ServerConfig is the explicit-config form of NewServer. Registry and
// Audit may be nil for tests that only care about the protocol output.
type ServerConfig struct {
	Identity  Identity
	Confirmer SASConfirmer
	Now       func() time.Time
	Rand      io.Reader

	Registry *Registry
	Audit    *AuditLog
}

// NewServer constructs a responder-side driver. Confirmer defaults to
// AutoConfirm.
func NewServer(cfg ServerConfig) *Server {
	s := &Server{
		identity:  cfg.Identity,
		confirmer: cfg.Confirmer,
		now:       cfg.Now,
		rand:      cfg.Rand,
		registry:  cfg.Registry,
		audit:     cfg.Audit,
	}
	if s.confirmer == nil {
		s.confirmer = AutoConfirm
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.rand == nil {
		s.rand = rand.Reader
	}
	return s
}

// Run drives the responder-side pairing flow. Returns Result on
// `paired`. If a Registry was configured, a record is persisted before
// return; if an AuditLog was configured, the appropriate
// pair.success / pair.fail line is appended.
func (s *Server) Run(ctx context.Context, stream io.ReadWriter) (Result, error) {
	if err := ValidateDeviceID(s.identity.DeviceID); err != nil {
		return Result{}, err
	}
	r := newStreamReader(stream)
	w := newStreamWriter(stream)
	fsm := NewFSM(RoleResponder)

	priv, pub, err := genX25519(s.rand)
	if err != nil {
		return Result{}, fmt.Errorf("pair: server gen ephemeral: %w", err)
	}
	defer zero(priv)

	transcript := NewTranscript()

	// 1. Receive pair.invite (no timeout — this is the signal that the
	//    responder side is engaged in the first place; daemons that
	//    want to bound idle should wrap ctx with their own deadline).
	frame, err := readFrameWithCtx(ctx, r)
	if err != nil {
		return Result{}, fmt.Errorf("pair: server recv invite: %w", err)
	}
	if _, _, err := fsm.Step(Event{Kind: EventRecvFrame, Frame: frame}); err != nil {
		return s.failAudit(s.identity.DeviceID, ReasonProtocolViolation, err.Error()), err
	}
	invite, ok := frame.(InviteFrame)
	if !ok {
		return Result{}, fmt.Errorf("pair: server expected pair.invite, got %T", frame)
	}
	if err := transcript.Append(invite); err != nil {
		return Result{}, err
	}

	// 1.b. Re-pair check: if a record already exists for invite.DeviceID,
	//      and registry is configured, abort with dup-deviceid (spec
	//      §10.1 default path). The registry check itself returns
	//      ErrAlreadyPaired on Save; we surface that *before* doing
	//      ECDH so we don't waste the ephemeral.
	if s.registry != nil {
		if _, err := s.registry.Load(invite.DeviceID); err == nil {
			ab := abortNow(ReasonDupDeviceID, fmt.Sprintf("deviceId %q already paired", invite.DeviceID))
			ab.TS_ = s.now().UnixMilli()
			_ = w.writeFrame(ab)
			if s.audit != nil {
				_ = s.audit.AppendRepairRejected(invite.DeviceID)
			}
			return Result{}, fmt.Errorf("pair: %w: %s", ErrAlreadyPaired, invite.DeviceID)
		}
	}

	// 2. Compose + send pair.accept.
	accept := AcceptFrame{
		ProtocolVersion: ProtocolVersion,
		ResponderPubkey: pub,
		DeviceID:        s.identity.DeviceID,
		Kind_:           s.identity.Kind,
		DisplayName:     s.identity.DisplayName,
		PushToken:       s.identity.PushToken,
		TS_:             s.now().UnixMilli(),
		Nonce:           mustRandomNonce(s.rand, 16),
	}
	if err := w.writeFrame(accept); err != nil {
		return Result{}, fmt.Errorf("pair: server send accept: %w", err)
	}
	// Responder transitions awaiting-pubkey → sas-confirm by virtue
	// of having emitted accept. The FSM expresses this via a manual
	// state push (the FSM's allowed-inbound matrix doesn't model this
	// path because it's a self-driven step).
	fsm.state = StateSASConfirm
	if err := transcript.Append(accept); err != nil {
		return Result{}, err
	}

	// 3. Derive shared secret + sas_key + display SAS.
	shared, err := ComputeSharedSecret(priv, invite.InitiatorPubkey)
	if err != nil {
		return Result{}, fmt.Errorf("pair: server ECDH: %w", err)
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

	// 4. Receive peer's pair.sas-confirm (60s).
	confirmCtx, confirmCancel := context.WithTimeout(ctx, TimeoutSASConfirm)
	frame, err = readFrameWithCtx(confirmCtx, r)
	confirmCancel()
	if err != nil {
		ab := abortNow(reasonForCtxErr(err), err.Error())
		ab.TS_ = s.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: server recv peer sas-confirm: %w", err)
	}
	peerConfirm, ok := frame.(SASConfirmFrame)
	if !ok {
		ab := abortNow(ReasonProtocolViolation, fmt.Sprintf("expected sas-confirm, got %s", frame.Kind()))
		ab.TS_ = s.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: expected pair.sas-confirm, got %T", frame)
	}
	if !peerConfirm.OK {
		return Result{}, ErrSASMismatch
	}
	if peerConfirm.Role != RoleInitiator {
		ab := abortNow(ReasonProtocolViolation, fmt.Sprintf("peer sas-confirm role=%q", peerConfirm.Role))
		ab.TS_ = s.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, fmt.Errorf("pair: peer sas-confirm role mismatch %q", peerConfirm.Role)
	}
	if err := VerifyConfirmMAC(sasKey, RoleInitiator, thash, peerConfirm.MAC); err != nil {
		ab := abortNow(ReasonSASMismatch, "peer MAC mismatch")
		ab.TS_ = s.now().UnixMilli()
		_ = w.writeFrame(ab)
		return Result{}, err
	}
	if err := transcript.Append(peerConfirm); err != nil {
		return Result{}, err
	}

	// 5. Local user confirms SAS.
	if err := s.confirmer.Confirm(sas); err != nil {
		ab := abortNow(ReasonSASMismatch, err.Error())
		ab.TS_ = s.now().UnixMilli()
		_ = w.writeFrame(ab)
		if s.audit != nil {
			_ = s.audit.AppendFail(s.identity.DeviceID, ReasonSASMismatch, err.Error())
		}
		return Result{}, fmt.Errorf("pair: user rejected SAS: %w", err)
	}

	// 6. Emit our pair.sas-confirm{ok:true,role:responder}.
	myMAC := ConfirmMAC(sasKey, RoleResponder, thash)
	myConfirm := SASConfirmFrame{
		ProtocolVersion: ProtocolVersion,
		OK:              true,
		Role:            RoleResponder,
		MAC:             myMAC,
		TS_:             s.now().UnixMilli(),
	}
	if err := w.writeFrame(myConfirm); err != nil {
		return Result{}, fmt.Errorf("pair: server send sas-confirm: %w", err)
	}
	if err := transcript.Append(myConfirm); err != nil {
		return Result{}, err
	}

	// 7. Emit pair.complete (server-side ack per spec §2.4 daemon role
	//    — responder + daemon co-located in v0.1).
	complete := CompleteFrame{
		ProtocolVersion: ProtocolVersion,
		TS_:             s.now().UnixMilli(),
	}
	if err := w.writeFrame(complete); err != nil {
		return Result{}, fmt.Errorf("pair: server send complete: %w", err)
	}
	fsm.state = StatePaired

	// 8. Derive long-term keys + persist + audit.
	ltk, tbk, err := DeriveLongTermKey(shared, thash)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		LocalDeviceID:       s.identity.DeviceID,
		PeerDeviceID:        invite.DeviceID,
		PeerKind:            invite.Kind_,
		PeerName:            invite.DisplayName,
		LongTermKey:         ltk,
		TransportBindingKey: tbk,
		TranscriptHash:      thash,
		SAS:                 sas,
	}
	if s.registry != nil {
		rec := DeviceRecord{
			V:                   1,
			DeviceID:            invite.DeviceID,
			Kind:                invite.Kind_,
			DisplayName:         invite.DisplayName,
			LongTermKey:         ltk,
			TransportBindingKey: tbk,
			LongTermKeyID:       fmt.Sprintf("ltk-%s", s.now().UTC().Format("20060102-150405")),
			PairedAt:            s.now().UTC(),
			LastSeen:            s.now().UTC(),
		}
		if err := s.registry.Save(rec); err != nil {
			// Re-pair race: someone else paired this id between our
			// pre-flight check and now. Surface the error; caller
			// decides whether to ForceSave.
			return res, fmt.Errorf("pair: server persist: %w", err)
		}
	}
	if s.audit != nil {
		_ = s.audit.AppendSuccess(s.identity.DeviceID, invite.DeviceID, "", thash)
	}
	return res, nil
}

// failAudit logs a pair.fail event (best-effort) and returns a zero
// Result to caller. Used at error paths where we still want to record
// the attempt before unwinding.
func (s *Server) failAudit(id DeviceID, reason AbortReason, detail string) Result {
	if s.audit != nil {
		_ = s.audit.AppendFail(id, reason, detail)
	}
	return Result{}
}
