package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

func TestValidSID(t *testing.T) {
	good := []string{
		"633e5ed8-cada-422a-aee1-c7a3502eb4fd", // cc UUID
		"ses_01HX9P2VPGYZ8MN7Q4VEY8MJ9V",       // opencode-style
		"t-01KRJ1CF1JG94C7P18J4NNTA8N",         // polyforge ULID
		"abc12345",                             // minimal
	}
	for _, sid := range good {
		if !validSID(sid) {
			t.Errorf("validSID(%q) = false, want true", sid)
		}
	}

	bad := []struct {
		sid    string
		reason string
	}{
		{"", "empty"},
		{"short", "too short"},
		{"../etc/passwd", "path traversal"},
		{"..%2Fpasswd", "url-encoded traversal"},
		{"sess/with/slash", "slash"},
		{"sess\\with\\backslash", "backslash"},
		{"sess with space", "space"},
		{"sess\x00null", "null byte"},
		{"sess.dotted", "dot"},
		{"sess+plus", "plus"},
		{"\nnewline_prefix", "control char"},
	}
	for _, c := range bad {
		if validSID(c.sid) {
			t.Errorf("validSID(%q) = true, want false (%s)", c.sid, c.reason)
		}
	}

	// Length cap — 129 chars of all-valid alphabet must still reject.
	long := strings.Repeat("a", 129)
	if validSID(long) {
		t.Errorf("validSID(129-char string) = true, want false (length cap)")
	}
}

// TestGetMessages_IncludesBlock — (tether#8 T7) the GET
// /api/v1/sessions/{sid}/messages endpoint must serialize a persisted
// fenced block (session.HistoryMessage.Block) as a "block" field in the
// JSON response, in the same position/order it was written to history, so
// the frontend can reconstruct the DAG card after a page reload.
func TestGetMessages_IncludesBlock(t *testing.T) {
	h := session.NewHistoryStore(t.TempDir())
	h.AccumulateAssistant("sid-http", "before text\n")
	h.FinalizeAssistant("sid-http")
	h.AppendBlock("sid-http", wire.FencedBlock{
		Kind:    wire.FencedBlockDag,
		Skill:   "s",
		Content: `{"x":1}`,
		BlockID: "s-0",
	})
	h.AccumulateAssistant("sid-http", "after text")
	h.FinalizeAssistant("sid-http")

	_, getMessages := sessionAPIHandlers(h)

	req := httptest.NewRequest("GET", "/api/v1/sessions/sid-http/messages", nil)
	rec := httptest.NewRecorder()
	getMessages(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got []struct {
		Role  string            `json:"role"`
		Text  string            `json:"text"`
		Ts    int64             `json:"ts"`
		Block *wire.FencedBlock `json:"block,omitempty"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v; body=%s", err, rec.Body.String())
	}

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}
	if got[0].Text != "before text\n" || got[0].Block != nil {
		t.Errorf("got[0] = %+v, want text-only \"before text\\n\"", got[0])
	}
	if got[1].Block == nil {
		t.Fatalf("got[1].Block = nil, want a block: %+v", got[1])
	}
	want := wire.FencedBlock{Kind: wire.FencedBlockDag, Skill: "s", Content: `{"x":1}`, BlockID: "s-0"}
	if *got[1].Block != want {
		t.Errorf("got[1].Block = %+v, want %+v", *got[1].Block, want)
	}
	if got[2].Text != "after text" || got[2].Block != nil {
		t.Errorf("got[2] = %+v, want text-only \"after text\"", got[2])
	}

	// Raw JSON must actually contain a "block" key — guards against a
	// silent regression where omitempty accidentally drops a non-nil block.
	if !strings.Contains(rec.Body.String(), `"block"`) {
		t.Errorf("response body missing \"block\" key: %s", rec.Body.String())
	}
}
