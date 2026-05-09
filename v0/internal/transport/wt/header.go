// header.go — v0.1 control-channel session-id header.
//
// PLACEHOLDER — slice #4 (pairing) replaces this with the real
// pair.invite handshake (see internal/pair/* and tether-doc §11.AB).
//
// Why a placeholder? The wt package on its own has no way to learn
// "which logical cc session does this WT connection map to?". The
// daemon-side SessionHandler needs a sid before it can call
// emitter.Subscribe(sid) and start pushing envelopes onto the events
// stream. v0.1 sends a tiny pre-protocol JSON line on the control
// channel that carries `{sessionId: "..."}\n`. The pair handshake will
// extend the same control channel with a richer attach payload, at
// which point this file goes away (or shrinks to a deprecated
// fallback).
//
// Wire shape:
//
//	[1 byte channel-id 0x01] [JSON line: {"sessionId":"..."} \n]
//
// The 1-byte prefix is the standard channel-id (consumed by the accept
// loop before the SessionHandler ever sees the stream). After that,
// the SessionHandler reads exactly one line of JSON, parses the
// sessionId field, and starts dispatch. Extra bytes after the newline
// stay on the stream for the pair handshake to consume in slice #4.

package wt

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SessionIDHeader is the v0.1 control-channel header carrying the cc
// session id the WT session maps to. JSON-marshaled as a single line
// terminated by '\n' on the control stream.
//
// SLICE #4 GLUE — DeviceID added as an OPTIONAL field. After a peer
// completes the pair handshake (see internal/pair) it reconnects and
// announces "I am device X" via this field; the daemon's
// SessionHandler then resolves the per-session AEAD key from
// pair.Registry by deviceId. Peers that haven't paired (legacy / dev /
// cross-stack tests using DevSharedKey) simply omit the field, and the
// daemon falls back to wt.DevSharedKey[:].
//
// Migration story (after slice #4 ships):
//
//  1. SessionHandler peeks the first JSON line on the control channel.
//     If it parses as a pair envelope (kind=pair.invite, keyVersion=0)
//     the handler runs pair.Server.Run to completion and returns —
//     pair clients reconnect for the actual session post-pairing.
//  2. Otherwise the line parses as SessionIDHeader; if DeviceID is
//     non-empty, look up the long-term key; if empty, fall back to
//     DevSharedKey.
//  3. After all clients ship pair + announce DeviceID, DevSharedKey
//     fallback is deleted.
type SessionIDHeader struct {
	SessionID string `json:"sessionId"`
	// DeviceID, when non-empty, identifies the paired device this WT
	// session belongs to. The daemon resolves the per-device long-term
	// key from pair.Registry. Empty (legacy / unpaired peers) falls
	// back to wt.DevSharedKey[:]. Slice #5 (Rust-side cutover) writes
	// this field after the Tauri pair-completion path persists the
	// long-term key into its own registry.
	DeviceID string `json:"deviceId,omitempty"`
}

// ErrEmptySessionID is returned when the parsed header has no
// sessionId field (or empty string). Callers must reject the
// connection — without a sid we cannot subscribe to envelopes.
var ErrEmptySessionID = errors.New("wt: session id header missing sessionId")

// WriteSessionIDHeader writes the v0.1 sid header as one JSON line
// followed by '\n'. Caller is expected to have already opened a
// control stream (channel-id prefix already written by openBidi).
//
// DeviceID is omitted (legacy v0.1 path). Callers that have completed
// the pair handshake should use WriteSessionHeader(w, hdr) so the
// daemon can look up the paired long-term key from pair.Registry.
func WriteSessionIDHeader(w io.Writer, sid string) error {
	return WriteSessionHeader(w, SessionIDHeader{SessionID: sid})
}

// WriteSessionHeader writes the full SessionIDHeader (including
// optional DeviceID) as a single JSON line.
func WriteSessionHeader(w io.Writer, hdr SessionIDHeader) error {
	if hdr.SessionID == "" {
		return ErrEmptySessionID
	}
	body, err := json.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("wt: marshal session id header: %w", err)
	}
	body = append(body, '\n')
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("wt: write session id header: %w", err)
	}
	return nil
}

// ReadSessionIDHeader reads exactly one '\n'-terminated JSON line from
// r and parses out the sessionId. The reader is wrapped in a
// bufio.Reader internally; callers that want to keep reading the
// underlying stream after the header should pass the bufio.Reader
// directly (see *bufio.Reader-typed overload below).
//
// Returns ErrEmptySessionID if the parsed sessionId is empty. Other
// errors (malformed JSON, EOF mid-line) are returned wrapped.
//
// DeviceID is silently dropped — legacy callers don't need it. Use
// ReadSessionHeader for the full struct.
func ReadSessionIDHeader(r io.Reader) (string, error) {
	hdr, err := ReadSessionHeader(r)
	if err != nil {
		return "", err
	}
	return hdr.SessionID, nil
}

// ReadSessionHeader is the slice-#4 form: returns the full header
// struct so callers can resolve a per-device key via DeviceID. Same
// error contract as ReadSessionIDHeader.
func ReadSessionHeader(r io.Reader) (SessionIDHeader, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return readSessionHeader(br)
}

// ParseSessionHeaderLine parses a header from a single JSON line
// already extracted from the stream. Used by the daemon's first-frame
// router when it has peeked the leading line to disambiguate
// pair.invite vs SessionIDHeader.
func ParseSessionHeaderLine(line []byte) (SessionIDHeader, error) {
	var hdr SessionIDHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		return SessionIDHeader{}, fmt.Errorf("wt: parse session id header: %w", err)
	}
	if hdr.SessionID == "" {
		return SessionIDHeader{}, ErrEmptySessionID
	}
	return hdr, nil
}

func readSessionHeader(br *bufio.Reader) (SessionIDHeader, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		// io.EOF without a terminator is a malformed header (the peer
		// never finished writing). Wrap so callers can branch.
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return SessionIDHeader{}, fmt.Errorf("wt: session id header: %w", io.ErrUnexpectedEOF)
		}
		return SessionIDHeader{}, fmt.Errorf("wt: read session id header: %w", err)
	}
	return ParseSessionHeaderLine(line)
}
