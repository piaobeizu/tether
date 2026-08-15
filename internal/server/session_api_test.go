package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	_, getMessages := sessionAPIHandlers(&session.SessionIndex{History: h}, nil)

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

// ─── tether#91: the session list and the work-item binding ──────────────────

// newSessionAPI builds the two handlers over a temp sessions directory, plus the
// stores a test needs to arrange fixtures with.
func newSessionAPI(t *testing.T) (dir string, list, sub http.HandlerFunc, wis *session.WIBindingStore) {
	t.Helper()
	dir = t.TempDir()
	h := session.NewHistoryStore(dir)
	wis = session.NewWIBindingStore(dir)
	idx := &session.SessionIndex{History: h, WI: wis, Bindings: session.NewBindingStore(dir)}
	list, sub = sessionAPIHandlers(idx, wis)
	return dir, list, sub, wis
}

// seedTranscript writes a one-line transcript for sid and stamps its mtime, so a
// test can control the list's ORDER independently of the directory names.
func seedTranscript(t *testing.T, dir, sid, text string, mtime time.Time) {
	t.Helper()
	sidDir := filepath.Join(dir, sid)
	if err := os.MkdirAll(sidDir, 0o700); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{"role": "user", "text": text, "ts": 1})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sidDir, "history.jsonl")
	if err := os.WriteFile(p, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

type listRow struct {
	Sid       string `json:"sid"`
	WorkItem  string `json:"workItem"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updatedAt"`
}

func getList(t *testing.T, list http.HandlerFunc) []listRow {
	t.Helper()
	rec := httptest.NewRecorder()
	list(rec, httptest.NewRequest("GET", "/api/v1/sessions", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/v1/sessions = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []listRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal list: %v; body=%s", err, rec.Body.String())
	}
	return rows
}

// TestListSessions_RecordsNewestFirst — GET /api/v1/sessions returns records, not
// bare sids, ordered by the transcript's mtime.
//
// The fixture's names sort a < b < c while its times sort b > a > c, so the
// expected answer is neither the directory order nor its reverse. That matters
// because the shape this replaces was exactly "ReadDir order, reversed by the
// browser": a test asserting only that the order is deterministic would pass
// against the bug.
func TestListSessions_RecordsNewestFirst(t *testing.T) {
	dir, list, _, wis := newSessionAPI(t)
	base := time.Now().Add(-time.Hour)
	seedTranscript(t, dir, "aaaa1111", "middle one", base.Add(20*time.Minute))
	seedTranscript(t, dir, "bbbb2222", "newest one", base.Add(40*time.Minute))
	seedTranscript(t, dir, "cccc3333", "oldest one", base.Add(5*time.Minute))
	if err := wis.Save("aaaa1111", "tether#91"); err != nil {
		t.Fatal(err)
	}

	rows := getList(t, list)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	if rows[0].Sid != "bbbb2222" || rows[1].Sid != "aaaa1111" || rows[2].Sid != "cccc3333" {
		t.Fatalf("order = %v, want [bbbb2222 aaaa1111 cccc3333] (mtime order)",
			[]string{rows[0].Sid, rows[1].Sid, rows[2].Sid})
	}
	if rows[1].WorkItem != "tether#91" {
		t.Errorf("rows[1].WorkItem = %q, want tether#91", rows[1].WorkItem)
	}
	if rows[0].WorkItem != "" {
		t.Errorf("rows[0].WorkItem = %q, want empty (unbound)", rows[0].WorkItem)
	}
	if rows[0].Title != "newest one" {
		t.Errorf("rows[0].Title = %q, want %q", rows[0].Title, "newest one")
	}
	if rows[0].UpdatedAt <= rows[1].UpdatedAt || rows[1].UpdatedAt <= rows[2].UpdatedAt {
		t.Errorf("updatedAt not descending: %d %d %d", rows[0].UpdatedAt, rows[1].UpdatedAt, rows[2].UpdatedAt)
	}
}

// TestListSessions_EmptyIsAnArrayNotNull — the frontend maps over this. `null`
// would be a TypeError on first load of a fresh daemon.
func TestListSessions_EmptyIsAnArrayNotNull(t *testing.T) {
	_, list, _, _ := newSessionAPI(t)
	rec := httptest.NewRecorder()
	list(rec, httptest.NewRequest("GET", "/api/v1/sessions", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want %q", got, "[]")
	}
}

// TestPutSessionWI_RoundTripsIntoTheList — the wiring hop, end to end over HTTP:
// a PUT is visible as a workItem on the next GET of the list. Asserting only that
// the store received it would pass with the two halves unconnected.
func TestPutSessionWI_RoundTripsIntoTheList(t *testing.T) {
	dir, list, sub, _ := newSessionAPI(t)
	seedTranscript(t, dir, "aaaa1111", "hello", time.Now())

	rec := httptest.NewRecorder()
	sub(rec, httptest.NewRequest("PUT", "/api/v1/sessions/aaaa1111/wi",
		strings.NewReader(`{"workItem":"tether#91"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	rows := getList(t, list)
	if len(rows) != 1 || rows[0].WorkItem != "tether#91" {
		t.Fatalf("list after PUT = %+v, want one row bound to tether#91", rows)
	}
}

// TestPutSessionWI_ManySessionsOneWorkItem — the point of storing session -> wi
// rather than wi -> session: one work item can own several sessions with no extra
// structure, and asking for them is a filter over the list that already exists.
func TestPutSessionWI_ManySessionsOneWorkItem(t *testing.T) {
	dir, list, sub, _ := newSessionAPI(t)
	seedTranscript(t, dir, "aaaa1111", "first", time.Now().Add(-2*time.Minute))
	seedTranscript(t, dir, "bbbb2222", "second", time.Now().Add(-time.Minute))
	seedTranscript(t, dir, "cccc3333", "unrelated", time.Now())

	for _, sid := range []string{"aaaa1111", "bbbb2222"} {
		rec := httptest.NewRecorder()
		sub(rec, httptest.NewRequest("PUT", "/api/v1/sessions/"+sid+"/wi",
			strings.NewReader(`{"workItem":"tether#91"}`)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("PUT %s = %d, want 204", sid, rec.Code)
		}
	}

	var bound []string
	for _, r := range getList(t, list) {
		if r.WorkItem == "tether#91" {
			bound = append(bound, r.Sid)
		}
	}
	// Newest first, so the second session leads — which is what "open the wi's
	// session" resolves to in the UI.
	if len(bound) != 2 || bound[0] != "bbbb2222" || bound[1] != "aaaa1111" {
		t.Errorf("sessions bound to tether#91 = %v, want [bbbb2222 aaaa1111]", bound)
	}
}

// TestPutSessionWI_Rejections — every way the route says no. The sid guard is
// asserted here as well as in the store because this is where a client-supplied
// one arrives.
func TestPutSessionWI_Rejections(t *testing.T) {
	dir, _, sub, wis := newSessionAPI(t)
	seedTranscript(t, dir, "aaaa1111", "hello", time.Now())

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		// The first three are three DIFFERENT gates, which is why they are named
		// for the gate and not all called "bad sid": a traversal attempt arrives
		// with extra path segments and dies on the SHAPE check before validSID is
		// reached (`..%2F..%2Fetc` decodes to `../../etc`, i.e. 7 parts, not 5), so
		// it proves nothing about the allowlist. The length and alphabet cases are
		// the ones that actually run validSID — without them, deleting the guard
		// entirely would leave this table green.
		{"traversal sid — refused on path shape", "PUT", "/api/v1/sessions/..%2F..%2Fetc/wi", `{"workItem":"x"}`, 400},
		{"sid too short for the allowlist", "PUT", "/api/v1/sessions/short/wi", `{"workItem":"x"}`, 400},
		{"sid outside the allowlist alphabet", "PUT", "/api/v1/sessions/aaaa.1111/wi", `{"workItem":"x"}`, 400},
		{"sid outside the allowlist, on the read route", "GET", "/api/v1/sessions/aaaa.1111/messages", "", 400},
		{"empty work item", "PUT", "/api/v1/sessions/aaaa1111/wi", `{"workItem":""}`, 400},
		{"missing work item", "PUT", "/api/v1/sessions/aaaa1111/wi", `{}`, 400},
		{"work item with a newline", "PUT", "/api/v1/sessions/aaaa1111/wi", "{\"workItem\":\"a\\nb\"}", 400},
		{"over-long work item", "PUT", "/api/v1/sessions/aaaa1111/wi",
			`{"workItem":"` + strings.Repeat("x", session.MaxWorkItemLen+1) + `"}`, 400},
		{"not json", "PUT", "/api/v1/sessions/aaaa1111/wi", "wat", 400},
		{"GET on the wi route", "GET", "/api/v1/sessions/aaaa1111/wi", "", 405},
		{"PUT on the messages route", "PUT", "/api/v1/sessions/aaaa1111/messages", "", 405},
		{"unknown leaf", "GET", "/api/v1/sessions/aaaa1111/nope", "", 404},
		{"no leaf at all", "GET", "/api/v1/sessions/aaaa1111", "", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			sub(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
			if rec.Code != c.want {
				t.Errorf("%s %s = %d, want %d; body=%s", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}

	// Nothing was recorded by any of the refusals.
	if b, ok := wis.Load("aaaa1111"); ok {
		t.Errorf("a refused request still wrote a binding: %+v", b)
	}
}

// TestPutSessionWI_NoStoreIsUnavailableNotSuccess — a daemon assembled without a
// wi store must say so. Answering 204 would tell the browser its binding is safe
// when nothing was written, and the browser deletes its legacy localStorage key
// on that answer.
func TestPutSessionWI_NoStoreIsUnavailableNotSuccess(t *testing.T) {
	dir := t.TempDir()
	_, sub := sessionAPIHandlers(&session.SessionIndex{History: session.NewHistoryStore(dir)}, nil)

	rec := httptest.NewRecorder()
	sub(rec, httptest.NewRequest("PUT", "/api/v1/sessions/aaaa1111/wi",
		strings.NewReader(`{"workItem":"tether#91"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT with no store = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSessionSub_MessagesStillWorks — the prefix handler grew a router; the route
// it grew around must be unchanged.
func TestSessionSub_MessagesStillWorks(t *testing.T) {
	dir, _, sub, _ := newSessionAPI(t)
	seedTranscript(t, dir, "aaaa1111", "hello there", time.Now())

	rec := httptest.NewRecorder()
	sub(rec, httptest.NewRequest("GET", "/api/v1/sessions/aaaa1111/messages", nil))
	if rec.Code != 200 {
		t.Fatalf("GET messages = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var msgs []struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(msgs) != 1 || msgs[0].Text != "hello there" {
		t.Errorf("messages = %+v, want one user turn %q", msgs, "hello there")
	}
}
