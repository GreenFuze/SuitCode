package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

// logWriter is the destination for coordinator log lines. Defaults to stderr.
// Platform-specific init files may redirect it (e.g. to a log file on Windows
// when built with -tags systray, where no console is available).
var logWriter io.Writer = os.Stderr

// logf writes a timestamped line to logWriter with the [sc coordinator] prefix.
func logf(format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(logWriter, "[sc coordinator] %s %s\n", ts, msg)
}
