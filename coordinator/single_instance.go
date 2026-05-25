package main

import (
	"fmt"
	"net/http"
	"time"
)

// checkAlreadyRunning does a quick HTTP health-check on the coordinator's port.
// Returns (true, url) if another coordinator instance is already responding;
// (false, "") otherwise.
//
// A 1 s timeout is deliberately short — startup must not block noticeably if
// nothing is listening on the port. We treat any non-200 response (including
// connection errors) as "port clear, safe to start".
func checkAlreadyRunning(port int) (bool, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port)
	client := &http.Client{Timeout: 1 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return false, "" // nothing listening
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return false, ""
}
