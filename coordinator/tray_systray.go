//go:build systray

package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

//go:embed assets/icon.png
var iconPNG []byte

const (
	trayPollInterval = 5 * time.Second
	maxProjectSlots  = 8
)

// runTray starts the system-tray icon on the main goroutine. It blocks until
// the tray is dismissed or ctx is cancelled. cancel is called just before
// returning so the coordinator shutdown sequence sees a done context regardless
// of whether the tray was dismissed by a signal or by the user clicking Quit.
//
// On Linux, if no display server is detected the function returns immediately
// with a warning rather than crashing.
func runTray(ctx context.Context, coordinatorURL string, cancel context.CancelFunc) {
	if runtime.GOOS == "linux" {
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			logf("warn: no display server detected (DISPLAY/WAYLAND_DISPLAY unset) — tray icon disabled")
			return
		}
	}

	client := coord.NewClient(coordinatorURL, "")
	t := newTray(ctx, client)
	t.run() // Blocks on the main goroutine until systray exits.

	// Ensure ctx is cancelled so the coordinator shutdown path runs even if
	// the tray exited without a SIGTERM/SIGINT (e.g. user clicked Quit).
	cancel()
}

// ── Tray ─────────────────────────────────────────────────────────────────────

// tray manages the system-tray icon lifecycle.
type tray struct {
	ctx    context.Context
	client *coord.Client
	menu   *trayMenu
	poller *trayPoller
}

func newTray(ctx context.Context, client *coord.Client) *tray {
	return &tray{ctx: ctx, client: client}
}

// run starts the systray event loop. Blocks until the tray is quit.
// Must be called from the main goroutine (systray requirement).
func (t *tray) run() {
	systray.Run(t.onReady, t.onExit)
}

// onReady is invoked by systray once the native tray is initialised.
func (t *tray) onReady() {
	systray.SetTitle("SuitCode")
	systray.SetTooltip("SuitCode — repository intelligence")

	// Set the icon after a short delay. On Windows, Shell_NotifyIcon(NIM_MODIFY)
	// can fail with ERROR_SUCCESS if called immediately after NIM_ADD —
	// the shell notification area isn't fully ready yet. Deferring by 200 ms
	// avoids the spurious "unable to set icon" log message.
	go func() {
		time.Sleep(200 * time.Millisecond)
		systray.SetIcon(trayIcon())
	}()

	// Build menu structure.
	t.menu = newTrayMenu(t.ctx, t.client)
	t.menu.build()

	// Start background poller.
	t.poller = newTrayPoller(t.ctx, t.client, t.menu)
	go t.poller.run()

	// Quit systray when the context is cancelled (SIGTERM / Ctrl-C).
	go func() {
		<-t.ctx.Done()
		systray.Quit()
	}()
}

// onExit is called by systray just before the native icon is removed.
func (t *tray) onExit() {
	if t.poller != nil {
		t.poller.stop()
	}
}

// ── Menu ─────────────────────────────────────────────────────────────────────

// trayMenu manages the system-tray drop-down. All public methods are safe to
// call from any goroutine after build() completes.
type trayMenu struct {
	ctx    context.Context
	client *coord.Client

	// mStatus is a permanently disabled item showing coordinator connectivity.
	mStatus *systray.MenuItem

	// slots are pre-allocated project rows; unused ones are hidden.
	slots [maxProjectSlots]*projectSlot

	// mQuit triggers graceful coordinator shutdown.
	mQuit *systray.MenuItem
}

// projectSlot is one pre-allocated row in the tray menu for a single project.
type projectSlot struct {
	item *systray.MenuItem

	mu          sync.Mutex
	projectPath string // empty when the slot is hidden
}

func newTrayMenu(ctx context.Context, client *coord.Client) *trayMenu {
	return &trayMenu{ctx: ctx, client: client}
}

// build allocates all menu items. Must be called from onReady (main goroutine).
func (m *trayMenu) build() {
	// Status row — always visible, never clickable.
	m.mStatus = systray.AddMenuItem("SuitCode — connecting...", "Coordinator connection status")
	m.mStatus.Disable()

	// Pre-allocate project slots, all initially hidden.
	for i := range m.slots {
		item := systray.AddMenuItem("", "")
		item.Hide()
		s := &projectSlot{item: item}
		m.slots[i] = s
		go m.runSlotHandler(s)
	}

	// Separator then Quit.
	systray.AddSeparator()
	m.mQuit = systray.AddMenuItem("Quit", "Stop the SuitCode coordinator")

	go func() {
		for range m.mQuit.ClickedCh {
			systray.Quit()
		}
	}()
}

// updateStatus sets the status row text. Safe from any goroutine.
func (m *trayMenu) updateStatus(text string) {
	if m.mStatus != nil {
		m.mStatus.SetTitle(text)
	}
}

// updateProjects refreshes the project slots to match the given list.
// Slots beyond maxProjectSlots are silently dropped. Safe from any goroutine.
func (m *trayMenu) updateProjects(projects []coord.ProjectInfo) {
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
func (m *trayMenu) runSlotHandler(s *projectSlot) {
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

			// Clear the slot immediately; the poller will sync on the next cycle.
			s.setProject("")
			m.refreshStatus()

			if err := m.client.StopProject(m.ctx, path); err != nil {
				logf("warn: tray: stop %s: %v", path, err)
			}
		}
	}
}

// refreshStatus recomputes the status text from currently visible slots.
func (m *trayMenu) refreshStatus() {
	count := 0
	for _, s := range m.slots {
		if s.getProject() != "" {
			count++
		}
	}
	m.updateStatus(fmt.Sprintf("Coordinator: online · %d project(s)", count))
}

// setProject updates the slot to display the given path, or hides it if empty.
func (s *projectSlot) setProject(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.projectPath = path

	if path == "" {
		s.item.Hide()
		return
	}

	s.item.SetTitle("Stop  " + filepath.Base(path))
	s.item.SetTooltip(path)
	s.item.Show()
}

func (s *projectSlot) getProject() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectPath
}

// ── Poller ────────────────────────────────────────────────────────────────────

// trayPoller periodically fetches coordinator and investigator state and pushes
// updates to the trayMenu.
type trayPoller struct {
	ctx    context.Context
	client *coord.Client
	menu   *trayMenu
	stopCh chan struct{}
}

func newTrayPoller(ctx context.Context, client *coord.Client, menu *trayMenu) *trayPoller {
	return &trayPoller{
		ctx:    ctx,
		client: client,
		menu:   menu,
		stopCh: make(chan struct{}),
	}
}

// run polls the coordinator on a fixed interval. Call in a dedicated goroutine.
func (p *trayPoller) run() {
	// Poll immediately on start-up, then on every tick.
	p.poll()

	ticker := time.NewTicker(trayPollInterval)
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

func (p *trayPoller) stop() {
	close(p.stopCh)
}

func (p *trayPoller) poll() {
	ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
	defer cancel()

	health, err := p.client.GetHealth(ctx)
	if err != nil {
		p.menu.updateStatus("Coordinator: offline")
		p.menu.updateProjects(nil)
		return
	}

	projectsResp, err := p.client.GetProjects(ctx)
	if err != nil {
		p.menu.updateStatus(fmt.Sprintf("Coordinator: online · %d project(s)", health.Projects))
		p.menu.updateProjects(nil)
		return
	}

	count := len(projectsResp.Projects)
	p.menu.updateStatus(fmt.Sprintf("Coordinator: online · %d project(s)", count))
	p.menu.updateProjects(projectsResp.Projects)
}

// ── Icon ──────────────────────────────────────────────────────────────────────

// trayIconSize is the edge length of the PNG passed to systray. Windows will
// use this as the HICON source; 256 px gives crisp rendering at every DPI.
const trayIconSize = 256

// trayIcon decodes the embedded assets/icon.png, down-samples it to
// trayIconSize × trayIconSize using a box filter, and returns the result as
// a fresh PNG byte slice. The original source may be any size ≥ trayIconSize.
func trayIcon() []byte {
	src, _, err := image.Decode(bytes.NewReader(iconPNG))
	if err != nil {
		// Fallback: return the raw embedded bytes unchanged.
		return iconPNG
	}

	// Down-sample only if the source is larger than the target.
	srcBounds := src.Bounds()
	sw, sh := srcBounds.Dx(), srcBounds.Dy()
	if sw <= trayIconSize && sh <= trayIconSize {
		return iconPNG
	}

	// Box-filter: for each output pixel, average every source pixel that falls
	// inside its mapped source rectangle. Gives much better quality than
	// nearest-neighbour for large reductions (e.g. 1080 → 256).
	dst := image.NewNRGBA(image.Rect(0, 0, trayIconSize, trayIconSize))
	scaleX := float64(sw) / trayIconSize
	scaleY := float64(sh) / trayIconSize

	for oy := 0; oy < trayIconSize; oy++ {
		sy0 := int(float64(oy) * scaleY)
		sy1 := int(float64(oy+1) * scaleY)
		if sy1 >= sh {
			sy1 = sh - 1
		}

		for ox := 0; ox < trayIconSize; ox++ {
			sx0 := int(float64(ox) * scaleX)
			sx1 := int(float64(ox+1) * scaleX)
			if sx1 >= sw {
				sx1 = sw - 1
			}

			// Accumulate linear RGBA over the source box.
			var rSum, gSum, bSum, aSum float64
			n := 0
			for sy := sy0; sy <= sy1; sy++ {
				for sx := sx0; sx <= sx1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					rSum += float64(cr >> 8)
					gSum += float64(cg >> 8)
					bSum += float64(cb >> 8)
					aSum += float64(ca >> 8)
					n++
				}
			}

			if n > 0 {
				fn := float64(n)
				dst.SetNRGBA(ox, oy, color.NRGBA{
					R: uint8(rSum / fn),
					G: uint8(gSum / fn),
					B: uint8(bSum / fn),
					A: uint8(aSum / fn),
				})
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return iconPNG
	}
	return buf.Bytes()
}
