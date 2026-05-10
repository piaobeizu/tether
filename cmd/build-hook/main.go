// build-hook compiles the embedded permission hook to the given output path.
// Used in the release pipeline to pre-compile the hook for each target platform.
//
// Usage: go run ./cmd/build-hook <output-path>
package main

import (
	"fmt"
	"os"

	"github.com/piaobeizu/tether/internal/permission/cchook"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: build-hook <output-path>")
		os.Exit(1)
	}
	if err := cchook.EnsureHookBinary(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "build-hook: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "built: %s\n", os.Args[1])
}
