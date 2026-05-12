// Package main is the entry point for the SuitCode coordinator service.
//
// The coordinator acts as a central registry of investigator instances,
// routes client requests, and aggregates results across repositories.
//
// This component is NOT YET IMPLEMENTED. Run the binary to see the notice.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "SuitCode coordinator: not implemented yet")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The coordinator is planned for a future release.")
	fmt.Fprintln(os.Stderr, "In the meantime, run the investigator directly:")
	fmt.Fprintln(os.Stderr, "  investigator <repo-path> <command> [flags]")
	os.Exit(1)
}
