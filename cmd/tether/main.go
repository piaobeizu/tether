package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/piaobeizu/tether/internal/doctor"
	"github.com/piaobeizu/tether/internal/server"
)

// version is the compile-time default. release.sh injects a real release
// version via `-ldflags -X main.version=...`. That trick only rewrites
// package-level *variables*, never constants, so this must stay a var —
// verified empirically: `-X main.version=` silently no-ops when version is
// declared `const` (prints the untouched default, no linker error) and
// works only once it is a `var`.
var version = "v0.1.0-dev"

// defaultVersion is a plain constant mirroring version's initial value. It
// is never touched by -X, so comparing version != defaultVersion is how
// resolveVersion detects that ldflags actually injected something.
const defaultVersion = "v0.1.0-dev"

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// resolveVersion determines the version string to print, in three tiers:
//
//  1. ldflags injection: if release.sh's `-X main.version=` fired, version
//     no longer equals defaultVersion — return it verbatim, unchanged.
//  2. runtime/debug.ReadBuildInfo: for everyday `go build` (no ldflags),
//     fall back to Go's automatic VCS stamping (module pseudo-version +
//     vcs.revision/vcs.modified build settings) when available.
//  3. Neither: return the compile-time default as-is (today's behaviour).
//
// Caveat: Go's automatic VCS stamping does NOT work inside a git *linked
// worktree* (where .git is a file, not a directory) — a bare `go build`
// run from such a worktree legitimately yields no vcs.* settings, so tier
// 2 degrades to tier 3's output even though ReadBuildInfo reports ok=true.
func resolveVersion() string {
	bi, ok := debug.ReadBuildInfo()
	return formatVersion(version, defaultVersion, bi, ok)
}

// formatVersion is the pure decision logic behind resolveVersion — kept
// free of package-level globals so it is directly table-testable. See
// resolveVersion's doc comment for the three-tier precedence it implements.
// The sentinel is the injected value differing from def. A dedicated
// "" -unless-injected variable would be unambiguous, but the symbol name is
// fixed: scripts/release.sh hard-codes `-X main.version=`, and changing that
// script is out of scope here. Two consequences are accepted and guarded
// instead: an EMPTY injection is treated as "not injected" (below) rather
// than printing a blank line, and injecting a value that happens to equal
// def is indistinguishable from no injection at all.
func formatVersion(injected, def string, bi *debug.BuildInfo, ok bool) string {
	if injected != "" && injected != def {
		return injected
	}
	if !ok || bi == nil {
		return def
	}

	base := def
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		// A module pseudo-version (v0.5.1-0.20260805045854-0e48e1cb4c8f) is a
		// richer base than the compile-time default: it carries the last tag
		// and the build timestamp as well as the commit. Go appends its own
		// "+dirty" marker to it; strip that so the dirty flag below is the
		// single, consistently-formatted place it is reported.
		base = strings.TrimSuffix(bi.Main.Version, "+dirty")
	}

	var revision string
	var modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	result := base
	if revision != "" {
		short := revision
		if len(short) > 12 {
			short = short[:12]
		}
		// A pseudo-version already ends in the short commit hash; appending it
		// again would produce ...-0e48e1cb4c8f+0e48e1cb4c8f. Only add it when
		// the base does not already identify the commit.
		if !strings.Contains(base, short) {
			result = base + "+" + short
		}
	}
	if modified == "true" {
		result += ".dirty"
	}
	return result
}

var rootCmd = &cobra.Command{
	Use:   "tether",
	Short: "tether — AI workspace daemon + browser UI",
	CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
}

func init() {
	rootCmd.AddCommand(versionCmd, newServerCmd(), attachCmd, pairCmd, newDoctorCmd())
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Println(resolveVersion())
	},
}

func newServerCmd() *cobra.Command {
	cfg := &server.Config{}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the tether HTTP/3 + WebTransport server",
		RunE: func(_ *cobra.Command, _ []string) error {
			// Same resolver `tether version` uses, so GET /api/v1/version — and
			// therefore the version shown in the UI — can never disagree with
			// the CLI's answer for this binary.
			cfg.Version = resolveVersion()
			return server.Run(cfg)
		},
	}
	cmd.Flags().IntVarP(&cfg.Port, "port", "p", 8898, "listen port (TCP + UDP)")
	cmd.Flags().StringVar(&cfg.CertFile, "cert-file", "", "TLS cert PEM you own (re-read every minute, never re-minted; needs --key-file; ignored with --acme-domain)")
	cmd.Flags().StringVar(&cfg.KeyFile, "key-file", "", "TLS key PEM for --cert-file (both are needed; either alone falls back to the managed cert)")
	cmd.Flags().StringVar(&cfg.AcmeDomain, "acme-domain", "", "domain for ACME/Let's Encrypt cert (validated over :443; port 80 unused)")
	cmd.Flags().StringVar(&cfg.AcmeEmail, "acme-email", "", "contact email for the ACME account (optional; adding it later registers a different account)")
	cmd.Flags().BoolVar(&cfg.DevMode, "dev", false, "proxy SPA to Vite dev server")
	cmd.Flags().StringVar(&cfg.DevFrontendURL, "dev-url", "", "Vite dev server URL (default http://localhost:5173)")
	cmd.Flags().StringVar(&cfg.Token, "token", "", "static access token (runtime only, not persisted; overrides $TETHER_TOKEN)")
	cmd.Flags().IntVar(&cfg.MCPPort, "mcp-port", 8899, "loopback port for /mcp endpoint")
	cmd.Flags().StringVar(&cfg.MCPConfigPath, "mcp-config", "", "path to config file with [mcp.servers] (default ~/.tether/config.json)")
	cmd.Flags().StringVar(&cfg.WorkspaceRoot, "workspace-root", "", "workspace root for builtin tools AND the DEFAULT agent cwd — a chat session that selects a registered workspace runs in that workspace instead (default ~/.tether/workspace)")
	cmd.Flags().BoolVar(&cfg.SkipMCPInject, "skip-mcp-inject", false, "skip ~/.claude/settings.json injection (CI/test)")
	return cmd
}

var attachCmd = &cobra.Command{
	Use:   "attach",
	Short: "Attach to a running session (stub — implemented in s6)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("attach: not yet implemented (s6)")
	},
}

var pairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair with a remote tether instance (stub — implemented in s6)",
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("pair: not yet implemented (s6)")
	},
}

// newDoctorCmd builds `tether doctor`.
//
// The cert flags are deliberately spelled the same as `tether server`'s and
// feed the same struct: which certificate is served is decided by those flags
// alone, nothing on disk records the choice afterwards, and doctor used to
// assume the managed one — so a healthy --cert-file deployment was told its
// cert was missing (tether#84). Run doctor the way you run the server (same
// unit file, same wrapper script) and it now reports on the cert you serve;
// run it bare on such a host and the cert check says it cannot tell, rather
// than inventing a failure.
func newDoctorCmd() *cobra.Command {
	cfg := &server.Config{}
	var verbose bool
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks",
		RunE: func(_ *cobra.Command, _ []string) error {
			report := doctor.Run(cfg, verbose)
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			for _, c := range report.Checks {
				fmt.Printf("  %s  %-22s  %s\n", mark(c.Status), c.Name, c.Message)
				if verbose && c.Detail != "" {
					fmt.Printf("       %s\n", c.Detail)
				}
			}
			if !report.OK {
				return fmt.Errorf("one or more preflight checks failed")
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&cfg.Port, "port", "p", 8898, "port to check bindability for")
	cmd.Flags().StringVar(&cfg.CertFile, "cert-file", "", "check this cert instead of the managed one, as the server would serve it (needs --key-file)")
	cmd.Flags().StringVar(&cfg.KeyFile, "key-file", "", "key for --cert-file (both are needed; either alone is ignored, exactly as the server ignores it)")
	cmd.Flags().StringVar(&cfg.AcmeDomain, "acme-domain", "", "check the cert certmagic stored for this domain, as the server would serve it")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show extra detail")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// mark renders a check's status. "-" rather than a tick for a skipped check:
// the whole point of the third state is that the reader can tell "checked, and
// it is fine" from "could not check", and two symbols for three states puts one
// of those back under the other's mark.
//
// Only StatusOK earns the tick. Anything unrecognised gets "?" rather than
// falling through to it — a state added to the doctor package later would
// otherwise ship displayed as healthy, which is the failure mode this whole
// change is about.
func mark(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "✓"
	case doctor.StatusFail:
		return "✗"
	case doctor.StatusSkip:
		return "-"
	default:
		return "?"
	}
}
