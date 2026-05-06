// Command tether is the v0.1 single-binary CLI (spec D-01). v0.1 ships
// these user-visible subcommands, all routed through the dispatcher below:
//
//	tether daemon                  start the supervised daemon (main goroutine
//	                               = watchdog over daemon + client subsystems,
//	                               per spec §4.1 / D-11).
//	tether skill {install|list|remove|info}   manage the global skill pool
//	                                          (Epic #8, spec §11.Z).
//	tether tether-blob-register <path>   D-18 #3 forward-compat stub
//	                                     (returns "not implemented in v0.1";
//	                                     reserved for the v0.2 blob proxy).
//
// Other subcommands (attach, spawn, resume, doctor, …) land in later
// slices and plug into the same dispatch table.
//
// Exit codes (spec §6 / §4.1):
//
//	0   clean shutdown / success
//	1   daemon failed to bootstrap (config / I/O error before watchdog
//	    started), or subcommand reported a runtime error
//	2   misuse (unknown subcommand, bad flags)
//
// Anything stronger (panic that escapes the supervisor — should never
// happen because every supervised goroutine routes through safeRun) is
// the Go runtime's default "exit 2 with a stack trace" path.
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
		Verbose: *verbose,
		Stderr:  os.Stderr,
	}
	if err := daemon.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tether daemon: %v\n", err)
		return 1
	}
	return 0
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: tether <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  daemon                         start the supervised daemon (watchdog + daemon + client)")
	fmt.Fprintln(w, "  skill install <name|git-url>   install a skill into the global pool")
	fmt.Fprintln(w, "  skill list                     list installed skills")
	fmt.Fprintln(w, "  skill remove <name>            uninstall a skill")
	fmt.Fprintln(w, "  skill info <name>              print manifest details for an installed skill")
	fmt.Fprintln(w, "  tether-blob-register <path>    (v0.1 stub) reserve blob URL for a workspace path")
	fmt.Fprintln(w, "  help                           show this message")
}
