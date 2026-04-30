package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper: create a Controller wired to a bytes.Buffer for stdin captures.
func newTestController(auth ToolAuthorizer) (*Controller, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	c := NewController(buf, auth)
	return c, buf
}

// Inbound can_use_tool — authorizer returns Allow → outbound control_response success/allow.
func TestController_Inbound_CanUseTool_Allow(t *testing.T) {
	auth := ToolAuthorizerFunc(func(_ context.Context, name string, _ json.RawMessage) (PermissionResult, error) {
		if name != "Bash" {
			t.Errorf("unexpected tool name: %s", name)
		}
		return PermissionResult{Behavior: PermissionAllow}, nil
	})
	c, buf := newTestController(auth)

	line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"cmd":"ls"}}}`)
	env, _ := ParseLine(line)

	c.Dispatch(context.Background(), env)

	out := buf.Bytes()
	if !bytes.Contains(out, []byte(`"type":"control_response"`)) {
		t.Errorf("expected control_response on stdin; got: %s", out)
	}
	if !bytes.Contains(out, []byte(`"request_id":"req-1"`)) {
		t.Errorf("response must echo request_id; got: %s", out)
	}
	if !bytes.Contains(out, []byte(`"behavior":"allow"`)) {
		t.Errorf("expected allow behavior; got: %s", out)
	}
	if !bytes.Contains(out, []byte(`"updated_input"`)) {
		t.Errorf("allow response must include updated_input; got: %s", out)
	}
}

// Inbound can_use_tool — authorizer returns Deny → outbound deny + message.
func TestController_Inbound_CanUseTool_Deny(t *testing.T) {
	auth := ToolAuthorizerFunc(func(_ context.Context, _ string, _ json.RawMessage) (PermissionResult, error) {
		return PermissionResult{Behavior: PermissionDeny, Message: "user said no"}, nil
	})
	c, buf := newTestController(auth)

	line := []byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`)
	env, _ := ParseLine(line)

	c.Dispatch(context.Background(), env)

	if !bytes.Contains(buf.Bytes(), []byte(`"behavior":"deny"`)) {
		t.Errorf("expected deny; got: %s", buf.Bytes())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"message":"user said no"`)) {
		t.Errorf("expected message; got: %s", buf.Bytes())
	}
}

// Inbound can_use_tool — authorizer errors → outbound subtype=error.
func TestController_Inbound_CanUseTool_AuthorizerError(t *testing.T) {
	auth := ToolAuthorizerFunc(func(_ context.Context, _ string, _ json.RawMessage) (PermissionResult, error) {
		return PermissionResult{}, errors.New("authorizer down")
	})
	c, buf := newTestController(auth)

	line := []byte(`{"type":"control_request","request_id":"req-3","request":{"subtype":"can_use_tool","tool_name":"Bash"}}`)
	env, _ := ParseLine(line)

	c.Dispatch(context.Background(), env)

	if !bytes.Contains(buf.Bytes(), []byte(`"subtype":"error"`)) {
		t.Errorf("expected subtype=error; got: %s", buf.Bytes())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`authorizer down`)) {
		t.Errorf("expected authorizer error message; got: %s", buf.Bytes())
	}
}

// Inbound can_use_tool with no authorizer wired → safe deny default.
func TestController_Inbound_CanUseTool_NoAuthorizer(t *testing.T) {
	c, buf := newTestController(nil)

	line := []byte(`{"type":"control_request","request_id":"req-4","request":{"subtype":"can_use_tool","tool_name":"Bash"}}`)
	env, _ := ParseLine(line)

	c.Dispatch(context.Background(), env)

	if !bytes.Contains(buf.Bytes(), []byte(`"behavior":"deny"`)) {
		t.Errorf("expected default deny; got: %s", buf.Bytes())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`no ToolAuthorizer configured`)) {
		t.Errorf("expected explanatory message; got: %s", buf.Bytes())
	}
}

// A.2 verification: unknown control subtype → empty success ack, no panic.
func TestController_Inbound_UnknownSubtype_GracefulDegrade(t *testing.T) {
	c, buf := newTestController(nil)

	line := []byte(`{"type":"control_request","request_id":"req-5","request":{"subtype":"future_subtype_xyz","mystery_field":42}}`)
	env, _ := ParseLine(line)

	c.Dispatch(context.Background(), env)

	if !bytes.Contains(buf.Bytes(), []byte(`"subtype":"success"`)) {
		t.Errorf("unknown subtype should ack with success; got: %s", buf.Bytes())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"request_id":"req-5"`)) {
		t.Errorf("ack must echo request_id; got: %s", buf.Bytes())
	}
}

// Outbound interrupt: SendInterrupt emits a control_request, response routed by request_id.
func TestController_Outbound_Interrupt_RoundTrip(t *testing.T) {
	c, buf := newTestController(nil)

	// Run SendInterrupt in a goroutine; meanwhile, capture the request_id
	// from buf and feed back a matching control_response via Dispatch.
	type result struct {
		body ControlResponseBody
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		body, err := c.SendInterrupt(context.Background())
		resultCh <- result{body, err}
	}()

	// Wait for the outbound write to land.
	deadline := time.Now().Add(2 * time.Second)
	for buf.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// Extract request_id from the outbound bytes.
	var raw struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &raw); err != nil {
		t.Fatalf("decode outbound: %v\nraw: %s", err, buf.Bytes())
	}
	if raw.Type != "control_request" || raw.RequestID == "" {
		t.Fatalf("outbound malformed: %+v", raw)
	}

	// Feed back the matching response.
	respLine := []byte(`{"type":"control_response","response":{"subtype":"success","request_id":"` + raw.RequestID + `"}}`)
	respEnv, _ := ParseLine(respLine)
	c.Dispatch(context.Background(), respEnv)

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Errorf("SendInterrupt error: %v", r.err)
		}
		if r.body.Subtype != "success" {
			t.Errorf("expected success, got %s", r.body.Subtype)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendInterrupt didn't return after response routed")
	}

	if c.PendingCount() != 0 {
		t.Errorf("pending should be empty after success; got %d", c.PendingCount())
	}
}

// Outbound interrupt with ctx-cancel — SendInterrupt returns ctx.Err and pending is cleaned.
func TestController_Outbound_CtxCancel(t *testing.T) {
	c, _ := newTestController(nil)

	ctx, cancel := context.WithCancel(context.Background())

	type result struct{ err error }
	resultCh := make(chan result, 1)
	go func() {
		_, err := c.SendInterrupt(ctx)
		resultCh <- result{err}
	}()

	// Let the outbound register, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case r := <-resultCh:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendInterrupt didn't return after ctx cancel")
	}

	if c.PendingCount() != 0 {
		t.Errorf("pending must be cleaned after ctx-cancel; got %d", c.PendingCount())
	}
}

// C.6 verification: AbortPending closes all in-flight outbound requests
// with an error-shaped body.
func TestController_AbortPending(t *testing.T) {
	c, _ := newTestController(nil)

	// Start three concurrent SendInterrupt — none will get a response.
	const N = 3
	results := make(chan ControlResponseBody, N)
	errs := make(chan error, N)
	for range N {
		go func() {
			body, err := c.SendInterrupt(context.Background())
			results <- body
			errs <- err
		}()
	}

	// Wait for all three to register.
	deadline := time.Now().Add(1 * time.Second)
	for c.PendingCount() < N && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.PendingCount() != N {
		t.Fatalf("expected %d pending, got %d", N, c.PendingCount())
	}

	// Abort.
	c.AbortPending(ErrRecoverInterrupted)

	// Each call should return shortly with subtype=error.
	for i := range N {
		select {
		case body := <-results:
			if body.Subtype != "error" {
				t.Errorf("call %d: expected error subtype, got %s", i, body.Subtype)
			}
			if !strings.Contains(body.Error, "recover interrupted") {
				t.Errorf("call %d: expected recover msg, got %s", i, body.Error)
			}
			<-errs // drain matching err
		case <-time.After(2 * time.Second):
			t.Fatalf("call %d didn't receive AbortPending sentinel", i)
		}
	}

	if c.PendingCount() != 0 {
		t.Errorf("pending should be 0 after AbortPending; got %d", c.PendingCount())
	}
}

// stdinMu serializes writes — concurrent inbound responses + outbound
// requests must not produce interleaved bytes.
func TestController_ConcurrentWrites_NoInterleave(t *testing.T) {
	auth := ToolAuthorizerFunc(func(_ context.Context, _ string, _ json.RawMessage) (PermissionResult, error) {
		return PermissionResult{Behavior: PermissionAllow}, nil
	})
	c, buf := newTestController(auth)

	const N = 50
	var wg sync.WaitGroup
	for i := range N {
		wg.Add(2)
		// Inbound can_use_tool flood
		go func(i int) {
			defer wg.Done()
			line := []byte(`{"type":"control_request","request_id":"in-` + itoa(i) + `","request":{"subtype":"can_use_tool","tool_name":"Bash"}}`)
			env, _ := ParseLine(line)
			c.Dispatch(context.Background(), env)
		}(i)
		// Outbound interrupt flood (will hang since no response — but the WRITE happens)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, _ = c.SendInterrupt(ctx)
		}()
	}
	wg.Wait()

	// Every line in buf must parse as valid JSON — interleaving would
	// produce garbage at line boundaries.
	for i, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Errorf("line %d malformed (interleave?): %v\nline: %s", i, err, line)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	if neg {
		s = append([]byte{'-'}, s...)
	}
	return string(s)
}
