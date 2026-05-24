//go:build systray

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"fyne.io/systray"
	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

// maxProjectSlots is the maximum number of investigator projects that can be
// shown in the tray menu simultaneously. Items beyond this limit are silently
// omitted — a coordinator handling more than 8 projects simultaneously is
// an unusual deployment and the tray companion is a convenience tool.
const maxProjectSlots = 8

// Menu manages the system-tray drop-down. After Build() is called, all public
// methods are safe to call from any goroutine.
type Menu struct {
	ctx    context.Context
	client *coord.Client

	// mStatus is a permanently disabled item showing coordinator connectivity.
	mStatus *systray.MenuItem

	// slots are pre-allocated project rows. Unused slots are hidden.
	slots [maxProjectSlots]*projectSlot

	// mQuit triggers graceful tray exit.
	mQuit *systray.MenuItem
}

// projectSlot is one pre-allocated row in the tray menu for a single project.
type projectSlot struct {
	item *systray.MenuItem

	mu          sync.Mutex
	projectPath string // empty when hidden
}

// NewMenu constructs a Menu. Call Build() exactly once from onReady.
func NewMenu(ctx context.Context, client *coord.Client) *Menu {
	return &Menu{ctx: ctx, client: client}
}

// Build allocates all menu items. Must be called from onReady (main goroutine).
func (m *Menu) Build() {
	// Coordinator status — always visible, never clickable.
	m.mStatus = systray.AddMenuItem("SuitCode — connecting...", "Coordinator connection status")
	m.mStatus.Disable()

	// Pre-allocate project slots, all initially hidden.
	for i := range m.slots {
		item := systray.AddMenuItem("", "")
		item.Hide()
		s := &projectSlot{item: item}
		m.slots[i] = s

		// Each slot handles its own click in a dedicated goroutine.
		go m.runSlotHandler(s)
	}

	// Separator then Quit.
	systray.AddSeparator()
	m.mQuit = systray.AddMenuItem("Quit", "Stop the SuitCode tray companion")

	go func() {
		for range m.mQuit.ClickedCh {
			systray.Quit()
		}
	}()
}

// UpdateStatus updates the status menu item text. Safe to call from any goroutine.
func (m *Menu) UpdateStatus(text string) {
	if m.mStatus != nil {
		m.mStatus.SetTitle(text)
	}
}

// UpdateProjects refreshes the project slots to match the given list.
// Projects beyond maxProjectSlots are silently dropped.
// Safe to call from any goroutine.
func (m *Menu) UpdateProjects(projects []coord.ProjectInfo) {
	for i, s := range m.slots {
		if i < len(projects) {
			s.setProject(projects[i].ProjectPath)
		} else {
			s.setProject("")
		}
	}
}

// runSlotHandler loops over click events for one project slot. A click stops
// the associated investigator and clears the slot immediately for responsive UX.
func (m *Menu) runSlotHandler(s *projectSlot) {
	for {
		select {
		case <-m.ctx.Done():
			return
		case _, ok := <-s.item.ClickedCh:
			if !ok {
				return
			}
			path := s.getProject()
			if path == "" {
				continue
			}

			// Clear the slot immediately — the poller will sync on the next cycle.
			s.setProject("")
			m.refreshStatus()

			if err := m.client.StopProject(m.ctx, path); err != nil {
				fmt.Printf("suitcode-tray: stop %s: %v\n", path, err)
			}
		}
	}
}

// refreshStatus recomputes the status text from the currently visible slots.
// Called after a manual stop so the count updates without waiting for the poller.
func (m *Menu) refreshStatus() {
	count := 0
	for _, s := range m.slots {
		if s.getProject() != "" {
			count++
		}
	}
	m.UpdateStatus(fmt.Sprintf("Coordinator: online · %d project(s)", count))
}

// ──────────────────────────────────────────────────────────────────────────────
// projectSlot helpers
// ──────────────────────────────────────────────────────────────────────────────

// setProject updates the slot to display the given project path, or hides it
// if path is empty. Safe to call from any goroutine.
func (s *projectSlot) setProject(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.projectPath = path

	if path == "" {
		s.item.Hide()
		return
	}

	base := filepath.Base(path)
	s.item.SetTitle("Stop  " + base)
	s.item.SetTooltip(path)
	s.item.Show()
}

// getProject returns the project path currently associated with this slot.
func (s *projectSlot) getProject() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectPath
}
