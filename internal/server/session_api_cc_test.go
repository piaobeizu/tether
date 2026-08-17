package server

// tether#92 — the routes over the two stores, and the log line this route never
// had.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/session"
)

// ccAPI builds the session routes over a tether sessions directory AND a
// synthetic cc project store. Both directories are created by the test; nothing
// here points at a real one.
func ccAPI(t *testing.T) (tetherDir string, ccProjects string, workdir string, list, sub http.HandlerFunc) {
	t.Helper()
	tetherDir = t.TempDir()
	ccProjects = filepath.Join(t.TempDir(), "projects")
	workdir = "/w"
	if err := os.MkdirAll(filepath.Join(ccProjects, session.EncodeProjectDir(workdir)), 0o700); err != nil {
		t.Fatal(err)
	}
	idx := &session.SessionIndex{
		History: session.NewHistoryStore(tetherDir),
		CC:      session.NewCCStore(ccProjects, func() []string { return []string{workdir} }),
	}
	list, sub = sessionAPIHandlers(idx, nil)
	return tetherDir, ccProjects, workdir, list, sub
}

// seedCC writes a synthetic cc transcript.
func seedCC(t *testing.T, ccProjects, workdir, sid string, lines ...string) {
	t.Helper()
	path := filepath.Join(ccProjects, session.EncodeProjectDir(workdir), sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ccUserRecord(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message":   map[string]any{"role": "user", "content": text},
	})
	return string(b)
}

// seedTetherTranscript writes tether's own history for sid.
func seedTetherTranscript(t *testing.T, dir, sid, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, sid), 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"role": "user", "text": text, "ts": 1})
	if err := os.WriteFile(filepath.Join(dir, sid, "history.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// captureLogs already exists in this package (cert_acme_alpn_test.go) and is
// reused rather than reimplemented — two log-capture helpers with different
// buffer types is precisely the drift lib/session.ts's doc is a post-mortem of.

func getMessages(t *testing.T, sub http.HandlerFunc, sid string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	sub(rec, httptest.NewRequest("GET", "/api/v1/sessions/"+sid+"/messages", nil))
	return rec
}

// TestMessagesServesACCOnlySession — the route a click lands on. Before this
// change it answered 200 [] for every session tether had not recorded, which is
// what made a listed cc session open as an empty chat.
func TestMessagesServesACCOnlySession(t *testing.T) {
	_, ccProjects, workdir, _, sub := ccAPI(t)
	seedCC(t, ccProjects, workdir, "cc-session-0001", ccUserRecord("typed in a terminal"))

	rec := getMessages(t, sub, "cc-session-0001")
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got []session.HistoryMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0].Role != "user" || got[0].Text != "typed in a terminal" {
		t.Errorf("messages = %+v, want the cc turn", got)
	}
}

// TestMessagesPrefersTetherForASharedSid — the fallback must not shadow the path
// that already works. Same precedence as the list; asserted here because these
// are the two places that apply it.
func TestMessagesPrefersTetherForASharedSid(t *testing.T) {
	tetherDir, ccProjects, workdir, _, sub := ccAPI(t)
	const sid = "shared-session-0001"
	seedTetherTranscript(t, tetherDir, sid, "tether's version")
	seedCC(t, ccProjects, workdir, sid, ccUserRecord("cc's version"))

	var got []session.HistoryMessage
	if err := json.Unmarshal(getMessages(t, sub, sid).Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "tether's version" {
		t.Errorf("messages = %+v, want tether's transcript", got)
	}
}

// TestMessagesForAnUnknownSessionIsStillAnEmptyList — openSession fetches this
// route for a session created moments ago. A 404 here would make a brand-new
// chat look like a failure.
func TestMessagesForAnUnknownSessionIsStillAnEmptyList(t *testing.T) {
	_, _, _, _, sub := ccAPI(t)
	rec := getMessages(t, sub, "nobody-has-this-1")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %s, want []", rec.Body.String())
	}
}

// TestMessagesLogsWhichStoreAnswered — the observability gap this wi closes.
//
// The route logged NOTHING, so when a user reported "clicking a session does
// nothing" the daemon had no record that the request had even arrived, and the
// investigation ran on indirect evidence. The three cases below are exactly the
// three that look identical from outside — an empty response is not enough on its
// own to tell them apart.
func TestMessagesLogsWhichStoreAnswered(t *testing.T) {
	for _, tc := range []struct {
		name, sid, wantSource string
		wantCount             int
		seed                  func(t *testing.T, tetherDir, ccProjects, workdir string)
	}{
		{
			name: "tether", sid: "tttt1111", wantSource: session.SourceTether, wantCount: 1,
			seed: func(t *testing.T, tetherDir, _, _ string) {
				seedTetherTranscript(t, tetherDir, "tttt1111", "hello")
			},
		},
		{
			name: "cc", sid: "cc-session-0001", wantSource: session.SourceCC, wantCount: 1,
			seed: func(t *testing.T, _, ccProjects, workdir string) {
				seedCC(t, ccProjects, workdir, "cc-session-0001", ccUserRecord("hello"))
			},
		},
		{
			name: "neither", sid: "nobody-has-this-1", wantSource: "", wantCount: 0,
			seed: func(*testing.T, string, string, string) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tetherDir, ccProjects, workdir, _, sub := ccAPI(t)
			tc.seed(t, tetherDir, ccProjects, workdir)

			logs := captureLogs(t)
			getMessages(t, sub, tc.sid)
			line := logs.String()

			if !strings.Contains(line, "session transcript served") {
				t.Fatalf("no transcript log line was emitted; got %q", line)
			}
			for _, want := range []string{
				"sid=" + tc.sid,
				"count=" + strconv.Itoa(tc.wantCount),
				"source=" + quoteIfEmpty(tc.wantSource),
			} {
				if !strings.Contains(line, want) {
					t.Errorf("log line missing %q; got %q", want, line)
				}
			}
		})
	}
}

// quoteIfEmpty mirrors slog's text encoding: an empty attribute value is written
// as source="" rather than as a bare source=.
func quoteIfEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// TestListIncludesCCSessions — end to end over the collection route, which is
// what the browser actually fetches. The daemon must ship a row for a session it
// never recorded, carrying its title and its source.
func TestListIncludesCCSessions(t *testing.T) {
	tetherDir, ccProjects, workdir, list, _ := ccAPI(t)
	seedTetherTranscript(t, tetherDir, "tttt1111", "tether's own")
	seedCC(t, ccProjects, workdir, "cc-session-0001", ccUserRecord("typed in a terminal"))

	rec := httptest.NewRecorder()
	list(rec, httptest.NewRequest("GET", "/api/v1/sessions", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var rows []session.SessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("json.Unmarshal: %v; body=%s", err, rec.Body.String())
	}
	bySid := map[string]session.SessionSummary{}
	for _, r := range rows {
		bySid[r.Sid] = r
	}
	cc, ok := bySid["cc-session-0001"]
	if !ok {
		t.Fatalf("the cc session is not in the list: %+v", rows)
	}
	if cc.Source != session.SourceCC || cc.Title != "typed in a terminal" {
		t.Errorf("cc row = %+v, want source %q and its opening prompt", cc, session.SourceCC)
	}
	if bySid["tttt1111"].Source != session.SourceTether {
		t.Errorf("tether row source = %q, want %q", bySid["tttt1111"].Source, session.SourceTether)
	}
	// The wire field is not omitempty on purpose — a row whose source is absent
	// is indistinguishable from a daemon that predates the field.
	if !strings.Contains(rec.Body.String(), `"source":"tether"`) {
		t.Errorf("response body has no explicit tether source: %s", rec.Body.String())
	}
}
