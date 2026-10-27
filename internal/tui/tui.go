package tui

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/Bpoe/dsc-registry/internal/client"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	machinesPage = iota
	machinePage
	configurationPage
	reportPage
)

var pageNames = []string{"Machines", "Machine", "Configuration", "Latest report"}

type browser struct {
	app                                                      *tview.Application
	pages                                                    *tview.Pages
	footer                                                   *tview.TextView
	summary                                                  *tview.TextView
	machines, configurations                                 *tview.Table
	configuration, report                                    *tview.TextView
	page                                                     int
	dialog                                                   bool
	loading                                                  bool
	generation                                               uint64
	ctx                                                      context.Context
	cancel                                                   context.CancelFunc
	requestCancel                                            context.CancelFunc
	workers                                                  sync.WaitGroup
	results                                                  chan func()
	wake                                                     func()
	client                                                   *client.Client
	machineRows, configRows, visibleMachines, visibleConfigs []object
	machine, cfg, state                                      object
	machineFilter, configFilter                              string
}

// Run owns request cancellation and terminal restoration. Workers publish to a
// bounded channel and wake tview with an F24 event; its input callback consumes the
// results on the event loop. Unlike blocking QueueUpdateDraw calls this also
// permits cancellation/join when Run fails or the terminal disappears.
// PostEventWait also unblocks on screen shutdown, and cannot lose a wakeup
// when the terminal event queue is full.
func Run(ctx context.Context, c *client.Client, label string) error {
	return runScreen(ctx, c, label, nil)
}
func runScreen(ctx context.Context, c *client.Client, label string, screen tcell.Screen) error {
	b := newBrowser(ctx, c, label)
	if screen != nil {
		b.app.SetScreen(screen)
	} // Simulation screens in tests.
	done := make(chan struct{})
	var watcher sync.WaitGroup
	started := false
	b.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		if !started {
			started = true
			b.wake = func() { screen.PostEventWait(tcell.NewEventKey(tcell.KeyF24, 0, tcell.ModNone)) }
			watcher.Go(func() {
				select {
				case <-b.ctx.Done():
					screen.PostEventWait(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone))
				case <-done:
				}
			})
			b.refresh()
		}
		return false
	})
	err := b.app.Run()
	close(done)
	b.cancel()
	b.workers.Wait()
	watcher.Wait()
	if err != nil {
		return fmt.Errorf("cannot run terminal interface (use an interactive terminal with a supported TERM): %w", err)
	}
	return nil
}

func view() *tview.TextView {
	return tview.NewTextView().SetDynamicColors(false).SetRegions(false).SetWrap(true).SetScrollable(true)
}
func table() *tview.Table { return tview.NewTable().SetSelectable(true, false).SetFixed(1, 0) }
func newBrowser(ctx context.Context, c *client.Client, label string) *browser {
	ctx, cancel := context.WithCancel(ctx)
	b := &browser{app: tview.NewApplication(), pages: tview.NewPages(), footer: view(), summary: view(), machines: table(), configurations: table(), configuration: view(), report: view(), ctx: ctx, cancel: cancel, client: c, results: make(chan func(), 16), wake: func() {}}
	machine := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(b.summary, 7, 0, false).AddItem(b.configurations, 0, 1, true)
	b.pages.AddPage(pageNames[0], b.machines, true, true).AddPage(pageNames[1], machine, true, false).AddPage(pageNames[2], b.configuration, true, false).AddPage(pageNames[3], b.report, true, false)
	title := view().SetText(safe("DSC Registry | Context: " + label + " | read-only"))
	root := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(title, 1, 0, false).AddItem(b.pages, 0, 1, true).AddItem(b.footer, 2, 0, false)
	b.app.SetRoot(root, true)
	b.machines.SetSelectedFunc(func(row, col int) {
		if b.loading || row < 1 || row > len(b.visibleMachines) {
			return
		}
		b.machine = b.visibleMachines[row-1]
		b.configFilter = ""
		b.configRows = nil
		b.configurations.Clear().Select(1, 0).SetOffset(0, 0)
		b.show(machinePage)
		b.refresh()
	})
	b.configurations.SetSelectedFunc(func(row, col int) {
		if b.loading || row < 1 || row > len(b.visibleConfigs) {
			return
		}
		b.state = b.visibleConfigs[row-1]
		b.cfg = nil
		b.configuration.SetText("Loading configuration…").ScrollToBeginning()
		b.show(configurationPage)
		b.refresh()
	})
	b.app.SetInputCapture(b.keys)
	b.show(machinesPage)
	return b
}
func (b *browser) focus() tview.Primitive {
	switch b.page {
	case machinePage:
		return b.configurations
	case configurationPage:
		return b.configuration
	case reportPage:
		return b.report
	default:
		return b.machines
	}
}
func (b *browser) hint(message string) {
	shortcuts := "↑/↓ scroll | Esc back | Ctrl+R refresh | ? help | Ctrl+C quit"
	switch b.page {
	case machinesPage:
		shortcuts = "↑/↓ select | Enter machine | / filter | r/Ctrl+R refresh | ? help | q quit"
	case machinePage:
		shortcuts = "↑/↓ select | Enter configuration | / filter | r/Ctrl+R refresh | Esc back | ? help"
	case configurationPage:
		shortcuts += " | r latest report"
	}
	b.footer.SetText(safe(message + "\n" + shortcuts))
}
func (b *browser) invalidate() {
	b.generation++
	if b.requestCancel != nil {
		b.requestCancel()
	}
	b.loading = false
}
func (b *browser) show(page int) {
	b.invalidate()
	b.page = page
	b.pages.SwitchToPage(pageNames[page])
	b.app.SetFocus(b.focus())
	b.hint(pageNames[page])
}
func (b *browser) keys(e *tcell.EventKey) *tcell.EventKey {
	if e.Key() == tcell.KeyF24 {
		for {
			select {
			case apply := <-b.results:
				apply()
			default:
				return nil
			}
		}
	}

	if e.Key() == tcell.KeyCtrlC {
		b.cancel()
		b.app.Stop()
		return nil
	}
	if b.dialog {
		return e
	}
	if e.Key() == tcell.KeyEscape {
		if b.page > machinesPage {
			b.show(b.page - 1)
		}
		return nil
	}
	if e.Key() == tcell.KeyCtrlR {
		b.refresh()
		return nil
	}
	switch e.Rune() {
	case 'q':
		if b.page == machinesPage {
			b.cancel()
			b.app.Stop()
			return nil
		}
	case '?':
		b.message("Machines → Machine → Configuration → Latest report\n\n" + b.footer.GetText(false) + "\n\nRead-only. Report receipt time is not execution time. Execution success is not DSC compliance or machine online health.")
		return nil
	case '/':
		if b.page <= machinePage {
			b.filter()
			return nil
		}
	case 'r':
		if b.page <= machinePage {
			b.refresh()
			return nil
		}
		if b.page == configurationPage && !b.loading {
			if b.cfg == nil {
				b.message("Load a configuration before opening its report.")
			} else if child(b.state, "latestReceivedReport") == nil {
				b.message("No retained report is available for this configuration.")
			} else {
				b.report.SetText("Loading report…").ScrollToBeginning()
				b.show(reportPage)
				b.refresh()
			}
			return nil
		}
	}
	return e
}
func (b *browser) dismiss() {
	b.pages.RemovePage("dialog")
	b.dialog = false
	b.app.SetFocus(b.focus())
}
func (b *browser) message(s string) {
	b.dialog = true
	modal := tview.NewModal().SetText(tview.Escape(safe(s))).AddButtons([]string{"OK"}).SetDoneFunc(func(int, string) { b.dismiss() })
	modal.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape {
			b.dismiss()
			return nil
		}
		return e
	})
	b.pages.AddPage("dialog", modal, true, true)
	b.app.SetFocus(modal)
}
func (b *browser) filter() {
	b.dialog = true
	page := b.page
	value := b.machineFilter
	if page == machinePage {
		value = b.configFilter
	}
	input := tview.NewInputField().SetLabel("Name contains: ").SetText(value)
	input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			if page == machinesPage {
				b.machineFilter = input.GetText()
			} else {
				b.configFilter = input.GetText()
			}
			b.renderTable()
		}
		b.dismiss()
	})
	panel := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(nil, 0, 1, false).AddItem(input, 3, 0, true).AddItem(nil, 0, 1, false)
	b.pages.AddPage("dialog", panel, true, true)
	b.app.SetFocus(input)
}

// One active request chain per screen; generation prevents canceled/outdated
// responses from overwriting a new selection, including after navigating back.
func (b *browser) load(work func(context.Context) (func(), error)) {
	b.invalidate()
	generation := b.generation
	ctx, cancel := context.WithCancel(b.ctx)
	b.requestCancel = cancel
	b.loading = true
	b.hint("Loading…")
	b.workers.Go(func() {
		defer cancel()
		apply, err := work(ctx)
		if ctx.Err() != nil {
			return
		}
		update := func() {
			if generation != b.generation {
				return
			}
			b.loading = false
			if err != nil {
				b.hint("Refresh failed; previously loaded data may be outdated.")
				b.message(errorMessage(err))
				return
			}
			apply()
		}
		select {
		case b.results <- update:
			b.wake()
		case <-ctx.Done():
		}
	})
}
func errorMessage(err error) string {
	if remote, ok := err.(*client.Error); ok {
		switch remote.Status {
		case 401, 403:
			return "Registry denied access. Check administrator credentials and permissions."
		case 404:
			return "The machine, configuration, or retained report no longer exists. Go back and refresh."
		}
	}
	return err.Error()
}
func (b *browser) refresh() {
	switch b.page {
	case machinesPage:
		b.load(func(ctx context.Context) (func(), error) {
			data, err := b.client.List(ctx, "/v1/machines", "machines", 100, "", true)
			return func() { b.machineRows = items(data, "machines"); b.renderTable() }, err
		})
	case machinePage:
		machine := b.machine
		b.load(func(ctx context.Context) (func(), error) {
			data, err := get(ctx, b.client, machinePath(machine)+"/status")
			return func() {
				b.summary.SetText(safe(fmt.Sprintf("Name: %s\nID: %s\nCreated: %s\nOverall execution outcome: %s\nLast report received: %s\nExecution outcome does not imply compliance or online health.", text(machine, "name"), text(machine, "id"), text(machine, "createdAt"), text(data, "outcome"), text(data, "lastReportReceivedAt"))))
				b.configRows = assigned(data)
				b.renderTable()
			}, err
		})
	case configurationPage:
		machine, state := b.machine, b.state
		b.load(func(ctx context.Context) (func(), error) {
			cfg, err := get(ctx, b.client, machinePath(machine)+"/configurations/"+url.PathEscape(text(state, "name")))
			if err != nil {
				return nil, err
			}
			// Refresh aggregate status too, so a changed desired revision or retained
			// report does not inherit stale metadata from the parent screen.
			status, err := get(ctx, b.client, machinePath(machine)+"/status")
			if err != nil {
				return nil, err
			}
			var current object
			for _, s := range assigned(status) {
				if s["name"] == cfg["name"] {
					current = s
					break
				}
			}
			if current == nil || current["desiredRevision"] != cfg["revision"] {
				return nil, fmt.Errorf("configuration changed while loading; refresh to retry")
			}
			content, err := configurationText(machine, cfg, current)
			return func() { b.cfg = cfg; b.state = current; b.configuration.SetText(content); b.hint("Configuration") }, err
		})
	case reportPage:
		machine, cfg := b.machine, b.cfg
		b.load(func(ctx context.Context) (func(), error) {
			status, err := get(ctx, b.client, machinePath(machine)+"/status")
			if err != nil {
				return nil, err
			}
			var state object
			for _, s := range assigned(status) {
				if s["name"] == cfg["name"] {
					state = s
					break
				}
			}
			if state == nil {
				return nil, fmt.Errorf("configuration is no longer assigned; go back and refresh")
			}
			content, err := reportText(ctx, b.client, machine, object{"revision": state["desiredRevision"]}, state)
			return func() { b.report.SetText(content); b.hint("Latest retained report") }, err
		})
	}
}
func (b *browser) renderTable() {
	t := b.machines
	rows := filtered(b.machineRows, b.machineFilter)
	headers := []string{"Name", "ID", "Created"}
	key := "id"
	if b.page == machinePage {
		t = b.configurations
		rows = filtered(b.configRows, b.configFilter)
		headers = []string{"Configuration", "Status", "Last report received"}
		key = "name"
	}
	selected, _ := t.GetSelection()
	oldRows := b.visibleMachines
	if b.page == machinePage {
		oldRows = b.visibleConfigs
	}
	selectedID := ""
	if selected > 0 && selected <= len(oldRows) {
		selectedID = text(oldRows[selected-1], key)
	}
	offset, colOffset := t.GetOffset()
	t.Clear()
	for col, h := range headers {
		t.SetCell(0, col, tview.NewTableCell(h).SetSelectable(false).SetTextColor(tcell.ColorYellow))
	}
	for i, row := range rows {
		values := []string{text(row, "name"), text(row, "id"), text(row, "createdAt")}
		if b.page == machinePage {
			values = []string{text(row, "name"), statusLabel(row), text(child(row, "latestReceivedReport"), "receivedAt")}
		}
		for col, value := range values {
			cell := tview.NewTableCell(tview.Escape(cellText(value))).SetExpansion(1)
			if b.page == machinePage && col == 1 {
				switch value {
				case "Succeeded":
					cell.SetTextColor(tcell.ColorGreen)
				case "Failed":
					cell.SetTextColor(tcell.ColorRed)
				default:
					cell.SetTextColor(tcell.ColorYellow)
				}
			}
			t.SetCell(i+1, col, cell)
		}
		if text(row, key) == selectedID {
			selected = i + 1
		}
	}
	if selected < 1 {
		selected = 1
	}
	if selected > len(rows) {
		selected = len(rows)
	}
	t.Select(selected, 0).SetOffset(offset, colOffset)
	if b.page == machinePage {
		b.visibleConfigs = rows
	} else {
		b.visibleMachines = rows
	}
	message := fmt.Sprintf("%s: %d shown", pageNames[b.page], len(rows))
	if len(rows) == 0 {
		message = "No machines match the filter or none are registered."
		if b.page == machinePage {
			message = "No assigned configurations match the filter or none are assigned."
		}
	}
	b.hint(message)
}
