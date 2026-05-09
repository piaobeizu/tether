// Package doctor implements the `tether doctor` preflight checks (s5.5).
package doctor

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

// CheckResult is the result of a single preflight check.
type CheckResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// Report is the aggregated result of all preflight checks.
type Report struct {
	OK     bool          `json:"ok"`
	Checks []CheckResult `json:"checks"`
}

// Run executes all mandatory preflight checks. If verbose is true, extra
// diagnostic detail is populated on each CheckResult.
func Run(port int, verbose bool) Report {
	checks := []CheckResult{
		checkCCBinary(verbose),
		checkDataDir(verbose),
		checkCertState(verbose),
		checkPortBindable(port, verbose),
		checkCCSettingsHooks(verbose),
	}

	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	return Report{OK: ok, Checks: checks}
}

// checkCCBinary verifies the claude binary is on PATH and executable.
func checkCCBinary(verbose bool) CheckResult {
	path, err := exec.LookPath("claude")
	if err != nil {
		return CheckResult{Name: "cc-binary", OK: false, Message: "claude binary not found on PATH"}
	}
	r := CheckResult{Name: "cc-binary", OK: true, Message: "claude found"}
	if verbose {
		r.Detail = path
	}
	return r
}

// checkDataDir verifies ~/.tether/ exists and is writable.
func checkDataDir(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return CheckResult{Name: "data-dir", OK: false, Message: "cannot determine home dir: " + err.Error()}
	}
	dir := filepath.Join(home, ".tether")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return CheckResult{Name: "data-dir", OK: false, Message: "~/.tether does not exist (run `tether server` once to create it)"}
	}
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return CheckResult{Name: "data-dir", OK: false, Message: "~/.tether not writable: " + err.Error()}
	}
	_ = os.Remove(probe)
	r := CheckResult{Name: "data-dir", OK: true, Message: "~/.tether exists and writable"}
	if verbose {
		r.Detail = dir
	}
	return r
}

// checkCertState verifies the managed cert exists and has > 24h remaining.
func checkCertState(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return CheckResult{Name: "cert-state", OK: false, Message: "cannot determine home dir: " + err.Error()}
	}
	certPath := filepath.Join(home, ".tether", "cert.pem")
	data, err := os.ReadFile(certPath)
	if err != nil {
		return CheckResult{Name: "cert-state", OK: false, Message: "cert not found — run `tether server` to generate"}
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return CheckResult{Name: "cert-state", OK: false, Message: "cert.pem: invalid PEM"}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CheckResult{Name: "cert-state", OK: false, Message: "cert.pem: parse error: " + err.Error()}
	}
	remaining := time.Until(cert.NotAfter)
	if remaining < 24*time.Hour {
		return CheckResult{Name: "cert-state", OK: false, Message: fmt.Sprintf("cert expires in %v (< 24h); restart server to rotate", remaining.Round(time.Minute))}
	}
	r := CheckResult{Name: "cert-state", OK: true, Message: fmt.Sprintf("cert valid, expires in %v", remaining.Round(time.Hour))}
	if verbose {
		r.Detail = fmt.Sprintf("notAfter=%s", cert.NotAfter.Format(time.RFC3339))
	}
	return r
}

// checkPortBindable verifies that the given port can be bound on TCP.
func checkPortBindable(port int, verbose bool) CheckResult {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return CheckResult{Name: "port-bindable", OK: false, Message: fmt.Sprintf("port %d not bindable: %v", port, err)}
	}
	_ = l.Close()
	r := CheckResult{Name: "port-bindable", OK: true, Message: fmt.Sprintf("port %d available", port)}
	if verbose {
		r.Detail = addr
	}
	return r
}

// checkCCSettingsHooks verifies that ~/.config/claude/settings.json contains
// the tether-managed PreToolUse hook entry.
func checkCCSettingsHooks(verbose bool) CheckResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return CheckResult{Name: "cc-settings-hooks", OK: false, Message: "cannot determine home dir: " + err.Error()}
	}
	path := filepath.Join(home, ".config", "claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return CheckResult{Name: "cc-settings-hooks", OK: false, Message: "settings.json not found — run `tether server` to inject hook"}
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return CheckResult{Name: "cc-settings-hooks", OK: false, Message: "settings.json: parse error: " + err.Error()}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	list, _ := hooks["PreToolUse"].([]any)
	for _, h := range list {
		if hm, ok := h.(map[string]any); ok {
			if managed, _ := hm[agent.TetherManagedKey].(bool); managed {
				r := CheckResult{Name: "cc-settings-hooks", OK: true, Message: "tether PreToolUse hook is active"}
				if verbose {
					r.Detail = path
				}
				return r
			}
		}
	}
	return CheckResult{Name: "cc-settings-hooks", OK: false, Message: "tether hook not found in settings.json — run `tether server` to inject"}
}
