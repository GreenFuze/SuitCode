//go:build !(windows && systray)

package main

// showErrorDialog is a no-op on non-Windows or non-systray builds.
// Error messages are written to stderr via logf instead.
func showErrorDialog(_ string) {}
