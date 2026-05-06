// Command tether is the v0.1 user-facing CLI. Today it only exposes the
// skill subsystem (Epic #8) and a `tether-blob-register` forward-compat
// stub (D-18 #3); daemon/spawn surfaces land in later epics.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
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

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: tether <command> [args...]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  skill install <name|git-url>   install a skill into the global pool")
	fmt.Fprintln(w, "  skill list                     list installed skills")
	fmt.Fprintln(w, "  skill remove <name>            uninstall a skill")
	fmt.Fprintln(w, "  skill info <name>              print manifest details for an installed skill")
	fmt.Fprintln(w, "  tether-blob-register <path>    (v0.1 stub) reserve blob URL for a workspace path")
}
