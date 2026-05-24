//go:build systray

package main

import (
	"context"
	"fmt"
	"time"

	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

const pollInterval = 5 * time.Second

// Poller periodically fetches coordinator and investigator state and pushes
// updates to the Menu.
type Poller struct {
	ctx    context.Context
	client *coord.Client
	menu   *Menu
	stopCh chan struct{}
}

// NewPoller constructs a Poller. Call Run() in a goroutine to start polling.
func NewPoller(ctx context.Context, client *coord.Client, menu *Menu) *Poller {
	return &Poller{
		ctx:    ctx,
		client: client,
		menu:   menu,
		stopCh: make(chan struct{}),
	}
}

// Run polls the coordinator on a fixed interval until the context is cancelled
// or Stop is called. Intended to run in a dedicated goroutine.
func (p *Poller) Run() {
	// Poll immediately on startup, then on every tick.
	p.poll()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

// Stop signals the poller to exit on the next iteration.
func (p *Poller) Stop() {
	close(p.stopCh)
}

// poll fetches coordinator health and project list, then updates the menu.
func (p *Poller) poll() {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()

	// Fetch coordinator health.
	health, err := p.client.GetHealth(ctx)
	if err != nil {
		p.menu.UpdateStatus("Coordinator: offline")
		p.menu.UpdateProjects(nil)
		return
	}

	// Fetch project list.
	projectsResp, err := p.client.GetProjects(ctx)
	if err != nil {
		p.menu.UpdateStatus(fmt.Sprintf("Coordinator: online · %d project(s)", health.Projects))
		p.menu.UpdateProjects(nil)
		return
	}

	// Update menu with fresh state.
	count := len(projectsResp.Projects)
	p.menu.UpdateStatus(fmt.Sprintf("Coordinator: online · %d project(s)", count))
	p.menu.UpdateProjects(projectsResp.Projects)
}
