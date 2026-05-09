package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLine_SystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"abc-123","uuid":"u-1","model":"claude-haiku","claude_code_version":"2.1.123"}`)
	env, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine error: %v", err)
	}
	if env.Type != EventSystem || env.Subtype != "init" {
		t.Errorf("envelope wrong: type=%s subtype=%s", env.Type, env.Subtype)
	}
	if env.SessionID != "abc-123" || env.UUID != "u-1" {
		t.Errorf("ids wrong: sid=%s uuid=%s", env.SessionID, env.UUID)
	}

	si, err := env.DecodeSystemInit()
	if err != nil {
		t.Fatalf("DecodeSystemInit error: %v", err)
	}
	if si.Model != "claude-haiku" || si.ClaudeCodeVersion != "2.1.123" {
		t.Errorf("system/init fields wrong: %+v", si)
	}
}

func TestParseLine_Result(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","num_turns":1,"total_cost_usd":0.0042}`)
	env, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine error: %v", err)
	}
	r, err := env.DecodeResult()
	if err != nil {
		t.Fatalf("DecodeResult error: %v", err)
	}
	if r.IsError || r.StopReason != "end_turn" || r.NumTurns != 1 {
		t.Errorf("result fields wrong: %+v", r)
	}
}

func TestParseLine_UnknownType_NoError(t *testing.T) {
	// Per A.2 graceful-degrade: unknown types must not error.
	line := []byte(`{"type":"future_event_xyz","subtype":"new_thing","session_id":"s"}`)
	env, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine should not error on unknown type: %v", err)
	}
	if env.Type != "future_event_xyz" {
		t.Errorf("expected raw type preservation, got %s", env.Type)
	}
	// Raw should hold the original bytes for passthrough.
	if !bytes.Contains(env.Raw, []byte("future_event_xyz")) {
		t.Errorf("Raw should hold original bytes, got: %s", env.Raw)
	}
}

func TestParseLine_MalformedJSON(t *testing.T) {
	_, err := ParseLine([]byte(`not json garbage`))
	if err == nil {
		t.Error("ParseLine should error on malformed JSON")
	}
}

// Round-trip: parse every line of the captured fixture without error;
// ensure types we know are present can be specifically decoded.
func TestParseLine_FixtureStream1(t *testing.T) {
	path := filepath.Join("testdata", "stream1.ndjson")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	counts := map[EventType]int{}
	var sawInit bool
	var totalLines int

	for scanner.Scan() {
		totalLines++
		env, err := ParseLine(scanner.Bytes())
		if err != nil {
			t.Errorf("line %d: ParseLine error: %v\nline: %s", totalLines, err, scanner.Bytes())
			continue
		}
		counts[env.Type]++

		if env.Type == EventSystem && env.Subtype == "init" {
			si, err := env.DecodeSystemInit()
			if err != nil {
				t.Errorf("line %d DecodeSystemInit: %v", totalLines, err)
			} else {
				sawInit = true
				if si.SessionID == "" {
					t.Errorf("system/init must have non-empty session_id")
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}

	if totalLines == 0 {
		t.Fatal("fixture appears empty")
	}
	if !sawInit {
		t.Error("fixture should contain at least one system/init event")
	}
	t.Logf("parsed %d lines; counts: %+v", totalLines, counts)
}

func TestNewUserMessage_RoundTrip(t *testing.T) {
	um := NewUserMessage("sid-xyz", "hello world")
	b, err := json.Marshal(um)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back UserMessage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != "user" || back.SessionID != "sid-xyz" || back.Message.Content != "hello world" {
		t.Errorf("round-trip failed: %+v", back)
	}
}

func TestControlRequest_RequestSubtype(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"cmd":"ls"}}}`)
	env, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	cr, err := env.DecodeControlRequest()
	if err != nil {
		t.Fatalf("DecodeControlRequest: %v", err)
	}
	st, err := cr.RequestSubtype()
	if err != nil {
		t.Fatalf("RequestSubtype: %v", err)
	}
	if st != "can_use_tool" {
		t.Errorf("subtype wrong: %s", st)
	}
}
