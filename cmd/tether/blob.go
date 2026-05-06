package main

import (
	"fmt"
	"os"
)

// blobRegisterCmd is the v0.1 forward-compat stub for D-18 #3
// (`/blob/<sha256>` proxy). Spec §11.AA: v0.1 reserves the path/CLI shape
// so future skill code can target it stably; v0.2 lands the real
// blob-proxy + DAG skill that uses it.
func blobRegisterCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tether tether-blob-register <path>")
		return 2
	}
	fmt.Fprintln(os.Stderr, "tether-blob-register: not implemented in v0.1 (reserved for v0.2 blob proxy)")
	return 1
}
