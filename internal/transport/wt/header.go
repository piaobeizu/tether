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
// SLICE #4 — pair.invite supersedes this. The migration path is:
//
//  1. slice #4 lands a richer pair.invite that includes the
//     attachToSession field (or whatever the real spec names).
//  2. The daemon's SessionHandler tries pair.invite first; falls back
//     to the v0.1 SessionIDHeader if the peer is a pre-pair build.
//  3. After all clients ship pair, this struct is deleted.
type SessionIDHeader struct {
	SessionID string `json:"sessionId"`
}

// ErrEmptySessionID is returned when the parsed header has no
// sessionId field (or empty string). Callers must reject the
// connection — without a sid we cannot subscribe to envelopes.
var ErrEmptySessionID = errors.New("wt: session id header missing sessionId")

// WriteSessionIDHeader writes the v0.1 sid header as one JSON line
// followed by '\n'. Caller is expected to have already opened a
// control stream (channel-id prefix already written by openBidi).
func WriteSessionIDHeader(w io.Writer, sid string) error {
	if sid == "" {
		return ErrEmptySessionID
	}
	body, err := json.Marshal(SessionIDHeader{SessionID: sid})
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
func ReadSessionIDHeader(r io.Reader) (string, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return readSessionIDHeader(br)
}

func readSessionIDHeader(br *bufio.Reader) (string, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		// io.EOF without a terminator is a malformed header (the peer
		// never finished writing). Wrap so callers can branch.
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return "", fmt.Errorf("wt: session id header: %w", io.ErrUnexpectedEOF)
		}
		return "", fmt.Errorf("wt: read session id header: %w", err)
	}
	var hdr SessionIDHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		return "", fmt.Errorf("wt: parse session id header: %w", err)
	}
	if hdr.SessionID == "" {
		return "", ErrEmptySessionID
	}
	return hdr.SessionID, nil
}
