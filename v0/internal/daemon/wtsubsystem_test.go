package daemon_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/daemon"
)

// syncBuf is a goroutine-safe io.Writer wrapping bytes.Buffer. The
// daemon's verbose logger writes from multiple goroutines (supervisor
// + per-subsystem); a plain strings.Builder/bytes.Buffer races under
// `-race`.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestRun_WTSubsystem_StartsAndStops verifies that setting
// WTListenAddr (or WTListener) makes daemon.Run spawn the wt subsystem
// under the watchdog and tears it down cleanly on ctx cancel.
//
// We use WTListener with a pre-bound :0 UDP socket so the test doesn't
// need a fixed port.
func TestRun_WTSubsystem_StartsAndStops(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	// daemon.Run will close the WT listener via wt.Server.Close → conn.
	// No explicit defer udp.Close() here; the WT subsystem owns it.

	var stderrBuf syncBuf
	cfg := daemon.Config{
		Verbose:          true,
		Stderr:           &stderrBuf,
		ProjectsDir:      projectsDir,
		AttachSocketPath: attachSocket,
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
		WTListener:       udp,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	// Give the subsystem time to log "wt listener starting".
	time.Sleep(200 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon.Run did not exit within 3s of ctx cancel")
	}

	logs := stderrBuf.String()
	if !strings.Contains(logs, "wt listener starting") {
		t.Errorf("expected wt listener log line; got:\n%s", logs)
	}
}
