// Command tether is the v0.1 single-binary CLI (spec D-01). v0.1 ships
// these user-visible subcommands, all routed through the dispatcher below:
//
//	tether daemon                  start the supervised daemon (main goroutine
//	                               = watchdog over daemon + client subsystems,
//	                               per spec §4.1 / D-11).
//	tether skill {install|list|remove|info}   manage the global skill pool
//	                                          (Epic #8, spec §11.Z).
//	tether resume <sid>            cwd-aware wrapper around `claude --resume`
//	                               (spec §10.4 strategy c).
//	tether tether-blob-register <path>   D-18 #3 forward-compat stub
//	                                     (returns "not implemented in v0.1";
//	                                     reserved for the v0.2 blob proxy).
//
// Other subcommands (attach, spawn, doctor, …) land in later slices and
// plug into the same dispatch table.
//
// Exit codes (spec §6 / §4.1):
//
//	0   clean shutdown / success
//	1   daemon failed to bootstrap (config / I/O error before watchdog
//	    started), or subcommand reported a runtime error
//	2   misuse (unknown subcommand, bad flags)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/piaobeizu/tether/internal/daemon"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(runDaemon(os.Args[2:]))
	case "skill":
		os.Exit(skillCmd(os.Args[2:]))
	case "resume":
		os.Exit(runResume(os.Args[2:]))
	case "tether-blob-register":
		os.Exit(blobRegisterCmd(os.Args[2:]))
	case "-h", "--help", "help":
		usage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "tether: unknown subcommand %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("tether daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	verbose := fs.Bool("v", false, "verbose supervisor logging")
	projectsDir := fs.String("projects-dir", "",
		"cc projects directory to tail (default $HOME/.claude/projects)")
	attachSocket := fs.String("attach-socket", "",
		"path for the local attach Unix socket (default $HOME/.tether/attach.sock)")
	lockAuditLog := fs.String("lock-audit-log", "",
		"override path for the lock audit log JSONL "+
			"(default $HOME/.tether/users/default/sessions/default/lock.log; spec §11.D)")
	noAudit := fs.Bool("no-audit", false,
		"disable persistent lock audit log (in-memory History only)")
	enableAuth := fs.Bool("auth-broker", false,
		"enable the tool-authorization broker + cc hookserver "+
			"(PreToolUse → UI prompt → decision; "+
			"writes <hook-settings-dir>/settings.json that cc spawns must point at via --settings)")
	hookSettingsDir := fs.String("hook-settings-dir", "",
		"directory for the cc settings.json the daemon generates "+
			"(default $HOME/.tether/cc-settings; only used when --auth-broker is set)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Translate SIGINT / SIGTERM into a ctx cancel — the watchdog's
	// Run will then drain its supervised goroutines and return.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		fmt.Fprintf(os.Stderr, "tether daemon: signal %v received, shutting down\n", s)
		cancel()
	}()

	cfg := daemon.Config{
		Verbose:          *verbose,
		Stderr:           os.Stderr,
		ProjectsDir:      *projectsDir,
		AttachSocketPath: *attachSocket,
		LockAuditLogPath: *lockAuditLog,
		DisableLockAudit: *noAudit,
		EnableAuthBroker: *enableAuth,
		HookSettingsDir:  *hookSettingsDir,
		// Surface the resolved settings.json path on stdout so
		// downstream tooling (the future `tether resume` integration,
		// or a session-spawning sidecar) can read it without poking
		// at the filesystem default. v0.1 daemon doesn't itself spawn
		// cc — that's intentional; see PR description.
		OnHookSettingsReady: func(path string) {
			fmt.Fprintf(os.Stderr, "tether daemon: cc settings → %s\n", path)
		},
	}
	if err := daemon.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tether daemon: %v\n", err)
		return 1
	}
	return 0
}

// runResume parses flags + dispatches to the resume wrapper. Returns an
// exit code.
func runResume(argv []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bucket := fs.String("bucket", "", "disambiguate when sid is found in multiple buckets")
	cwd := fs.String("cwd", "", "manually specify the cwd (skips bucket-name decoding; required when the bucket decodes to multiple existing paths)")
	dryRun := fs.Bool("dry-run", false, "print resolved cwd + argv, do not exec claude")
	binary := fs.String("binary", "claude", "path/name of the claude binary to exec")
	projectsDir := fs.String("projects-dir", "", "override ~/.claude/projects (for tests)")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tether resume <sid> [--bucket B] [--cwd PATH] [--dry-run] [--binary PATH]")
		fmt.Fprintln(os.Stderr, "")
		fs.PrintDefaults()
	}

	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	sid := fs.Arg(0)

	root := *projectsDir
	if root == "" {
		var err error
		root, err = defaultProjectsDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "tether resume: %v\n", err)
			return 1
		}
	}

	var res *ResolveResult
	var err error
	if *cwd != "" {
		// Manual disambiguation path: caller has already chosen the cwd, we
		// just verify the jsonl exists under EncodeBucket(cwd).
		res, err = ResolveSessionWithCwd(root, sid, *cwd)
	} else {
		res, err = ResolveSession(root, sid, *bucket)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tether resume: %v\n", err)
		return 1
	}

	if *dryRun {
		fmt.Printf("cwd:    %s\n", res.Cwd)
		fmt.Printf("bucket: %s\n", res.Bucket)
		fmt.Printf("exec:   %s --resume %s\n", *binary, sid)
		return 0
	}

	if err := ExecClaude(*binary, res.Cwd, sid); err != nil {
		fmt.Fprintf(os.Stderr, "tether resume: %v\n", err)
		return 1
	}
	return 0 // unreachable when ExecClaude succeeds (it execve's)
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: tether <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  daemon                         start the supervised daemon (watchdog + daemon + client)")
	fmt.Fprintln(w, "  skill install <name|git-url>   install a skill into the global pool")
	fmt.Fprintln(w, "  skill list [--json]            list installed skills (--json: machine-readable, consumed by tether-app UI)")
	fmt.Fprintln(w, "  skill remove <name>            uninstall a skill")
	fmt.Fprintln(w, "  skill info <name>              print manifest details for an installed skill")
	fmt.Fprintln(w, "  resume <sid>                   resume a claude session by id (spec §10.4 strategy c)")
	fmt.Fprintln(w, "  tether-blob-register <path>    (v0.1 stub) reserve blob URL for a workspace path")
	fmt.Fprintln(w, "  help                           show this message")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Run `tether <subcommand> -h` for subcommand-specific flags.")
}
