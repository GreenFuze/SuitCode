package main

import (
	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

// CoordinatorClient is a type alias for the shared coordinator.Client.
// Existing call sites in main.go continue to work unchanged.
type CoordinatorClient = coord.Client

// CoordinatorHealthResponse is a type alias for the shared health response type.
type CoordinatorHealthResponse = coord.HealthResponse

// NewCoordinatorClient wraps coordinator.NewClient.
// Kept for compatibility with existing call sites in main.go.
func NewCoordinatorClient(coordinatorURL, projectPath string) *CoordinatorClient {
	return coord.NewClient(coordinatorURL, projectPath)
}

// ensureCoordinator delegates to the shared coordinator package.
func ensureCoordinator(coordinatorURL string) error {
	return coord.EnsureRunning(coordinatorURL)
}

// readyClient ensures the coordinator is running and returns a configured
// CoordinatorClient for the given project path.
func readyClient(repoPath string) (*CoordinatorClient, error) {
	if err := coord.EnsureRunning(defaultCoordinatorURL); err != nil {
		return nil, err
	}
	return coord.NewClient(defaultCoordinatorURL, repoPath), nil
}
