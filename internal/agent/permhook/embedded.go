package permhook

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// hookSource is the permission hook source code embedded verbatim.
// EnsureHookBinary compiles it on startup if the binary is missing or stale.
const hookSource = `package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	body, _ := io.ReadAll(os.Stdin)
	endpoint := os.Getenv("TETHER_DAEMON_PERM_ENDPOINT")
	if endpoint == "" {
		// When env var is unset, this is NOT a tether-spawned cc (it's the
		// user's IDE / standalone cc). Exit 0 so we don't break unrelated
		// cc invocations. The hook only enforces tether's permission UI for
		// cc subprocesses launched by the tether daemon.
		os.Exit(0)
	}
	// InsecureSkipVerify is safe: endpoint is always loopback (127.0.0.1).
	client := &http.Client{
		Timeout: 65 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hook] daemon unreachable: %v\n", err)
		os.Exit(2)
	}
	defer resp.Body.Close()
	var dec struct {
		Allow   bool   ` + "`json:\"allow\"`" + `
		Message string ` + "`json:\"message,omitempty\"`" + `
	}
	if err := json.NewDecoder(resp.Body).Decode(&dec); err != nil {
		fmt.Fprintf(os.Stderr, "[hook] decode response: %v\n", err)
		os.Exit(2)
	}
	if dec.Allow {
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "[hook] denied: %s\n", dec.Message)
	os.Exit(2)
}
`

// EnsureHookBinary compiles the hook binary to binPath if it is absent or the
// source hash has changed. Mode 0755 on success (D-05b §4.2).
func EnsureHookBinary(binPath string) error {
	srcHash := fmt.Sprintf("%x", sha256.Sum256([]byte(hookSource)))

	if existing, err := os.ReadFile(binPath + ".hash"); err == nil {
		if string(existing) == srcHash {
			return nil // up-to-date
		}
	}

	tmpDir, err := os.MkdirTemp("", "tether-hook-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	srcPath := filepath.Join(tmpDir, "hook.go")
	if err := os.WriteFile(srcPath, []byte(hookSource), 0o600); err != nil {
		return err
	}

	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compile hook: %w", err)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return err
	}
	return os.WriteFile(binPath+".hash", []byte(srcHash), 0o600)
}
