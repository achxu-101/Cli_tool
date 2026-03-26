package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"upgrador/internal/config"
	"upgrador/internal/resolver"
	"upgrador/internal/scanner"
	"upgrador/internal/upgrader"
)

// screen is the current TUI state.
type screen int

const (
	screenScan screen = iota
	screenGroupSelect
	screenComponentSelect
	screenConfirm
	screenUpgrade
	screenSummary
)

// scanDoneMsg carries the resolver results back to the bubbletea runtime.
type scanDoneMsg struct {
	results []resolver.Result
	err     error
}

// scanProgressMsg carries a status string from the scan goroutine.
type scanProgressMsg string

// scanProgressDoneMsg signals the progress channel is closed.
type scanProgressDoneMsg struct{}

// selfUpdateMsg carries an optional update notice.
type selfUpdateMsg struct{ notice string }

// styles
var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 2)

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("255"))

	styleGroupName = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("255"))

	styleCyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("51"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleGrey   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

// Model is the single bubbletea model for the entire application.
type Model struct {
	cfg     *config.Config
	screen  screen
	spinner spinner.Model
	dryRun  bool
	offline bool

	// scan results
	results        []resolver.Result
	scanErr        error
	groupSummary   []groupRow
	updateNotice   string // non-empty if a newer version is available
	scanStatus     string // current scan step shown while scanning
	scanProgressCh chan string

	// group selection (screen 2)
	selectedGroups []string
	groupForm      *huh.Form

	// component drill-down (screen 3)
	currentGroupIndex int
	compCursor        int
	compSelected      []bool
	confirmedResults  []resolver.Result

	// inline edit / offline version form
	editingIdx     int
	editStep       int
	editForm       *huh.Form
	editMethod     string
	editGithubRepo string

	// offline: collect target versions after ENTER
	offlineQueue      []int  // indices in current group needing a version
	offlineQueueIdx   int
	offlineForm       *huh.Form
	offlineVersionInput string

	// upgrade execution (screen 5)
	upgradeIdx     int
	upgradeReader  *io.PipeReader
	upgradeContent string
	vp             viewport.Model
	upgradeResults []upgradeResult

	// summary (screen 6)
	rebootRequired bool
}

type upgradeResult struct {
	name string
	err  error
}

type upgradeOutputMsg string
type upgradeDoneMsg struct{ err error }

// groupRow holds one row of the scan summary table.
type groupRow struct {
	name     string
	total    int
	outdated int
}

// New creates a ready-to-run Model.
func New(cfg *config.Config, dryRun, offline bool) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	return Model{
		cfg:            cfg,
		screen:         screenScan,
		spinner:        sp,
		dryRun:         dryRun,
		offline:        offline,
		editingIdx:     -1,
		scanProgressCh: make(chan string, 8),
	}
}

// Init starts the spinner, fires the background scan, and checks for updates.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.runScan(), m.checkSelfUpdate(), m.readScanProgress())
}

// runScan performs the scan (and resolve, unless offline) off the UI goroutine.
func (m Model) runScan() tea.Cmd {
	offline := m.offline
	cfg := m.cfg
	ch := m.scanProgressCh
	return func() tea.Msg {
		components := scanner.ScanAllWithProgress(cfg, func(status string) {
			ch <- status
		})
		var results []resolver.Result
		if offline {
			results = make([]resolver.Result, len(components))
			for i, c := range components {
				results[i] = resolver.Result{Component: c}
			}
		} else {
			ch <- "Resolving latest versions..."
			results = resolver.ResolveAll(components)
		}
		close(ch)
		return scanDoneMsg{results: results}
	}
}

// readScanProgress waits for the next progress message from the scan goroutine.
func (m Model) readScanProgress() tea.Cmd {
	ch := m.scanProgressCh
	return func() tea.Msg {
		status, ok := <-ch
		if !ok {
			return scanProgressDoneMsg{}
		}
		return scanProgressMsg(status)
	}
}

// checkSelfUpdate silently queries GitHub for a newer release.
func (m Model) checkSelfUpdate() tea.Cmd {
	return func() tea.Msg {
		notice := fetchUpdateNotice()
		return selfUpdateMsg{notice: notice}
	}
}

// Update handles messages and key presses.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle self-update notice at any screen.
	if u, ok := msg.(selfUpdateMsg); ok {
		m.updateNotice = u.notice
		return m, nil
	}

	switch m.screen {
	case screenScan:
		return m.updateScan(msg)
	case screenGroupSelect:
		return m.updateGroupSelect(msg)
	case screenComponentSelect:
		return m.updateComponentSelect(msg)
	case screenConfirm:
		return m.updateConfirm(msg)
	case screenUpgrade:
		return m.updateUpgrade(msg)
	case screenSummary:
		return m.updateSummary(msg)
	}
	return m, nil
}

func (m Model) updateScan(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter":
			if m.results != nil {
				return m.transitionToGroupSelect()
			}
		}

	case scanProgressMsg:
		m.scanStatus = string(msg)
		return m, m.readScanProgress()

	case scanProgressDoneMsg:
		return m, nil

	case scanDoneMsg:
		m.scanErr = msg.err
		m.results = msg.results
		m.groupSummary = buildGroupSummary(m.results)
		return m, nil

	case spinner.TickMsg:
		if m.results == nil {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m Model) updateGroupSelect(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "q" || k.String() == "ctrl+c") {
		return m, tea.Quit
	}

	form, cmd := m.groupForm.Update(msg)
	if f, ok := form.(*huh.Form); ok {
		m.groupForm = f
	}
	if m.groupForm.State == huh.StateCompleted {
		return m.transitionToComponentSelect()
	}
	return m, cmd
}

func (m Model) updateComponentSelect(msg tea.Msg) (tea.Model, tea.Cmd) {
	items := m.currentGroupResults()

	// Offline version-collection mode.
	if m.offlineForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" {
			return m, tea.Quit
		}
		form, cmd := m.offlineForm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.offlineForm = f
		}
		if m.offlineForm.State == huh.StateCompleted {
			return m.applyOfflineVersion(items)
		}
		return m, cmd
	}

	// Inline method-edit mode.
	if m.editingIdx >= 0 {
		if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" {
			return m, tea.Quit
		}
		form, cmd := m.editForm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.editForm = f
		}
		if m.editForm.State == huh.StateCompleted {
			var editCmd tea.Cmd
			m, editCmd = m.applyEdit(items)
			return m, editCmd
		}
		return m, cmd
	}

	// Normal browsing mode.
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.compCursor > 0 {
				m.compCursor--
			}
		case "down", "j":
			if m.compCursor < len(items)-1 {
				m.compCursor++
			}
		case " ":
			if m.compCursor < len(m.compSelected) {
				m.compSelected[m.compCursor] = !m.compSelected[m.compCursor]
			}
		case "e":
			if len(items) > 0 {
				var cmd tea.Cmd
				m, cmd = m.startEdit(items[m.compCursor])
				return m, cmd
			}
		case "enter":
			// Collect selected items for this group.
			var selected []int
			for i, r := range items {
				if i < len(m.compSelected) && m.compSelected[i] {
					m.confirmedResults = append(m.confirmedResults, r)
					if m.offline && r.Latest == "" {
						selected = append(selected, len(m.confirmedResults)-1)
					}
				}
			}
			// In offline mode, prompt for versions of selected items.
			if m.offline && len(selected) > 0 {
				m.offlineQueue = selected
				m.offlineQueueIdx = 0
				return m.startOfflineVersionPrompt()
			}
			return m.advanceGroup()
		}
	}
	return m, nil
}

// advanceGroup moves to the next group or to screenConfirm.
func (m Model) advanceGroup() (tea.Model, tea.Cmd) {
	m.currentGroupIndex++
	if m.currentGroupIndex >= len(m.selectedGroups) {
		m.screen = screenConfirm
		return m, nil
	}
	m = m.initCompSelected()
	return m, nil
}

// startOfflineVersionPrompt shows a huh Input for the next queued item.
func (m Model) startOfflineVersionPrompt() (tea.Model, tea.Cmd) {
	if m.offlineQueueIdx >= len(m.offlineQueue) {
		m.offlineForm = nil
		return m.advanceGroup()
	}
	idx := m.offlineQueue[m.offlineQueueIdx]
	name := m.confirmedResults[idx].Component.Name
	m.offlineVersionInput = ""
	m.offlineForm = huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(fmt.Sprintf("Target version for %s (e.g. v1.2.3):", name)).
			Value(&m.offlineVersionInput),
	))
	return m, m.offlineForm.Init()
}

// applyOfflineVersion stores the entered version and advances the queue.
func (m Model) applyOfflineVersion(items []resolver.Result) (tea.Model, tea.Cmd) {
	_ = items
	if m.offlineQueueIdx < len(m.offlineQueue) {
		idx := m.offlineQueue[m.offlineQueueIdx]
		m.confirmedResults[idx].Latest = strings.TrimSpace(m.offlineVersionInput)
	}
	m.offlineQueueIdx++
	m.offlineForm = nil
	return m.startOfflineVersionPrompt()
}

// View renders the current screen.
func (m Model) View() string {
	switch m.screen {
	case screenScan:
		return m.viewScan()
	case screenGroupSelect:
		return m.viewGroupSelect()
	case screenComponentSelect:
		return m.viewComponentSelect()
	case screenConfirm:
		return m.viewConfirm()
	case screenUpgrade:
		return m.viewUpgrade()
	case screenSummary:
		return m.viewSummary()
	default:
		return "Screen not yet implemented.\n"
	}
}

// ── Screen 1: Scan ────────────────────────────────────────────────────────────

func (m Model) viewScan() string {
	if m.results == nil {
		status := m.scanStatus
		if status == "" {
			status = "Scanning your environment..."
		}
		return "\n  " + m.spinner.View() + "  " + status + "\n"
	}

	if m.scanErr != nil {
		return fmt.Sprintf("\n  Error during scan: %v\n\nPress q to quit.\n", m.scanErr)
	}

	out := styleBorder.Render(m.renderScanTable())
	if m.updateNotice != "" {
		out = styleYellow.Render("ℹ  "+m.updateNotice) + "\n" + out
	}
	return out
}

func (m Model) renderScanTable() string {
	var b strings.Builder

	title := "upgrador"
	if m.dryRun {
		title += "  " + styleYellow.Render("[DRY RUN]")
	}
	if m.offline {
		title += "  " + styleDim.Render("[OFFLINE]")
	}
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n\n")

	header := fmt.Sprintf("  %-18s %-8s %-12s %s", "Group", "Total", "Outdated", "Status")
	b.WriteString(styleGrey.Render(header))
	b.WriteString("\n")
	b.WriteString(styleGrey.Render("  " + strings.Repeat("─", 52)))
	b.WriteString("\n")

	for _, row := range m.groupSummary {
		outdatedStr := fmt.Sprintf("%d", row.outdated)
		var status string
		if row.outdated > 0 {
			outdatedStr = styleYellow.Render(outdatedStr)
			status = styleYellow.Render("⚠ action")
		} else {
			status = styleGreen.Render("✓ up to date")
		}
		line := fmt.Sprintf("  %-18s %-8s %-12s %s",
			styleGroupName.Render(row.name),
			fmt.Sprintf("%d", row.total),
			outdatedStr,
			status,
		)
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(styleDim.Render("  Press ENTER to continue, q to quit"))
	return b.String()
}

func buildGroupSummary(results []resolver.Result) []groupRow {
	order := []string{"OS", "Binaries", "Services", "Helm Charts"}
	totals := make(map[string]int, 4)
	outdateds := make(map[string]int, 4)

	for _, r := range results {
		g := r.Component.Group
		totals[g]++
		if r.IsOutdated {
			outdateds[g]++
		}
	}

	rows := make([]groupRow, 0, len(order))
	for _, name := range order {
		if totals[name] == 0 {
			continue
		}
		rows = append(rows, groupRow{name: name, total: totals[name], outdated: outdateds[name]})
	}
	return rows
}

// ── Screen 2: Group selection ─────────────────────────────────────────────────

func (m Model) transitionToGroupSelect() (tea.Model, tea.Cmd) {
	var options []huh.Option[string]
	var defaults []string
	for _, row := range m.groupSummary {
		if row.outdated > 0 || m.offline {
			label := fmt.Sprintf("%-16s (%d outdated)", row.name, row.outdated)
			options = append(options, huh.NewOption(label, row.name))
			if row.outdated > 0 {
				defaults = append(defaults, row.name)
			}
		}
	}

	m.selectedGroups = defaults
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select groups to upgrade").
				Description("SPACE to toggle, ENTER to confirm, q to quit").
				Options(options...).
				Value(&m.selectedGroups),
		),
	)
	m.groupForm = form
	m.screen = screenGroupSelect
	return m, form.Init()
}

func (m Model) viewGroupSelect() string {
	if m.groupForm == nil {
		return ""
	}
	return m.groupForm.View()
}

// ── Screen 3: Component drill-down ───────────────────────────────────────────

func (m Model) transitionToComponentSelect() (tea.Model, tea.Cmd) {
	m.currentGroupIndex = 0
	m.editingIdx = -1
	m.confirmedResults = nil
	m.offlineForm = nil
	m.screen = screenComponentSelect
	m = m.initCompSelected()
	return m, nil
}

func (m Model) initCompSelected() Model {
	items := m.currentGroupResults()
	m.compSelected = make([]bool, len(items))
	m.compCursor = 0
	for i, r := range items {
		if m.offline {
			m.compSelected[i] = true // pre-select all in offline mode
		} else {
			m.compSelected[i] = r.IsOutdated
		}
	}
	return m
}

func (m Model) currentGroupResults() []resolver.Result {
	if m.currentGroupIndex >= len(m.selectedGroups) {
		return nil
	}
	group := m.selectedGroups[m.currentGroupIndex]
	var items []resolver.Result
	for _, r := range m.results {
		if r.Component.Group == group {
			items = append(items, r)
		}
	}
	return items
}

func (m Model) startEdit(r resolver.Result) (Model, tea.Cmd) {
	m.editingIdx = m.compCursor
	m.editStep = 0
	m.editMethod = r.Component.Method
	if m.editMethod == "" || r.Component.IsUnknown {
		m.editMethod = "github_tarball"
	}
	m.editGithubRepo = r.Component.GithubRepo

	m.editForm = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(fmt.Sprintf("Edit upgrade method for: %s", r.Component.Name)).
			Options(
				huh.NewOption("GitHub releases (tarball)", "github_tarball"),
				huh.NewOption("GitHub releases (single binary)", "github_binary"),
				huh.NewOption("Apt package", "apt"),
				huh.NewOption("Custom script/URL", "custom_script"),
				huh.NewOption("Skip — manage manually", "skip"),
			).
			Value(&m.editMethod),
	))
	return m, m.editForm.Init()
}

func (m Model) applyEdit(items []resolver.Result) (Model, tea.Cmd) {
	if m.editStep == 0 && (m.editMethod == "github_tarball" || m.editMethod == "github_binary") {
		m.editStep = 1
		m.editForm = huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("GitHub repo (e.g. org/reponame)").
				Value(&m.editGithubRepo),
		))
		return m, m.editForm.Init()
	}

	if m.editingIdx < len(items) {
		comp := items[m.editingIdx].Component
		ub := config.UserBinary{
			Name:       comp.Name,
			Method:     m.editMethod,
			GithubRepo: m.editGithubRepo,
			BinaryPath: comp.BinaryPath,
			Overridden: comp.IsKnown,
			AddedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		_ = m.cfg.SetBinary(ub)

		for i, r := range m.results {
			if r.Component.Name == comp.Name {
				m.results[i].Component.Method = m.editMethod
				m.results[i].Component.GithubRepo = m.editGithubRepo
				m.results[i].Component.IsUnknown = false
				m.results[i].Component.IsKnown = true
				break
			}
		}
	}

	m.editingIdx = -1
	m.editForm = nil
	return m, nil
}

func (m Model) viewComponentSelect() string {
	// Offline version prompt overlay.
	if m.offlineForm != nil {
		return m.offlineForm.View()
	}
	// Method-edit overlay.
	if m.editingIdx >= 0 && m.editForm != nil {
		return m.editForm.View()
	}

	items := m.currentGroupResults()
	if len(items) == 0 {
		return styleBorder.Render(styleDim.Render("No components found.\n\nPress ENTER to continue."))
	}

	group := m.selectedGroups[m.currentGroupIndex]
	total := len(m.selectedGroups)
	idx := m.currentGroupIndex + 1

	var b strings.Builder
	barRight := strings.Repeat("─", max(0, 52-len(group)-16))
	b.WriteString(styleTitle.Render(fmt.Sprintf("── %s (%d of %d groups) %s", group, idx, total, barRight)))
	b.WriteString("\n")

	hint := "(SPACE to toggle, ENTER to confirm, e to edit method, q to quit)"
	if m.offline {
		hint = "(SPACE to toggle, ENTER to confirm + enter versions, e to edit method, q to quit)"
	}
	b.WriteString(styleDim.Render("Select components to upgrade:"))
	b.WriteString("\n")
	b.WriteString(styleDim.Render(hint))
	b.WriteString("\n\n")

	for i, r := range items {
		prefix := "  "
		if i == m.compCursor {
			prefix = "> "
		}

		selected := i < len(m.compSelected) && m.compSelected[i]
		var check string
		if r.Component.IsUnknown {
			check = styleYellow.Render("[?]")
		} else if selected {
			check = styleGreen.Render("[✓]")
		} else {
			check = "[ ]"
		}

		namePad := fmt.Sprintf("%-22s", r.Component.Name)
		var name string
		if i == m.compCursor {
			name = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Underline(true).Render(namePad)
		} else {
			name = styleGroupName.Render(namePad)
		}

		current := styleGrey.Render(fmt.Sprintf("%-10s", r.Component.Current))
		arrow := styleGrey.Render("→")

		var latest string
		if m.offline {
			latest = styleDim.Render(fmt.Sprintf("%-10s", "(enter on confirm)"))
		} else if r.Component.IsUnknown || r.Latest == "" {
			latest = styleYellow.Render(fmt.Sprintf("%-10s", "???"))
		} else {
			latest = styleCyan.Render(fmt.Sprintf("%-10s", r.Latest))
		}

		ml := methodLabel(r.Component.Method, r.Component.IsUnknown)
		var methodTag string
		if r.Component.IsUnknown {
			methodTag = styleYellow.Render(fmt.Sprintf("[%s]", ml))
		} else {
			methodTag = styleGrey.Render(fmt.Sprintf("[%s]", ml))
		}

		fmt.Fprintf(&b, "%s%s %s  %s  %s  %s  %s\n",
			prefix, check, name, current, arrow, latest, methodTag)
	}

	return styleBorder.Render(b.String())
}

func methodLabel(method string, isUnknown bool) string {
	if isUnknown {
		return "unknown — press e"
	}
	switch method {
	case "github_tarball":
		return "github tarball"
	case "github_binary":
		return "github binary"
	case "apt":
		return "apt"
	case "custom_script":
		return "custom script"
	case "helm_upgrade":
		return "helm upgrade"
	case "k3s_script":
		return "k3s script"
	case "helm_script":
		return "helm script"
	case "rancher_script":
		return "rancher script"
	case "skip":
		return "skip"
	default:
		return method
	}
}

// ── Screen 4: Confirmation ────────────────────────────────────────────────────

var groupOrder = []string{"OS", "Binaries", "Services", "Helm Charts"}

func (m Model) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "b":
			return m.transitionToComponentSelect()
		case "enter":
			return m.startUpgrades()
		}
	}
	return m, nil
}

func (m Model) viewConfirm() string {
	var b strings.Builder

	title := "UPGRADE PLAN"
	if m.dryRun {
		title += "  " + styleYellow.Render("[DRY RUN — nothing will be modified]")
	}
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n\n")

	byGroup := make(map[string][]resolver.Result, 4)
	for _, r := range m.confirmedResults {
		g := r.Component.Group
		byGroup[g] = append(byGroup[g], r)
	}

	for _, g := range groupOrder {
		items := byGroup[g]
		if len(items) == 0 {
			continue
		}
		b.WriteString(styleGroupName.Render(g))
		b.WriteString("\n")
		for _, r := range items {
			current := styleGrey.Render(r.Component.Current)
			arrow := styleGrey.Render("→")
			latestStr := r.Latest
			if latestStr == "" {
				latestStr = "???"
			}
			latest := styleCyan.Render(latestStr)
			b.WriteString(fmt.Sprintf("  • %-18s %s  %s  %s\n",
				r.Component.Name, current, arrow, latest))
		}
		b.WriteString("\n")
	}

	b.WriteString(styleDim.Render(fmt.Sprintf("Total: %d upgrades", len(m.confirmedResults))))
	b.WriteString("\n\n")
	b.WriteString(styleDim.Render("[ENTER] Run upgrades     [b] Go back     [q] Cancel"))

	return styleBorder.Render(b.String())
}

// ── Screen 5: Upgrade execution ───────────────────────────────────────────────

func (m Model) startUpgrades() (tea.Model, tea.Cmd) {
	m.screen = screenUpgrade
	m.upgradeIdx = 0
	m.upgradeResults = nil
	m.vp = viewport.New(72, 20)

	sorted := make([]resolver.Result, 0, len(m.confirmedResults))
	for _, g := range groupOrder {
		for _, r := range m.confirmedResults {
			if r.Component.Group == g {
				sorted = append(sorted, r)
			}
		}
	}
	m.confirmedResults = sorted
	return m.launchCurrentUpgrade()
}

func (m Model) launchCurrentUpgrade() (tea.Model, tea.Cmd) {
	r := m.confirmedResults[m.upgradeIdx]
	pr, pw := io.Pipe()
	m.upgradeReader = pr
	m.upgradeContent = ""
	m.vp.SetContent("")

	dryRun := m.dryRun
	go func() {
		err := upgrader.RunUpgrade(r.Component, r.Latest, pw, dryRun)
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	return m, m.readOutput()
}

func (m Model) readOutput() tea.Cmd {
	pr := m.upgradeReader
	return func() tea.Msg {
		buf := make([]byte, 4096)
		n, err := pr.Read(buf)
		if n > 0 {
			return upgradeOutputMsg(buf[:n])
		}
		if err == io.EOF {
			return upgradeDoneMsg{}
		}
		if err != nil {
			return upgradeDoneMsg{err: err}
		}
		return upgradeDoneMsg{}
	}
}

func (m Model) updateUpgrade(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

	case upgradeOutputMsg:
		m.upgradeContent += string(msg)
		m.vp.SetContent(m.upgradeContent)
		m.vp.GotoBottom()
		return m, m.readOutput()

	case upgradeDoneMsg:
		r := m.confirmedResults[m.upgradeIdx]
		m.upgradeResults = append(m.upgradeResults, upgradeResult{name: r.Component.Name, err: msg.err})
		m.upgradeIdx++
		if m.upgradeIdx >= len(m.confirmedResults) {
			m.screen = screenSummary
			m.rebootRequired = rebootRequired()
			return m, nil
		}
		return m.launchCurrentUpgrade()
	}
	return m, nil
}

func (m Model) viewUpgrade() string {
	if m.upgradeIdx >= len(m.confirmedResults) {
		return ""
	}
	r := m.confirmedResults[m.upgradeIdx]
	total := len(m.confirmedResults)
	current := m.upgradeIdx + 1

	header := fmt.Sprintf("Upgrading %s (%d of %d)", r.Component.Name, current, total)
	return styleBorder.Render(styleTitle.Render(header) + "\n\n" + m.vp.View())
}

func rebootRequired() bool {
	_, err := os.Stat("/var/run/reboot-required")
	return err == nil
}

// ── Screen 6: Summary ─────────────────────────────────────────────────────────

func (m Model) updateSummary(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "q", "ctrl+c", "n":
			return m, tea.Quit
		case "y":
			if m.rebootRequired {
				_ = exec.Command("reboot").Run()
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m Model) viewSummary() string {
	var b strings.Builder

	b.WriteString(styleTitle.Render("UPGRADE COMPLETE"))
	b.WriteString("\n\n")

	succeeded := 0
	for _, r := range m.upgradeResults {
		if r.err == nil {
			b.WriteString(styleGreen.Render("✓") + "  " + r.name + "\n")
			succeeded++
		} else {
			b.WriteString(styleRed.Render("✗") + "  " +
				fmt.Sprintf("%-18s", r.name) +
				styleRed.Render("FAILED: "+r.err.Error()) + "\n")
		}
	}

	failed := len(m.upgradeResults) - succeeded
	b.WriteString("\n")
	b.WriteString(styleDim.Render(fmt.Sprintf(
		"%d attempted  •  %d succeeded  •  %d failed",
		len(m.upgradeResults), succeeded, failed,
	)))

	if m.rebootRequired {
		b.WriteString("\n\n")
		b.WriteString(styleYellow.Render("⚠  Reboot required to complete OS upgrade"))
		b.WriteString("\n")
		b.WriteString(styleDim.Render("   Reboot now? [y/n]"))
	} else {
		b.WriteString("\n\n")
		b.WriteString(styleDim.Render("Press q to exit."))
	}

	return styleBorder.Render(b.String())
}
