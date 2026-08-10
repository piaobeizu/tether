package main

import (
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/doctor"
)

// The third state only exists to be read. A skip rendered under the tick — the
// shape this had before tether#84, when every result was one of two marks —
// would leave the operator with the same report they had, and the plumbing
// underneath it would be doing nothing anyone could see.
func TestMark_GivesEachStatusItsOwnSymbol(t *testing.T) {
	seen := map[string]doctor.Status{}
	for _, s := range []doctor.Status{doctor.StatusOK, doctor.StatusFail, doctor.StatusSkip} {
		m := mark(s)
		if m == "" {
			t.Errorf("%s renders as nothing", s)
		}
		if prev, dup := seen[m]; dup {
			t.Errorf("%s and %s both render as %q", prev, s, m)
		}
		seen[m] = s
	}
}

// An unknown status must not read as healthy. It can only arrive from a future
// state added to the doctor package, and defaulting that to a tick is how a new
// kind of problem would ship looking fine.
func TestMark_UnknownStatusIsNotShownAsAPass(t *testing.T) {
	if got, ok := mark(doctor.Status("nonsense")), mark(doctor.StatusOK); got == ok {
		t.Errorf("unknown status renders as %q, the same as ok", got)
	}
}

// pflag reads the first back-quoted word of a flag's usage string as the name
// of that flag's argument (UnquoteUsage). Prose in backticks therefore does not
// render as prose: "same as `tether server --cert-file`: ..." came out of
// --help as "--cert-file tether server --cert-file   same as ...", which is
// help an operator cannot act on. That shipped through a green test suite and
// was caught only by running the binary — so this asserts the rendered block
// rather than the usage strings that feed it, which is the half that was wrong.
func TestDoctorFlags_RenderTheirArgumentPlaceholder(t *testing.T) {
	usage := newDoctorCmd().Flags().FlagUsages()
	for _, name := range []string{"cert-file", "key-file", "acme-domain"} {
		if want := "--" + name + " string"; !strings.Contains(usage, want) {
			t.Errorf("--help does not render %q; the usage text is probably being read as the placeholder:\n%s", want, usage)
		}
	}
}

// --mcp-port defaults to 0 and must keep doing so, which is not the tidy choice:
// `tether server`'s own --mcp-port defaults to 8899, and matching it here looks
// like the consistent thing to do. It would delete the distinction the check
// runs on. server.Config documents 0 as "use the default", so 0 means "nobody
// told doctor" and any other value means "the operator says this deployment
// uses that port" — which is what lets checkMCPLoopback know when it is
// guessing, and what gates the settings.json/port-conflict comparison in
// checkMCPSettingsInject. Defaulted to 8899, every host that serves MCP
// somewhere else would be told cc is wired to the wrong tether.
func TestDoctorFlags_MCPPortDefaultsToZeroMeaningUnspecified(t *testing.T) {
	f := newDoctorCmd().Flags().Lookup("mcp-port")
	if f == nil {
		t.Fatal("doctor has no --mcp-port flag")
	}
	if f.DefValue != "0" {
		t.Errorf("--mcp-port defaults to %q; it must default to 0 so an unspecified port stays distinguishable from a specified 8899", f.DefValue)
	}
}
