// Package main is the entry point for the SuitCode CLI client.
//
// The CLI client communicates with a running coordinator (or directly with an
// investigator HTTP server) to retrieve repository intelligence without
// requiring a local copy of the source tree.
//
// This component is NOT YET IMPLEMENTED. Run the binary to see the notice.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "SuitCode CLI client: not implemented yet")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The CLI client is planned for a future release.")
	fmt.Fprintln(os.Stderr, "To query an investigator directly, start its HTTP server:")
	fmt.Fprintln(os.Stderr, "  investigator <repo-path> serve [--port 7878]")
	os.Exit(1)
}
