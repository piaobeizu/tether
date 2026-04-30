package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrRecoverInterrupted is reported to all pending outbound control_requests
// when Controller.AbortPending is called (spec §6.C.6).
var ErrRecoverInterrupted = errors.New("control: recover interrupted pending request")

// PermissionBehavior is the choice returned by a ToolAuthorizer.
type PermissionBehavior string

const (
	PermissionAllow PermissionBehavior = "allow"
	PermissionDeny  PermissionBehavior = "deny"
)

// PermissionResult is what a ToolAuthorizer returns. For "allow" the caller
// may also rewrite the tool's Input by setting Updated; if Updated is empty,
// the original input is passed through unchanged.
type PermissionResult struct {
	Behavior PermissionBehavior
	Updated  json.RawMessage // optional rewritten input for "allow"
	Message  string          // user-facing reason for "deny"
}

// ToolAuthorizer decides whether cc may invoke a given tool.
//
// Returning a non-nil error causes Controller to write a control_response
// of subtype=error back to cc. PermissionDeny is for the "tool was correctly
// requested but the user said no" case.
type ToolAuthorizer interface {
	CanUseTool(ctx context.Context, toolName string, input json.RawMessage) (PermissionResult, error)
}

// ToolAuthorizerFunc adapts a function to the ToolAuthorizer interface.
type ToolAuthorizerFunc func(ctx context.Context, toolName string, input json.RawMessage) (PermissionResult, error)

// CanUseTool implements ToolAuthorizer.
func (f ToolAuthorizerFunc) CanUseTool(ctx context.Context, toolName string, input json.RawMessage) (PermissionResult, error) {
	return f(ctx, toolName, input)
}

// Controller multiplexes control_request / control_response over a single
// subprocess stdin. Two responsibilities:
//
//  1. Inbound (cc → tether): when Dispatch is given an Envelope of type
//     control_request, route can_use_tool to ToolAuthorizer; for unknown
//     subtypes write a graceful-degrade empty success ack (spec §6.A.2).
//  2. Outbound (tether → cc): SendInterrupt etc. issue a control_request
//     with a fresh request_id, register a pending handler, and block (until
//     ctx-cancel) for the matching inbound control_response.
//
// stdin writes are serialized via a mutex so NDJSON lines never interleave.
type Controller struct {
	stdin io.Writer
	auth  ToolAuthorizer

	stdinMu sync.Mutex
	pendMu  sync.Mutex
	pending map[string]chan ControlResponseBody

	logf func(format string, args ...any)
}

// NewController wires a Controller. auth may be nil — in that case all
// inbound can_use_tool requests are denied with an explanatory message.
func NewController(stdin io.Writer, auth ToolAuthorizer) *Controller {
	return &Controller{
		stdin:   stdin,
		auth:    auth,
		pending: make(map[string]chan ControlResponseBody),
		logf:    func(string, ...any) {},
	}
}

// SetLogger installs a debug logger. Optional.
func (c *Controller) SetLogger(logf func(format string, args ...any)) {
	c.logf = logf
}

// Dispatch routes an inbound Envelope to the controller. Returns true if
// the envelope was a control message (handled here); false means "not mine,
// caller routes it elsewhere".
func (c *Controller) Dispatch(ctx context.Context, env Envelope) bool {
	switch env.Type {
	case EventControlRequest:
		c.handleInbound(ctx, env)
		return true
	case EventControlResponse:
		c.routeResponse(env)
		return true
	default:
		return false
	}
}

func (c *Controller) handleInbound(ctx context.Context, env Envelope) {
	cr, err := env.DecodeControlRequest()
	if err != nil {
		c.logf("control: decode inbound: %v", err)
		return
	}
	subtype, _ := cr.RequestSubtype()
	switch subtype {
	case "can_use_tool":
		c.handleCanUseTool(ctx, cr)
	default:
		// A.2 graceful-degrade: ack unknown subtypes so cc isn't blocked.
		c.logf("control: unknown subtype %q request_id=%s — sending empty success ack",
			subtype, cr.RequestID)
		_ = c.writeResponse(ControlResponseBody{
			Subtype:   "success",
			RequestID: cr.RequestID,
		})
	}
}

func (c *Controller) handleCanUseTool(ctx context.Context, cr ControlRequest) {
	var payload CanUseToolPayload
	if err := json.Unmarshal(cr.Request, &payload); err != nil {
		c.logf("control: can_use_tool decode: %v", err)
		_ = c.writeResponse(ControlResponseBody{
			Subtype:   "error",
			RequestID: cr.RequestID,
			Error:     err.Error(),
		})
		return
	}

	if c.auth == nil {
		_ = c.writeResponse(ControlResponseBody{
			Subtype:   "success",
			RequestID: cr.RequestID,
			Response: mustMarshal(map[string]any{
				"behavior": string(PermissionDeny),
				"message":  "no ToolAuthorizer configured",
			}),
		})
		return
	}

	res, err := c.auth.CanUseTool(ctx, payload.ToolName, payload.Input)
	if err != nil {
		_ = c.writeResponse(ControlResponseBody{
			Subtype:   "error",
			RequestID: cr.RequestID,
			Error:     err.Error(),
		})
		return
	}

	respPayload := map[string]any{"behavior": string(res.Behavior)}
	switch res.Behavior {
	case PermissionAllow:
		if len(res.Updated) > 0 {
			respPayload["updated_input"] = res.Updated
		} else {
			respPayload["updated_input"] = payload.Input
		}
	case PermissionDeny:
		respPayload["message"] = res.Message
	}
	_ = c.writeResponse(ControlResponseBody{
		Subtype:   "success",
		RequestID: cr.RequestID,
		Response:  mustMarshal(respPayload),
	})
}

// routeResponse hands an inbound control_response to the pending outbound
// caller keyed by RequestID. Unmatched responses are dropped (logged).
func (c *Controller) routeResponse(env Envelope) {
	cr, err := env.DecodeControlResponse()
	if err != nil {
		c.logf("control: decode response: %v", err)
		return
	}

	c.pendMu.Lock()
	ch, ok := c.pending[cr.Response.RequestID]
	if ok {
		delete(c.pending, cr.Response.RequestID)
	}
	c.pendMu.Unlock()

	if !ok {
		c.logf("control: response for unknown request_id %q dropped", cr.Response.RequestID)
		return
	}
	select {
	case ch <- cr.Response:
	default:
		// channel is buffered=1; if it's full, the caller already left.
	}
}

// SendInterrupt issues an outbound interrupt control_request and waits for
// the matching control_response. Returns ctx.Err() on cancellation; the
// pending entry is cleaned up on every exit path.
func (c *Controller) SendInterrupt(ctx context.Context) (ControlResponseBody, error) {
	return c.outbound(ctx, json.RawMessage(`{"subtype":"interrupt"}`))
}

func (c *Controller) outbound(ctx context.Context, request json.RawMessage) (ControlResponseBody, error) {
	id, err := newRequestID()
	if err != nil {
		return ControlResponseBody{}, fmt.Errorf("request_id: %w", err)
	}

	ch := make(chan ControlResponseBody, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	req := OutboundControlRequest{
		Type:      "control_request",
		RequestID: id,
		Request:   request,
	}
	if err := c.writeJSON(req); err != nil {
		return ControlResponseBody{}, fmt.Errorf("write control_request: %w", err)
	}

	select {
	case body := <-ch:
		if body.Subtype == "error" {
			return body, fmt.Errorf("control error: %s", body.Error)
		}
		return body, nil
	case <-ctx.Done():
		return ControlResponseBody{}, ctx.Err()
	}
}

// AbortPending closes all in-flight outbound requests with err. Used by
// Session.Recover() (spec §6.C.6) before SIGKILL'ing the subprocess so
// pending callers don't block forever waiting for a dead process to reply.
//
// If err is nil, ErrRecoverInterrupted is used.
func (c *Controller) AbortPending(err error) {
	if err == nil {
		err = ErrRecoverInterrupted
	}
	c.pendMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan ControlResponseBody)
	c.pendMu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- ControlResponseBody{Subtype: "error", Error: err.Error()}:
		default:
		}
	}
}

// PendingCount reports the number of in-flight outbound requests. For
// tests / metrics — should be 0 after AbortPending.
func (c *Controller) PendingCount() int {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	return len(c.pending)
}

func (c *Controller) writeResponse(body ControlResponseBody) error {
	return c.writeJSON(OutboundControlResponse{
		Type:     "control_response",
		Response: body,
	})
}

// writeJSON is the only place we write to stdin — guarded by stdinMu so
// concurrent outbound + inbound writes don't interleave bytes mid-line.
func (c *Controller) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()
	_, err = c.stdin.Write(b)
	return err
}

func newRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Permission payloads are simple maps that should always marshal;
		// hitting this is a programming error.
		panic(fmt.Sprintf("control: mustMarshal: %v", err))
	}
	return b
}
