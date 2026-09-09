//go:build tuiprobe

// Command tuiprobe runs pimctl's interactive picker and justification prompt in
// isolation so an expect(1) script can drive them through a pty.
//
// Built only with `-tags tuiprobe`; it is never part of the pimctl binary.
//
//	go build -tags tuiprobe -o /tmp/pimctl-tuiprobe ./cmd/tuiprobe
//	expect scripts/tui-filter-test.exp /tmp/pimctl-tuiprobe
package main

import (
	"fmt"
	"os"

	"github.com/larsakerlund/pimctl/internal/cli"
)

// main dispatches on the first argument: `select` drives the multi-select over
// a fixture shaped like the real tenant, `justification` drives the
// justification prompt with an optional prefill in the second argument.
// Anything else prints the usage and exits 2. A probe that returns an error —
// which includes the user cancelling — exits 1, so the expect script can assert
// on the outcome as well as on what was drawn.
func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	var err error
	switch mode {
	case "select":
		err = cli.ProbeSelect()
	case "justification":
		// os.Args[0] is the binary, [1] the mode, [2] the optional prefill.
		const prefillArg = 2
		prefill := ""
		if len(os.Args) > prefillArg {
			prefill = os.Args[prefillArg]
		}
		err = cli.ProbeJustification(prefill)
	default:
		fmt.Fprintln(os.Stderr, "usage: tuiprobe select | tuiprobe justification <prefill>")
		os.Exit(2)
	}
	if err != nil {
		os.Exit(1)
	}
}
