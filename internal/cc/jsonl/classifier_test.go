package jsonl

import (
	"testing"
)

// TestClassify_Taxonomy is an exhaustive table covering the F-04
// three-class taxonomy. Adding a new RecordType means adding a row
// here AND a row in doc.go's table — keep them in lockstep.
func TestClassify_Taxonomy(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		want Class
	}{
		// EVENT
		{"user message → EVENT", Record{Type: RecordTypeUser, UUID: "u1"}, ClassEvent},
		{"assistant message → EVENT", Record{Type: RecordTypeAssistant, UUID: "a1"}, ClassEvent},

		// HOOK (attachment with hookEvent)
		{"attachment with hookEvent → HOOK",
			Record{Type: RecordTypeAttachment, Attachment: &Attachment{HookEvent: "SessionStart"}},
			ClassHook},
		{"attachment with PreToolUse hook → HOOK",
			Record{Type: RecordTypeAttachment, Attachment: &Attachment{HookEvent: "PreToolUse"}},
			ClassHook},

		// STATE — attachment without hookEvent
		{"attachment without hookEvent → STATE",
			Record{Type: RecordTypeAttachment, Attachment: &Attachment{HookName: "x"}},
			ClassState},
		{"attachment with nil Attachment → STATE",
			Record{Type: RecordTypeAttachment},
			ClassState},

		// STATE — pure mutations
		{"permission-mode → STATE",
			Record{Type: RecordTypePermissionMode, PermissionMode: "auto"}, ClassState},
		{"last-prompt → STATE",
			Record{Type: RecordTypeLastPrompt}, ClassState},
		{"file-history-snapshot → STATE",
			Record{Type: RecordTypeFileHistorySnapshot}, ClassState},
		{"ai-title → STATE",
			Record{Type: RecordTypeAITitle}, ClassState},
		{"system → STATE",
			Record{Type: RecordTypeSystem}, ClassState},

		// UNKNOWN — forward compat
		{"unknown type → UNKNOWN",
			Record{Type: "future-type"}, ClassUnknown},
		{"empty type → UNKNOWN",
			Record{}, ClassUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.rec)
			if got != tc.want {
				t.Errorf("Classify(%+v) = %v, want %v", tc.rec, got, tc.want)
			}
		})
	}
}

func TestRouteAsState(t *testing.T) {
	if !RouteAsState(ClassState) {
		t.Error("ClassState must route as STATE")
	}
	if !RouteAsState(ClassUnknown) {
		t.Error("ClassUnknown must route as STATE (forward-compat)")
	}
	if RouteAsState(ClassEvent) {
		t.Error("ClassEvent must NOT route as STATE")
	}
	if RouteAsState(ClassHook) {
		t.Error("ClassHook must NOT route as STATE")
	}
}

func TestClassString(t *testing.T) {
	cases := map[Class]string{
		ClassEvent:   "EVENT",
		ClassHook:    "HOOK",
		ClassState:   "STATE",
		ClassUnknown: "UNKNOWN",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("Class(%d).String() = %q, want %q", c, got, want)
		}
	}
}
