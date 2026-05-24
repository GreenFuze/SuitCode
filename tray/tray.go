//go:build systray

package main

import (
	"context"

	"fyne.io/systray"
	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

// Tray manages the system-tray icon lifecycle for the SuitCode companion.
type Tray struct {
	ctx    context.Context
	client *coord.Client
	menu   *Menu
	poller *Poller
}

// NewTray constructs a Tray for the given coordinator client.
func NewTray(ctx context.Context, client *coord.Client) *Tray {
	return &Tray{
		ctx:    ctx,
		client: client,
	}
}

// Run starts the system-tray event loop. Blocks until the tray is quit or the
// context is cancelled. Must be called from the main goroutine.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// onReady is invoked by systray once the native tray is initialised.
// It sets up the icon, builds the menu, and starts the background poller.
func (t *Tray) onReady() {
	systray.SetIcon(trayIcon())
	systray.SetTitle("SuitCode")
	systray.SetTooltip("SuitCode — repository intelligence")

	// Build the menu structure (all slots allocated, project slots hidden).
	t.menu = NewMenu(t.ctx, t.client)
	t.menu.Build()

	// Poller refreshes coordinator/investigator state every few seconds.
	t.poller = NewPoller(t.ctx, t.client, t.menu)
	go t.poller.Run()

	// Quit the tray when the context is cancelled (SIGTERM / Ctrl-C).
	go func() {
		<-t.ctx.Done()
		systray.Quit()
	}()
}

// onExit is called by systray just before the native tray icon is removed.
func (t *Tray) onExit() {
	if t.poller != nil {
		t.poller.Stop()
	}
}
