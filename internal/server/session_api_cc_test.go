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

// ---------------------------------------------------------------------------
// The route's cursor and the two headers it answers with (tether#107)
// ---------------------------------------------------------------------------

// getMessagesBefore is getMessages with the tether#107 cursor. The raw string, not
// an int64, so the malformed cases below go through exactly the same code path a
// browser would.
func getMessagesBefore(t *testing.T, sub http.HandlerFunc, sid, before string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	sub(rec, httptest.NewRequest("GET", "/api/v1/sessions/"+sid+"/messages?before="+before, nil))
	return rec
}

// bigCCTranscript builds a cc transcript past the byte window as uniquely numbered
// user turns, and returns the records plus the texts in order.
func bigCCTranscript(t *testing.T, atLeast int) ([]string, []string) {
	t.Helper()
	var lines, texts []string
	pad := strings.Repeat("y", 48<<10)
	size := 0
	for i := 0; size < atLeast; i++ {
		text := "TURN-" + strconv.Itoa(i) + " " + pad
		rec := ccUserRecord(text)
		lines = append(lines, rec)
		texts = append(texts, text)
		size += len(rec) + 1
	}
	return lines, texts
}

func decodeMessages(t *testing.T, rec *httptest.ResponseRecorder) []session.HistoryMessage {
	t.Helper()
	var msgs []session.HistoryMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("decoding the transcript response: %v\nbody: %.200s", err, rec.Body.String())
	}
	return msgs
}

// TestMessagesRouteOffersAnEarlierPageAndServesIt — the whole feature through the
// HTTP surface: a bounded transcript comes back with a cursor, and spending that
// cursor returns DIFFERENT, EARLIER messages.
//
// Asserting the messages differ is the part that matters. A route that read the
// parameter and ignored it would satisfy "200 with a body" and "a cursor header is
// present" while serving the same page forever, which is the bug wearing the fix's
// clothes.
func TestMessagesRouteOffersAnEarlierPageAndServesIt(t *testing.T) {
	_, ccProjects, workdir, _, sub := ccAPI(t)
	lines, texts := bigCCTranscript(t, 2<<20)
	seedCC(t, ccProjects, workdir, "cc-session-0001", lines...)

	newest := getMessages(t, sub, "cc-session-0001")
	if newest.Code != 200 {
		t.Fatalf("status = %d", newest.Code)
	}
	cursor := newest.Header().Get(session.TranscriptEarlierHeader)
	if cursor == "" {
		t.Fatalf("no %s on a %d-byte transcript; the pane cannot ask for anything earlier",
			session.TranscriptEarlierHeader, len(lines)*(48<<10))
	}
	n, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("%s = %q, want a positive decimal offset", session.TranscriptEarlierHeader, cursor)
	}
	newestMsgs := decodeMessages(t, newest)
	if len(newestMsgs) == 0 {
		t.Fatal("the newest page is empty")
	}
	// The newest page ends on the last turn written.
	if got := newestMsgs[len(newestMsgs)-1].Text; got != texts[len(texts)-1] {
		t.Errorf("newest page ends on %.20q, want %.20q", got, texts[len(texts)-1])
	}

	earlier := getMessagesBefore(t, sub, "cc-session-0001", cursor)
	if earlier.Code != 200 {
		t.Fatalf("earlier page status = %d", earlier.Code)
	}
	earlierMsgs := decodeMessages(t, earlier)
	if len(earlierMsgs) == 0 {
		t.Fatal("the earlier page is empty — the cursor bought nothing")
	}
	if earlierMsgs[0].Text == newestMsgs[0].Text {
		t.Fatalf("the earlier page starts on the same turn as the newest one (%.20q) — the cursor was ignored",
			earlierMsgs[0].Text)
	}
	// Contiguous: the earlier page's last turn is the one just before the newest
	// page's first. This is the seam, and it is where a lost record would show.
	wantLast := ""
	for i, s := range texts {
		if s == newestMsgs[0].Text && i > 0 {
			wantLast = texts[i-1]
		}
	}
	if wantLast == "" {
		t.Fatalf("could not locate the newest page's first turn %.20q in the fixture", newestMsgs[0].Text)
	}
	if got := earlierMsgs[len(earlierMsgs)-1].Text; got != wantLast {
		t.Errorf("earlier page ends on %.20q, want %.20q — a record fell into the seam", got, wantLast)
	}
}

// TestMessagesRouteOmitsTheCursorOnAWholeTranscript — the ABSENCE of the header is
// the signal that the reader has reached the beginning, so a route that always set it
// would leave the top of every conversation ambiguous exactly as it was.
func TestMessagesRouteOmitsTheCursorOnAWholeTranscript(t *testing.T) {
	_, ccProjects, workdir, _, sub := ccAPI(t)
	seedCC(t, ccProjects, workdir, "cc-session-0001", ccUserRecord("the only thing said"))

	rec := getMessages(t, sub, "cc-session-0001")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(session.TranscriptEarlierHeader); got != "" {
		t.Errorf("%s = %q on a one-turn transcript", session.TranscriptEarlierHeader, got)
	}
	if got := rec.Header().Get(session.TranscriptOtherRecordHeader); got != "" {
		t.Errorf("%s = %q for a sid only cc has", session.TranscriptOtherRecordHeader, got)
	}
}

// TestMessagesRouteNamesTheOtherRecord — the header that keeps "you are at the top"
// honest for a sid BOTH stores have. Empty population today; real mechanism.
func TestMessagesRouteNamesTheOtherRecord(t *testing.T) {
	tetherDir, ccProjects, workdir, _, sub := ccAPI(t)
	const sid = "shared-session-01"
	seedTetherTranscript(t, tetherDir, sid, "tether's short record")
	seedCC(t, ccProjects, workdir, sid, ccUserRecord("cc's much longer record"))

	rec := getMessages(t, sub, sid)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(session.TranscriptOtherRecordHeader); got != "cc" {
		t.Errorf("%s = %q, want %q", session.TranscriptOtherRecordHeader, got, "cc")
	}
	// Still tether's transcript. tether#107 must not have moved the store choice.
	msgs := decodeMessages(t, rec)
	if len(msgs) != 1 || msgs[0].Text != "tether's short record" {
		t.Errorf("messages = %+v, want tether's copy", msgs)
	}
	// And no cursor: that store has no ceiling.
	if got := rec.Header().Get(session.TranscriptEarlierHeader); got != "" {
		t.Errorf("%s = %q on the unbounded store", session.TranscriptEarlierHeader, got)
	}
}

// TestMessagesRouteRefusesAMalformedCursor — ignoring it would serve the newest page,
// which is indistinguishable on screen from "there is nothing earlier": pagination
// silently reverting to the ceiling it exists to remove.
func TestMessagesRouteRefusesAMalformedCursor(t *testing.T) {
	_, ccProjects, workdir, _, sub := ccAPI(t)
	seedCC(t, ccProjects, workdir, "cc-session-0001", ccUserRecord("hello"))

	// "%2012" is a leading SPACE, percent-encoded: r.URL.Query() decodes it, so the
	// parser sees " 12" — which is what a client that formatted the cursor with a
	// stray space would send. Written encoded because httptest.NewRequest PANICS on a
	// raw space in the target rather than producing a request the handler could refuse.
	for _, raw := range []string{"abc", "-1", "1.5", "0x10", "1e6", "%2012"} {
		t.Run(raw, func(t *testing.T) {
			rec := getMessagesBefore(t, sub, "cc-session-0001", raw)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("before=%q -> status %d, want 400 (body %.80q)", raw, rec.Code, rec.Body.String())
			}
		})
	}
	// The empty string is ABSENT, not malformed: `?before=` is what a client building
	// the query from an unset cursor produces, and the newest page is the right answer.
	rec := getMessagesBefore(t, sub, "cc-session-0001", "")
	if rec.Code != 200 {
		t.Errorf("before= (empty) -> status %d, want 200", rec.Code)
	}
}

// TestMessagesProbeCarriesNoCursor — the HEAD probe still returns before touching the
// transcript, so it cannot know whether an earlier page exists and must not pretend
// to. Computing one would cost the probe exactly what the fetch it exists to avoid
// costs (session_api.go's whole cost argument for the early return).
func TestMessagesProbeCarriesNoCursor(t *testing.T) {
	_, ccProjects, workdir, _, sub := ccAPI(t)
	lines, _ := bigCCTranscript(t, 2<<20)
	seedCC(t, ccProjects, workdir, "cc-session-0001", lines...)

	rec := httptest.NewRecorder()
	sub(rec, httptest.NewRequest("HEAD", "/api/v1/sessions/cc-session-0001/messages", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(session.TranscriptEarlierHeader); got != "" {
		t.Errorf("HEAD set %s = %q; the probe would have had to read the transcript to know that",
			session.TranscriptEarlierHeader, got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD wrote %d body bytes", rec.Body.Len())
	}
	// The version header is still there — that is the probe's entire answer.
	if got := rec.Header().Get(session.TranscriptUpdatedAtHeader); got == "" {
		t.Error("HEAD lost the version header")
	}
	// A cursor on a HEAD is ignored rather than validated: the probe never selects a
	// page, so validating would only add a way for it to fail.
	bad := httptest.NewRecorder()
	sub(bad, httptest.NewRequest("HEAD", "/api/v1/sessions/cc-session-0001/messages?before=abc", nil))
	if bad.Code != 200 {
		t.Errorf("HEAD with a malformed cursor -> status %d, want 200", bad.Code)
	}
}

// TestMessagesRouteCarriesThePositionOnEveryEntry — tether#109's field has to reach the
// BROWSER, and the route is where that stops being a claim about a struct.
//
// The body of this route is a bare JSON array of session.HistoryMessage, decoded on the
// other side against a HAND-MIRRORED `HistoryEntry` (web/src/lib/store.ts). So there are
// two ways for this to be silently wrong — the field could be omitted by `omitempty`, or
// the mirror could not have it — and neither shows up as an error: mergeHistory reads a
// missing position as "unverifiable" and falls back to replacing the transcript, forever,
// which looks like a jumpy UI rather than like a bug. This test covers the first way;
// store.test.ts's `historyEntryToMessage carries the position` covers the second.
//
// Asserted on the RAW BODY as well as on the decode, because unmarshalling into the same
// struct that marshalled it cannot notice a missing key — it would read as zero.
func TestMessagesRouteCarriesThePositionOnEveryEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, tetherDir, ccProjects, workdir, sid string)
		count int
	}{
		{
			name: "cc's store",
			seed: func(t *testing.T, _, ccProjects, workdir, sid string) {
				seedCC(t, ccProjects, workdir, sid,
					ccUserRecord("first"), ccUserRecord("second"), ccUserRecord("third"))
			},
			count: 3,
		},
		{
			name: "tether's own store",
			seed: func(t *testing.T, tetherDir, _, _, sid string) {
				seedTetherTranscript(t, tetherDir, sid, "the only turn")
			},
			count: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tetherDir, ccProjects, workdir, _, sub := ccAPI(t)
			const sid = "positioned-session-01"
			tc.seed(t, tetherDir, ccProjects, workdir, sid)

			rec := getMessages(t, sub, sid)
			if rec.Code != 200 {
				t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"ord":`) {
				t.Fatalf("the response body has no ord key at all:\n%s", rec.Body.String())
			}
			var got []session.HistoryMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("json.Unmarshal: %v; body=%s", err, rec.Body.String())
			}
			if len(got) != tc.count {
				t.Fatalf("served %d messages, want %d: %s", len(got), tc.count, rec.Body.String())
			}
			// Every entry, strictly increasing, never zero. "Every" is the load-bearing
			// word: mergeHistory refuses the whole page if ONE entry is missing a
			// position, so a store that numbered most of them would degrade exactly as
			// badly as one that numbered none.
			prev := int64(0)
			for i, m := range got {
				if m.Ord <= prev {
					t.Fatalf("entry %d has Ord %d, not greater than the previous %d: %s",
						i, m.Ord, prev, rec.Body.String())
				}
				prev = m.Ord
			}
		})
	}
}
