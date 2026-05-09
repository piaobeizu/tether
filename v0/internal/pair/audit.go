package pair

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditKind enumerates the pair-protocol audit events. They share the
// per-user audit.log file with §11.D lock events; pair events use the
// "pair." prefix.
type AuditKind string

const (
	AuditPairSuccess        AuditKind = "pair.success"
	AuditPairFail           AuditKind = "pair.fail"
	AuditPairRepairRejected AuditKind = "pair.repair-rejected"
	AuditPairForceRotated   AuditKind = "pair.force-rotated"
)

// AuditEvent is the on-disk shape of one JSONL line. Fields are
// minimized to what slice #4 actually surfaces; the §10.3 schema
// allows a richer "details" object — we shape that on a per-kind basis.
type AuditEvent struct {
	TS                  string    `json:"ts"`
	Kind                AuditKind `json:"kind"`
	DeviceID            DeviceID  `json:"deviceId,omitempty"`
	PeerDeviceID        DeviceID  `json:"peerDeviceId,omitempty"`
	LongTermKeyID       string    `json:"longTermKeyId,omitempty"`
	PrevLongTermKeyID   string    `json:"previousLongTermKeyId,omitempty"`
	Reason              string    `json:"reason,omitempty"`
	Detail              string    `json:"detail,omitempty"`
	TranscriptHashB64Url string   `json:"transcript_hash,omitempty"`
}

// AuditLog is an append-only JSONL writer mirroring lock.JSONLLogSink.
// Per spec §10.3 + §11.D it lives at
// `~/.tether/users/<user>/audit.log`. Mode 0600. Parent dirs 0700.
type AuditLog struct {
	path string

	mu     sync.Mutex
	f      *os.File
	closed bool
	now    func() time.Time
}

// AuditLogConfig is the explicit-config form of NewAuditLog. Path is
// the destination file (caller resolves the §10.3 layout). Now lets
// tests inject a fake clock — defaults to time.Now if nil.
type AuditLogConfig struct {
	Path string
	Now  func() time.Time
}

// NewAuditLog opens (or creates) an audit log at path. Parent dirs are
// created at 0700. The file is O_APPEND|O_CREATE|O_WRONLY at 0600.
func NewAuditLog(path string) (*AuditLog, error) {
	return NewAuditLogWithConfig(AuditLogConfig{Path: path})
}

// NewAuditLogWithConfig is the explicit-config form.
func NewAuditLogWithConfig(cfg AuditLogConfig) (*AuditLog, error) {
	if cfg.Path == "" {
		return nil, errors.New("pair: AuditLog: empty path")
	}
	dir := filepath.Dir(cfg.Path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("pair: mkdir audit dir %q: %w", dir, err)
		}
		_ = os.Chmod(dir, 0o700)
	}
	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pair: open audit log %q: %w", cfg.Path, err)
	}
	_ = os.Chmod(cfg.Path, 0o600)
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AuditLog{path: cfg.Path, f: f, now: now}, nil
}

// Path returns the file path the audit log writes to.
func (a *AuditLog) Path() string { return a.path }

// AppendPairEvent writes one event line. Caller fills the AuditEvent
// fields except TS, which AppendPairEvent overrides with now().
func (a *AuditLog) AppendPairEvent(ev AuditEvent) error {
	if ev.Kind == "" {
		return errors.New("pair: audit event missing kind")
	}
	ev.TS = a.now().UTC().Format(time.RFC3339Nano)
	row, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("pair: marshal audit row: %w", err)
	}
	row = append(row, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.f == nil {
		return errors.New("pair: audit log already closed")
	}
	if _, err := a.f.Write(row); err != nil {
		return fmt.Errorf("pair: append audit %q: %w", a.path, err)
	}
	if err := a.f.Sync(); err != nil {
		return fmt.Errorf("pair: fsync audit %q: %w", a.path, err)
	}
	return nil
}

// Convenience helpers for the four pair-event kinds.

// AppendSuccess records pair.success with the transcript_hash anchor
// per §10.3.
func (a *AuditLog) AppendSuccess(deviceID, peer DeviceID, ltkID string, transcriptHash []byte) error {
	return a.AppendPairEvent(AuditEvent{
		Kind:                 AuditPairSuccess,
		DeviceID:             deviceID,
		PeerDeviceID:         peer,
		LongTermKeyID:        ltkID,
		TranscriptHashB64Url: b64uEncode(transcriptHash),
	})
}

// AppendFail records pair.fail with the abort reason / detail.
func (a *AuditLog) AppendFail(deviceID DeviceID, reason AbortReason, detail string) error {
	return a.AppendPairEvent(AuditEvent{
		Kind:     AuditPairFail,
		DeviceID: deviceID,
		Reason:   string(reason),
		Detail:   detail,
	})
}

// AppendRepairRejected records pair.repair-rejected when an inbound
// invite would have re-paired an existing deviceId.
func (a *AuditLog) AppendRepairRejected(deviceID DeviceID) error {
	return a.AppendPairEvent(AuditEvent{
		Kind:     AuditPairRepairRejected,
		DeviceID: deviceID,
	})
}

// AppendForceRotated records pair.force-rotated when a Registry
// ForceSave call overwrites an existing record.
func (a *AuditLog) AppendForceRotated(deviceID DeviceID, newLTKID, prevLTKID string) error {
	return a.AppendPairEvent(AuditEvent{
		Kind:              AuditPairForceRotated,
		DeviceID:          deviceID,
		LongTermKeyID:     newLTKID,
		PrevLongTermKeyID: prevLTKID,
	})
}

// Close releases the underlying file handle. Idempotent.
func (a *AuditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}
