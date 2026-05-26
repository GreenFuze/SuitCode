//go:build systray

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/GreenFuze/SuitCode/calllog"
	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

//go:embed assets/icon.png
var iconPNG []byte

const (
	trayPollInterval = 5 * time.Second

	// maxProjectSlots controls how many investigators can be shown in the
	// tray menu simultaneously. Each slot is one top-level sub-menu item, so
	// adding more than ~6 makes the menu tall; 4 is a comfortable default.
	maxProjectSlots = 4
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

	// Pre-compute the icon once (box-filter is not free).
	icon := trayIcon()

	// Set the icon immediately, then retry after the shell has settled.
	// On Windows, Shell_NotifyIcon(NIM_MODIFY) can return ERROR_SUCCESS (i.e.
	// succeed but be a no-op) if the notification area hasn't fully registered
	// the new icon entry yet. A second call after ~1 s ensures the HICON lands.
	systray.SetIcon(icon)
	go func() {
		time.Sleep(1 * time.Second)
		systray.SetIcon(icon)
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

	// mNoProjects is shown when no investigator projects are active. It is
	// hidden whenever at least one project slot is visible.
	mNoProjects *systray.MenuItem

	// slots are pre-allocated flat groups of menu items; unused ones are hidden.
	slots [maxProjectSlots]*projectSlot

	// mQuit triggers graceful coordinator shutdown.
	mQuit *systray.MenuItem
}

// projectSlot is one pre-allocated sub-menu entry for an active investigator.
//
//	[mParent  ] ← top-level item; hovering it reveals a fly-out sub-menu (▶)
//	  [mCopyLog] ← "Copy Coordinator Log"
//	  [mCopyMet] ← "Copy Metrics Summary"
//	  [mCopyCall]← "Copy Call Log"
//	  [mOpenDir] ← "Open Project Folder"
//	  [mStop   ] ← "Stop Investigator"
//
// ORDERING CONSTRAINT (fyne.io/systray Windows backend):
// convertToSubMenu calls SetMenuItemInfo to attach a child HMENU to mParent.
// This only succeeds while mParent is present in the root HMENU. Calling
// mParent.Hide() before AddSubMenuItem removes mParent from the HMENU first,
// so SetMenuItemInfo fails silently and the item stays a plain button (no ▶).
//
// Fix applied in build(): add ALL sub-items first (mParent is still in the
// root HMENU), then call mParent.Hide(). The child HMENU is already attached
// and survives Hide/Show cycles correctly thereafter.
type projectSlot struct {
	mParent   *systray.MenuItem // top-level sub-menu trigger; title = project path
	mCopyLog  *systray.MenuItem // sub-item: "Copy Coordinator Log"
	mCopyMet  *systray.MenuItem // sub-item: "Copy Metrics Summary"
	mCopyCall *systray.MenuItem // sub-item: "Copy Call Log"
	mOpenDir  *systray.MenuItem // sub-item: "Open Project Folder"
	mStop     *systray.MenuItem // sub-item: "Stop Investigator"

	mu          sync.Mutex
	projectPath string          // empty when slot is hidden
	projectInfo coord.ProjectInfo // last polled state
}

func newTrayMenu(ctx context.Context, client *coord.Client) *trayMenu {
	return &trayMenu{ctx: ctx, client: client}
}

// build allocates all menu items. Must be called from onReady (main goroutine).
func (m *trayMenu) build() {
	// Status row — always visible, never clickable.
	m.mStatus = systray.AddMenuItem("SuitCode — connecting...", "Coordinator connection status")
	m.mStatus.Disable()

	// Placeholder shown when no investigators are running.
	m.mNoProjects = systray.AddMenuItem("No active projects — run 'suitcode <path> warmup'", "")
	m.mNoProjects.Disable()

	// Pre-allocate project slots. Each slot is a sub-menu item: the parent
	// appears as a top-level entry with a ▶ arrow; hovering reveals the
	// fly-out with the four action items.
	//
	// CRITICAL ORDER: AddSubMenuItem must be called while mParent is still
	// present in the root HMENU (i.e., before mParent.Hide()). See the
	// projectSlot comment for the full explanation.
	for i := range m.slots {
		// Add parent to the root HMENU first (still visible at this point).
		mParent := systray.AddMenuItem("(no project)", "Active investigator project")

		// Add sub-items while mParent is in the root HMENU — this is what
		// makes convertToSubMenu's SetMenuItemInfo call succeed on Windows.
		mCopyLog  := mParent.AddSubMenuItem("Copy Coordinator Log", "Copy the coordinator log file to the clipboard")
		mCopyMet  := mParent.AddSubMenuItem("Copy Metrics Summary", "Copy the condensed session summary (errors, warnings, latency) to the clipboard")
		mCopyCall := mParent.AddSubMenuItem("Copy Call Log", "Copy the per-call detail log with seeds and limitation kinds to the clipboard")
		mOpenDir  := mParent.AddSubMenuItem("Open Project Folder", "Open the project directory in the system file manager")
		mStop     := mParent.AddSubMenuItem("Stop Investigator", "Terminate the investigator process for this project")

		// Hide AFTER sub-items are attached so the child HMENU is wired up.
		mParent.Hide()

		s := &projectSlot{
			mParent:   mParent,
			mCopyLog:  mCopyLog,
			mCopyMet:  mCopyMet,
			mCopyCall: mCopyCall,
			mOpenDir:  mOpenDir,
			mStop:     mStop,
		}
		m.slots[i] = s
		go m.runSlotHandler(s)
	}

	// Separator then Quit at the bottom.
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
			s.setProjectInfo(projects[i])
		} else {
			s.clearProject()
		}
	}

	// Show the "no active projects" placeholder only when every slot is empty.
	if m.mNoProjects == nil {
		return
	}
	if len(projects) == 0 {
		m.mNoProjects.Show()
	} else {
		m.mNoProjects.Hide()
	}
}

// runSlotHandler loops over action-item click events for one project slot.
// Each slot has a dedicated goroutine so blocking operations (clipboard
// writes, HTTP calls) do not stall other slots or the tray event loop.
func (m *trayMenu) runSlotHandler(s *projectSlot) {
	for {
		select {
		case <-m.ctx.Done():
			return

		case _, ok := <-s.mCopyLog.ClickedCh:
			if !ok {
				return
			}
			m.handleCopyLog(s)

		case _, ok := <-s.mCopyMet.ClickedCh:
			if !ok {
				return
			}
			m.handleCopyMetrics(s)

		case _, ok := <-s.mCopyCall.ClickedCh:
			if !ok {
				return
			}
			m.handleCopyCallLog(s)

		case _, ok := <-s.mOpenDir.ClickedCh:
			if !ok {
				return
			}
			m.handleOpenFolder(s)

		case _, ok := <-s.mStop.ClickedCh:
			if !ok {
				return
			}
			m.handleStop(s)
		}
	}
}

// ── Slot action handlers ──────────────────────────────────────────────────────

// handleCopyLog reads the coordinator log file and sends it to the clipboard.
func (m *trayMenu) handleCopyLog(_ *projectSlot) {
	content, err := readCoordinatorLog()
	if err != nil {
		logf("tray: copy log: %v", err)
		return
	}
	if err := copyToClipboard(content); err != nil {
		logf("tray: copy log to clipboard: %v", err)
	}
}

// handleCopyMetrics formats the project's status and metrics as plain text,
// fetches the current readiness level from the investigator, and copies the
// result to the clipboard.
func (m *trayMenu) handleCopyMetrics(s *projectSlot) {
	s.mu.Lock()
	info := s.projectInfo
	s.mu.Unlock()

	if info.ProjectPath == "" {
		return
	}

	text := formatProjectMetrics(info)
	if err := copyToClipboard(text); err != nil {
		logf("tray: copy metrics to clipboard: %v", err)
	}
}

// handleCopyCallLog formats the per-call detail log (seeds, tok/budget, latency,
// limitation kinds) and copies the result to the clipboard.
func (m *trayMenu) handleCopyCallLog(s *projectSlot) {
	s.mu.Lock()
	info := s.projectInfo
	s.mu.Unlock()

	if info.ProjectPath == "" {
		return
	}

	logger, err := calllog.New(info.ProjectPath)
	if err != nil {
		logf("tray: copy call log: calllog.New: %v", err)
		return
	}

	var sb strings.Builder
	if err := logger.PrintCallLog(&sb, 100); err != nil {
		logf("tray: copy call log: %v", err)
		return
	}

	if err := copyToClipboard(sb.String()); err != nil {
		logf("tray: copy call log to clipboard: %v", err)
	}
}

// handleOpenFolder opens the project directory in the system file manager.
func (m *trayMenu) handleOpenFolder(s *projectSlot) {
	path := s.getProject()
	if path == "" {
		return
	}
	if err := openFolder(path); err != nil {
		logf("tray: open folder %s: %v", path, err)
	}
}

// handleStop stops the investigator for the slot's project. The slot is cleared
// immediately so the tray responds before the next poll cycle.
func (m *trayMenu) handleStop(s *projectSlot) {
	path := s.getProject()
	if path == "" {
		return
	}

	// Clear the slot immediately for responsive UX; the poller will confirm on
	// the next cycle.
	s.clearProject()
	m.refreshStatus()

	if err := m.client.StopProject(m.ctx, path); err != nil {
		logf("warn: tray: stop %s: %v", path, err)
	}
}

// refreshStatus recomputes the status text and no-projects placeholder from
// currently visible slots. Safe from any goroutine.
func (m *trayMenu) refreshStatus() {
	count := 0
	for _, s := range m.slots {
		if s.getProject() != "" {
			count++
		}
	}

	m.updateStatus(fmt.Sprintf("Coordinator: online | %d project(s)", count))

	if m.mNoProjects != nil {
		if count == 0 {
			m.mNoProjects.Show()
		} else {
			m.mNoProjects.Hide()
		}
	}
}

// ── projectSlot helpers ───────────────────────────────────────────────────────

// setProjectInfo updates the slot with live project data and reveals the
// sub-menu parent so the user can hover to access the action items.
func (s *projectSlot) setProjectInfo(info coord.ProjectInfo) {
	s.mu.Lock()
	s.projectPath = info.ProjectPath
	s.projectInfo = info
	s.mu.Unlock()

	if info.ProjectPath == "" {
		s.hideAll()
		return
	}

	// Update the parent item's title to the full project path and reveal it.
	s.mParent.SetTitle(info.ProjectPath)
	s.mParent.Show()
}

// clearProject hides the slot, removing it from the visible menu.
func (s *projectSlot) clearProject() {
	s.mu.Lock()
	s.projectPath = ""
	s.projectInfo = coord.ProjectInfo{}
	s.mu.Unlock()

	s.hideAll()
}

// hideAll hides the parent item, collapsing the entire sub-menu entry.
// Thread-safe (systray ops are channel-based); does not need the slot mutex.
func (s *projectSlot) hideAll() {
	s.mParent.Hide()
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
		p.menu.updateStatus(fmt.Sprintf("Coordinator: online | %d project(s)", health.Projects))
		p.menu.updateProjects(nil)
		return
	}

	count := len(projectsResp.Projects)
	p.menu.updateStatus(fmt.Sprintf("Coordinator: online | %d project(s)", count))
	p.menu.updateProjects(projectsResp.Projects)
}

// ── Clipboard & shell helpers ─────────────────────────────────────────────────

// copyToClipboard writes text to the system clipboard using a platform-native
// command-line utility (clip.exe on Windows, pbcopy on macOS, xclip/xsel on
// Linux). No external Go package is required.
func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("clip.exe")
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		// Prefer xclip; fall back to xsel when xclip is absent.
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		}
	}

	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipboard command failed: %w", err)
	}
	return nil
}

// openFolder opens path in the platform's default file manager.
func openFolder(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// coordinatorLogPath returns the path of the coordinator log file, or an empty
// string if the platform does not write one (non-Windows builds log to stderr).
func coordinatorLogPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "SuitCode", "coordinator.log")
}

// readCoordinatorLog returns the current contents of the coordinator log file.
func readCoordinatorLog() (string, error) {
	path := coordinatorLogPath()
	if path == "" {
		return "", fmt.Errorf("coordinator log is not written to a file on this platform")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read coordinator log: %w", err)
	}
	return string(data), nil
}

// formatProjectMetrics builds a human-readable summary of an investigator's
// status and session call statistics. It fetches the current readiness level
// from the investigator's health endpoint and reads the project's call log to
// produce a session summary via calllog.PrintAggregateSummary.
func formatProjectMetrics(info coord.ProjectInfo) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Project:   %s\n", info.ProjectPath))
	sb.WriteString(fmt.Sprintf("Port:      %d\n", info.Port))
	sb.WriteString(fmt.Sprintf("Started:   %s\n", info.StartedAt))

	// Fetch current readiness level directly from the investigator.
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
	httpClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpClient.Get(baseURL + "/api/v1/health")
	if err == nil && resp.Body != nil {
		defer resp.Body.Close()

		var health struct {
			ReadinessLevel int `json:"readiness_level"`
		}
		if json.NewDecoder(resp.Body).Decode(&health) == nil {
			sb.WriteString(fmt.Sprintf("Readiness: %d/3\n", health.ReadinessLevel))
		}
	}

	// Append session call summary from the project's call log.
	sb.WriteString("\n")
	logger, logErr := calllog.New(info.ProjectPath)
	if logErr == nil {
		if printErr := logger.PrintAggregateSummary(&sb, 0); printErr != nil {
			sb.WriteString(fmt.Sprintf("(call log unavailable: %v)\n", printErr))
		}
	} else {
		sb.WriteString(fmt.Sprintf("(call log unavailable: %v)\n", logErr))
	}

	return sb.String()
}

// ── Icon ──────────────────────────────────────────────────────────────────────

// trayIconSize is the pixel edge-length of the icon frame embedded in the ICO
// container. 32 is the Windows standard notification-area size (SM_CXICON at
// 96 DPI). Windows scales it for higher-DPI displays automatically.
const trayIconSize = 32

// trayIcon decodes the embedded assets/icon.png, box-filter-resizes it to
// trayIconSize × trayIconSize, and returns the result wrapped in an ICO
// container.
//
// WHY ICO? fyne.io/systray on Windows writes the bytes to a temp file then
// calls LoadImageW(IMAGE_ICON, LR_LOADFROMFILE). LoadImageW identifies the
// format by magic bytes, and IMAGE_ICON only accepts the ICO magic header
// (0x00 0x00 0x01 0x00). A raw PNG is silently rejected, yielding a null
// HICON and a blank tray slot. ICO frames can be either DIB or PNG data
// (detected automatically by the frame's magic bytes), so we produce a
// single-frame ICO that wraps our PNG.
func trayIcon() []byte {
	src, _, err := image.Decode(bytes.NewReader(iconPNG))
	if err != nil {
		logf("warn: tray: cannot decode icon: %v", err)
		return iconPNG
	}

	// Resize to trayIconSize × trayIconSize with a box filter.
	srcBounds := src.Bounds()
	sw, sh := srcBounds.Dx(), srcBounds.Dy()
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

			// Average all source pixels inside this output pixel's box.
			var rS, gS, bS, aS float64
			n := 0
			for sy := sy0; sy <= sy1; sy++ {
				for sx := sx0; sx <= sx1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					rS += float64(cr >> 8)
					gS += float64(cg >> 8)
					bS += float64(cb >> 8)
					aS += float64(ca >> 8)
					n++
				}
			}
			if n > 0 {
				fn := float64(n)
				dst.SetNRGBA(ox, oy, color.NRGBA{
					R: uint8(rS / fn),
					G: uint8(gS / fn),
					B: uint8(bS / fn),
					A: uint8(aS / fn),
				})
			}
		}
	}

	// Encode the resized image as PNG.
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, dst); err != nil {
		logf("warn: tray: cannot encode icon: %v", err)
		return iconPNG
	}

	// Wrap the PNG in a single-frame ICO container so LoadImageW accepts it.
	return pngToICO(pngBuf.Bytes(), trayIconSize, trayIconSize)
}

// pngToICO wraps raw PNG bytes in a minimal single-frame ICO container.
//
// ICO layout:
//
//	[0]  ICONDIR     (6 bytes)  — magic 0x00000100 + image count
//	[6]  ICONDIRENTRY (16 bytes) — dimensions, bit-depth, data size, data offset
//	[22] <pngBytes>             — the PNG frame (detected by its own magic bytes)
//
// The ICO spec allows image frames to be either DIB (BMP) or PNG data.
// Windows detects which by the frame's own magic bytes, so no special flag is
// needed — we just place the PNG bytes at the declared offset.
func pngToICO(pngBytes []byte, w, h int) []byte {
	const headerLen = 6
	const dirEntryLen = 16
	dataOffset := uint32(headerLen + dirEntryLen)
	dataLen := uint32(len(pngBytes))

	// Clamp width/height to the ICO directory byte field (0 encodes as 256).
	wb := byte(w)
	hb := byte(h)

	buf := make([]byte, headerLen+dirEntryLen+int(dataLen))

	// ICONDIR header.
	buf[0] = 0 // reserved
	buf[1] = 0 // reserved
	buf[2] = 1 // type: 1 = icon
	buf[3] = 0 //
	buf[4] = 1 // image count (low byte)
	buf[5] = 0 // image count (high byte)

	// ICONDIRENTRY.
	buf[6] = wb  // width
	buf[7] = hb  // height
	buf[8] = 0   // colour count (0 = >8-bit)
	buf[9] = 0   // reserved
	buf[10] = 1  // planes (low)
	buf[11] = 0  // planes (high)
	buf[12] = 32 // bits per pixel (low) — 32-bit RGBA
	buf[13] = 0  // bits per pixel (high)
	// data size (4 bytes LE)
	buf[14] = byte(dataLen)
	buf[15] = byte(dataLen >> 8)
	buf[16] = byte(dataLen >> 16)
	buf[17] = byte(dataLen >> 24)
	// data offset from start of file (4 bytes LE)
	buf[18] = byte(dataOffset)
	buf[19] = byte(dataOffset >> 8)
	buf[20] = byte(dataOffset >> 16)
	buf[21] = byte(dataOffset >> 24)

	copy(buf[22:], pngBytes)
	return buf
}
