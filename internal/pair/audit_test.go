package pair

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestAuditLog_AppendOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users", "default", "audit.log")
	// Pre-seed file with a "previous run" line.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const seed = `{"prev":"line"}` + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if err := a.AppendSuccess("device-x", "device-y", "ltk-1", []byte{0x01, 0x02}); err != nil {
		t.Fatalf("AppendSuccess: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.HasPrefix(body, []byte(seed)) {
		t.Errorf("seed line truncated; first %d bytes: %q", len(seed), body[:len(seed)])
	}
	// Second line should be valid JSON for the new event.
	sc := bufio.NewScanner(bytes.NewReader(body))
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if len(lines) != 2 {
		t.Fatalf("line count: got %d want 2", len(lines))
	}
	var ev AuditEvent
	if err := json.Unmarshal(lines[1], &ev); err != nil {
		t.Errorf("decode event: %v", err)
	}
	if ev.Kind != AuditPairSuccess {
		t.Errorf("kind: got %s want %s", ev.Kind, AuditPairSuccess)
	}
	if ev.DeviceID != "device-x" || ev.PeerDeviceID != "device-y" {
		t.Errorf("device ids: got %s/%s", ev.DeviceID, ev.PeerDeviceID)
	}
}

func TestAuditLog_FileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not enforced on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "users", "default", "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode: got %o want 0600", mode)
	}
	ds, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := ds.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode: got %o want 0700", mode)
	}
}

func TestAuditLog_AllEventKinds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if err := a.AppendSuccess("d1", "p1", "ltk-1", []byte{0x01}); err != nil {
		t.Errorf("AppendSuccess: %v", err)
	}
	if err := a.AppendFail("d1", ReasonSASMismatch, "test"); err != nil {
		t.Errorf("AppendFail: %v", err)
	}
	if err := a.AppendRepairRejected("d1"); err != nil {
		t.Errorf("AppendRepairRejected: %v", err)
	}
	if err := a.AppendForceRotated("d1", "ltk-2", "ltk-1"); err != nil {
		t.Errorf("AppendForceRotated: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Each kind should appear in the file.
	for _, k := range []AuditKind{
		AuditPairSuccess, AuditPairFail, AuditPairRepairRejected, AuditPairForceRotated,
	} {
		if !bytes.Contains(body, []byte(`"`+string(k)+`"`)) {
			t.Errorf("missing audit kind %q in file:\n%s", k, body)
		}
	}
}

// TestAuditLog_ConcurrentAppendNoTearing — fire many concurrent
// appends and verify each line parses cleanly.
func TestAuditLog_ConcurrentAppendNoTearing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	const Workers = 16
	const PerWorker = 32
	var wg sync.WaitGroup
	wg.Add(Workers)
	for w := 0; w < Workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < PerWorker; i++ {
				_ = a.AppendSuccess("device-x", "device-y", "ltk", []byte{0x42})
			}
		}()
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	count := 0
	for sc.Scan() {
		var ev AuditEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode line %d: %v\n%s", count, err, sc.Bytes())
		}
		count++
	}
	if want := Workers * PerWorker; count != want {
		t.Errorf("line count: got %d want %d", count, want)
	}
}

func TestAuditLog_CloseRejectsAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close: got %v want nil (idempotent)", err)
	}
	if err := a.AppendSuccess("d", "p", "ltk", nil); err == nil {
		t.Errorf("AppendSuccess on closed log: got nil want error")
	}
}
