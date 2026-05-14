package server

import (
	"strings"
	"testing"
)

func TestValidSID(t *testing.T) {
	good := []string{
		"633e5ed8-cada-422a-aee1-c7a3502eb4fd", // cc UUID
		"ses_01HX9P2VPGYZ8MN7Q4VEY8MJ9V",       // opencode-style
		"t-01KRJ1CF1JG94C7P18J4NNTA8N",         // polyforge ULID
		"abc12345",                              // minimal
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
