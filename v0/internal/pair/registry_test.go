package pair

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) (*Registry, *AuditLog, string) {
	t.Helper()
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "users", "default", "audit.log")
	a, err := NewAuditLog(auditPath)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	root := filepath.Join(dir, "users", "default", "devices")
	r, err := NewRegistry(RegistryConfig{Root: root, Audit: a})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, a, root
}

func makeRecord() DeviceRecord {
	return DeviceRecord{
		V:                   1,
		DeviceID:            "device-mobile-abc1",
		Kind:                KindMobile,
		DisplayName:         "Test Phone",
		LongTermKey:         bytes.Repeat([]byte{0xAA}, 32),
		TransportBindingKey: bytes.Repeat([]byte{0xBB}, 32),
		LongTermKeyID:       "ltk-test-1",
		PushToken:           "fcm-token-xyz",
		PairedAt:            time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		LastSeen:            time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
	}
}

func TestRegistry_SaveLoadRoundtrip(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	rec := makeRecord()
	if err := r.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := r.Load(rec.DeviceID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DeviceID != rec.DeviceID {
		t.Errorf("DeviceID: got %s want %s", got.DeviceID, rec.DeviceID)
	}
	if got.Kind != rec.Kind {
		t.Errorf("Kind: got %s want %s", got.Kind, rec.Kind)
	}
	if !bytes.Equal(got.LongTermKey, rec.LongTermKey) {
		t.Errorf("LongTermKey mismatch")
	}
	if !bytes.Equal(got.TransportBindingKey, rec.TransportBindingKey) {
		t.Errorf("TransportBindingKey mismatch")
	}
	if got.PushToken != rec.PushToken {
		t.Errorf("PushToken: got %q want %q", got.PushToken, rec.PushToken)
	}
}

func TestRegistry_RePairDefaultRejects(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	rec := makeRecord()
	if err := r.Save(rec); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	rec2 := rec
	rec2.LongTermKey = bytes.Repeat([]byte{0xCC}, 32)
	if err := r.Save(rec2); !errors.Is(err, ErrAlreadyPaired) {
		t.Errorf("re-pair Save: got %v want ErrAlreadyPaired", err)
	}
	// And the on-disk record was not overwritten.
	got, err := r.Load(rec.DeviceID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got.LongTermKey, rec.LongTermKey) {
		t.Errorf("Save rejection must not alter on-disk record")
	}
}

func TestRegistry_ForceSaveOverwritesAndAudits(t *testing.T) {
	r, audit, _ := newTestRegistry(t)
	rec := makeRecord()
	if err := r.Save(rec); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	rec2 := rec
	rec2.LongTermKey = bytes.Repeat([]byte{0xCC}, 32)
	rec2.LongTermKeyID = "ltk-test-2"
	if err := r.ForceSave(rec2); err != nil {
		t.Fatalf("ForceSave: %v", err)
	}
	got, err := r.Load(rec.DeviceID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got.LongTermKey, rec2.LongTermKey) {
		t.Errorf("ForceSave did not overwrite LongTermKey")
	}
	// Audit log should have a force-rotated line.
	body, err := os.ReadFile(audit.Path())
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !bytes.Contains(body, []byte(`"pair.force-rotated"`)) {
		t.Errorf("audit log missing pair.force-rotated line: %s", body)
	}
	if !bytes.Contains(body, []byte(`"previousLongTermKeyId":"ltk-test-1"`)) {
		t.Errorf("audit log missing previousLongTermKeyId: %s", body)
	}
}

func TestRegistry_ListAndDelete(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	for _, id := range []DeviceID{"device-mobile-aaa", "device-desktop-bbb"} {
		rec := makeRecord()
		rec.DeviceID = id
		if err := r.Save(rec); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	ids, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("List length: got %d want 2", len(ids))
	}
	if err := r.Delete("device-mobile-aaa"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := r.Delete("device-mobile-aaa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double Delete: got %v want ErrNotFound", err)
	}
}

func TestRegistry_FileModeAndDirMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not enforced on Windows")
	}
	r, _, root := newTestRegistry(t)
	rec := makeRecord()
	if err := r.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File 0600.
	st, err := os.Stat(filepath.Join(root, string(rec.DeviceID)+".json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode: got %o want 0600", mode)
	}
	// Dir 0700.
	ds, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := ds.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode: got %o want 0700", mode)
	}
}

func TestRegistry_ValidateDeviceID(t *testing.T) {
	cases := []struct {
		id    DeviceID
		valid bool
	}{
		{"device-mobile-abc1", true},
		{"DEVICE-DESK-XYZ", true},
		{"abc", true},
		{"", false},
		{"with space", false},
		{"with/slash", false},
		{"with..dots", false},
		{"unicodé", false},
	}
	for _, c := range cases {
		err := ValidateDeviceID(c.id)
		if c.valid && err != nil {
			t.Errorf("ValidateDeviceID(%q): got %v want nil", c.id, err)
		}
		if !c.valid && err == nil {
			t.Errorf("ValidateDeviceID(%q): got nil want error", c.id)
		}
	}
}
