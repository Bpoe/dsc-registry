package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const deleteMachineID = "a8dfeac8-1d90-4cea-bdd7-75eca755b312"

func deletionBrowser(t *testing.T, page int, handler http.HandlerFunc) *browser {
	t.Helper()
	b := newBrowser(t.Context(), testClient(t, handler), "test")
	t.Cleanup(func() { b.cancel(); b.workers.Wait() })
	b.machine = object{"id": deleteMachineID, "name": "display [red]host"}
	b.machineRows = []object{b.machine}
	b.renderTable()
	if page != machinesPage {
		b.show(machinePage)
		b.configRows = []object{{"name": "cfg /?#%", "desiredRevision": "r"}}
		b.renderTable()
		b.cfg = object{"name": "cfg /?#%", "revision": "r"}
		b.state = b.configRows[0]
		b.configuration.SetText("desired content")
		b.report.SetText("report content")
		b.show(page)
	}
	return b
}

// Dispatch through the application capture and actual focused standard widget,
// including tview's button focus and default Enter behavior.
func press(b *browser, key tcell.Key, r rune) {
	e := b.keys(tcell.NewEventKey(key, r, 0))
	if e != nil {
		b.app.GetFocus().InputHandler()(e, func(p tview.Primitive) { b.app.SetFocus(p) })
	}
}
func confirmDeletion(t *testing.T, b *browser) {
	t.Helper()
	press(b, tcell.KeyRune, 'd')
	if !b.dialog {
		t.Fatal("missing confirmation")
	}
	button, ok := b.app.GetFocus().(*tview.Button)
	if !ok || button.GetLabel() != "Cancel" {
		t.Fatalf("default focus: %T", b.app.GetFocus())
	}
	press(b, tcell.KeyTab, 0)
	press(b, tcell.KeyEnter, 0)
	if b.dialog || !b.loading {
		t.Fatal("confirmation did not start asynchronous deletion")
	}
}

func dialogText(t *testing.T, b *browser) string {
	t.Helper()
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(160, 50)
	_, modal := b.pages.GetFrontPage()
	modal.SetRect(0, 0, 160, 50)
	modal.Draw(screen)
	var result strings.Builder
	for y := 0; y < 50; y++ {
		for x := 0; x < 160; x++ {
			r, _, _, _ := screen.GetContent(x, y)
			if r < 0x2500 || r > 0x257f {
				result.WriteRune(r)
			}
		}
		result.WriteByte(' ')
	}
	return strings.Join(strings.Fields(result.String()), " ")
}

func TestDeleteConfirmationCancellation(t *testing.T) {
	for _, page := range []int{machinesPage, machinePage, configurationPage} {
		for _, key := range []tcell.Key{tcell.KeyEnter, tcell.KeyEscape} {
			t.Run(fmt.Sprintf("%d/%d", page, key), func(t *testing.T) {
				var calls atomic.Int32
				b := deletionBrowser(t, page, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
				before := b.footer.GetText(false)
				press(b, tcell.KeyRune, 'd')
				_, primitive := b.pages.GetFrontPage()
				warning := dialogText(t, b)
				for _, want := range []string{"display [red]host", "reports", "permanently removed", "not undone"} {
					if !strings.Contains(warning, want) {
						t.Fatalf("missing %q in warning: %s", want, warning)
					}
				}
				if page == machinesPage && !strings.Contains(warning, "API keys") {
					t.Fatal("missing key warning")
				}
				if page != machinesPage && !strings.Contains(warning, "cfg /?#%") {
					t.Fatal("missing configuration name")
				}
				modal, ok := primitive.(*tview.Modal)
				if !ok {
					t.Fatal("confirmation is not a standard modal")
				}
				// Modal capture handles Escape when tview dispatches via the Pages root.
				if key == tcell.KeyEscape {
					modal.InputHandler()(tcell.NewEventKey(key, 0, 0), func(p tview.Primitive) { b.app.SetFocus(p) })
				} else {
					press(b, key, 0)
				}
				if b.dialog || b.loading || b.page != page || calls.Load() != 0 || b.footer.GetText(false) != before {
					t.Fatal("cancel changed the UI or issued a request")
				}
			})
		}
	}
}

func TestDeleteIdentityAndRefresh(t *testing.T) {
	for _, page := range []int{machinesPage, machinePage, configurationPage} {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			var calls atomic.Int32
			b := deletionBrowser(t, page, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Header.Get("Authorization") != "ApiKey admin" {
					t.Error("missing authenticated client")
				}
				if n == 1 {
					want := "/v1/machines/" + deleteMachineID
					if page != machinesPage {
						want += "/configurations/" + url.PathEscape("cfg /?#%")
					}
					if r.Method != http.MethodDelete || r.URL.EscapedPath() != want || r.URL.RawQuery != "" {
						t.Errorf("request: %s %s; want DELETE %s", r.Method, r.URL, want)
					}
					w.WriteHeader(http.StatusNoContent)
				} else {
					want := "/v1/machines"
					if page != machinesPage {
						want = "/v1/machines/" + deleteMachineID + "/status"
					}
					if r.Method != http.MethodGet || r.URL.Path != want {
						t.Errorf("refresh: %s %s", r.Method, r.URL)
					}
					if page == machinesPage {
						fmt.Fprint(w, `{"machines":[{"id":"other","name":"display other"}]}`)
					} else {
						fmt.Fprint(w, `{"outcome":"unknown","configurations":[{"name":"other","desiredRevision":"r"}]}`)
					}
				}
			})
			b.machineFilter = "display"
			b.configFilter = "cfg"
			// Hidden rows and previous selections must never determine identity.
			if page == machinesPage {
				b.machineRows = append([]object{{"id": "hidden", "name": "hidden"}}, b.machineRows...)
				b.machine = object{"id": "previous", "name": "previous"}
				b.renderTable()
			} else if page == machinePage {
				b.configRows = append([]object{{"name": "hidden", "desiredRevision": "r"}}, b.configRows...)
				b.cfg = object{"name": "previous"}
				b.renderTable()
			} else {
				b.state = object{"name": "previous"}
			}
			confirmDeletion(t, b)
			waitResult(t, b)()
			waitResult(t, b)()
			b.workers.Wait()
			wantPage := machinesPage
			if page != machinesPage {
				wantPage = machinePage
			}
			if b.page != wantPage || b.loading || b.cfg != nil || b.state != nil || calls.Load() != 2 {
				t.Fatalf("page=%d loading=%v calls=%d", b.page, b.loading, calls.Load())
			}
			if b.configuration.GetText(false) != "" || b.report.GetText(false) != "" {
				t.Fatal("deleted content retained")
			}
			if b.machineFilter != "display" || b.configFilter != "cfg" {
				t.Fatal("filters changed")
			}
			if page == machinesPage {
				if b.machine != nil || len(b.visibleMachines) != 1 || text(b.visibleMachines[0], "id") != "other" {
					t.Fatal("machine references not refreshed")
				}
			} else if len(b.visibleConfigs) != 0 || !strings.Contains(b.summary.GetText(false), "unknown") {
				t.Fatal("filtered configuration status not refreshed")
			}
		})
	}
}

func TestDeleteUnavailable(t *testing.T) {
	for _, page := range []int{machinesPage, machinePage, configurationPage, reportPage} {
		for _, mode := range []string{"loading", "empty", "filtered", "report"} {
			t.Run(fmt.Sprintf("%d/%s", page, mode), func(t *testing.T) {
				b := deletionBrowser(t, page, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
				switch mode {
				case "loading":
					b.loading = true
				case "empty":
					b.machineRows = nil
					b.configRows = nil
					b.cfg = nil
				case "filtered":
					b.machineFilter = "absent"
					b.configFilter = "absent"
					b.cfg = nil
				case "report":
					b.show(reportPage)
				}
				if page <= machinePage && mode != "report" {
					b.renderTable()
				}
				press(b, tcell.KeyRune, 'd')
				if b.dialog {
					t.Fatal("deletion exposed without a loaded target")
				}
			})
		}
	}
}

func TestDeleteFailuresPreserveView(t *testing.T) {
	for _, page := range []int{machinesPage, machinePage, configurationPage} {
		for _, code := range []int{401, 403, 404, 503, 0} {
			t.Run(fmt.Sprintf("%d/%d", page, code), func(t *testing.T) {
				b := deletionBrowser(t, page, func(w http.ResponseWriter, r *http.Request) {
					if code == 0 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					w.WriteHeader(code)
					fmt.Fprint(w, `{"error":{"message":"sensitive payload"}}`)
				})
				confirmDeletion(t, b)
				waitResult(t, b)()
				b.workers.Wait()
				if !b.dialog || b.loading || b.page != page || b.machine == nil || len(b.machineRows) != 1 {
					t.Fatal("failure changed view")
				}
				if page != machinesPage && (b.cfg == nil || b.configuration.GetText(false) != "desired content") {
					t.Fatal("failure lost configuration")
				}

				message := dialogText(t, b)
				if strings.Contains(message, "sensitive payload") {
					t.Fatal("server body leaked")
				}
				if (code == 401 || code == 403) && !strings.Contains(message, "denied access") {
					t.Fatal(message)
				}
				if code == 404 && !strings.Contains(message, "no longer exists") {
					t.Fatal(message)
				}
				if code == 0 && !strings.Contains(message, "connection failed") {
					t.Fatal(message)
				}
				if !strings.Contains(b.footer.GetText(false), "Deletion failed") {
					t.Fatal("incorrect failure status")
				}
			})
		}
	}
}

func TestDeleteDuplicateCancellationAndStaleResult(t *testing.T) {
	for _, action := range []string{"complete", "shutdown", "stale"} {
		t.Run(action, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			canceled := make(chan struct{})
			var calls atomic.Int32
			b := deletionBrowser(t, configurationPage, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					fmt.Fprint(w, `{"configurations":[]}`)
					return
				}
				calls.Add(1)
				close(started)
				select {
				case <-release:
					w.WriteHeader(http.StatusNoContent)
				case <-r.Context().Done():
					close(canceled)
				}
			})
			confirmDeletion(t, b)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("DELETE blocked event loop")
			}
			press(b, tcell.KeyRune, 'd')
			press(b, tcell.KeyCtrlR, 0)
			press(b, tcell.KeyRune, 'r')
			press(b, tcell.KeyRune, '?')
			press(b, tcell.KeyRune, '/')
			if b.dialog || !b.loading {
				t.Fatal("duplicate input interrupted DELETE")
			}
			switch action {
			case "shutdown":
				press(b, tcell.KeyCtrlC, 0)
			case "complete", "stale":
				close(release)
			}
			if action == "complete" || action == "stale" {
				apply := waitResult(t, b)
				if action == "stale" {
					b.show(machinesPage)
				}
				apply()
				if action == "complete" {
					waitResult(t, b)()
				} else if b.page != machinesPage || b.cfg == nil {
					t.Fatal("stale delete result applied")
				}
			} else {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("DELETE not canceled")
				}
			}
			b.workers.Wait()
			if calls.Load() != 1 || b.deletePending.Load() {
				t.Fatal("duplicate request or stuck guard")
			}
		})
	}
}

func TestDeleteBlocksInputUntilResponseProcessed(t *testing.T) {
	for _, page := range []int{machinesPage, machinePage, configurationPage} {
		for _, code := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("%d/%d", page, code), func(t *testing.T) {
				started := make(chan struct{})
				release := make(chan struct{})
				var calls atomic.Int32
				b := deletionBrowser(t, page, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodDelete {
						if page == machinesPage {
							fmt.Fprint(w, `{"machines":[]}`)
						} else {
							fmt.Fprint(w, `{"configurations":[]}`)
						}
						return
					}
					calls.Add(1)
					close(started)
					select {
					case <-release:
						w.WriteHeader(code)
					case <-r.Context().Done():
					}
				})
				confirmDeletion(t, b)
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("DELETE did not start")
				}
				generation := b.generation
				assertBlocked := func() {
					t.Helper()
					for _, event := range []*tcell.EventKey{
						tcell.NewEventKey(tcell.KeyEscape, 0, 0),
						tcell.NewEventKey(tcell.KeyCtrlR, 0, 0),
						tcell.NewEventKey(tcell.KeyEnter, 0, 0),
						tcell.NewEventKey(tcell.KeyDown, 0, 0),
						tcell.NewEventKey(tcell.KeyTab, 0, 0),
						tcell.NewEventKey(tcell.KeyRune, 'q', 0),
						tcell.NewEventKey(tcell.KeyRune, 'r', 0),
						tcell.NewEventKey(tcell.KeyRune, '/', 0),
						tcell.NewEventKey(tcell.KeyRune, '?', 0),
						tcell.NewEventKey(tcell.KeyRune, 'd', 0),
					} {
						if b.keys(event) != nil {
							t.Fatalf("input passed to widget during deletion: %v", event)
						}
					}
					if b.page != page || b.generation != generation || !b.loading || !b.deleting || b.dialog || b.ctx.Err() != nil || calls.Load() != 1 {
						t.Fatal("input navigated, canceled, or interrupted DELETE")
					}
				}
				assertBlocked()
				close(release)
				apply := waitResult(t, b)
				b.workers.Wait()
				if b.deletePending.Load() {
					t.Fatal("HTTP worker did not finish")
				}
				// The HTTP response is queued; input must remain blocked until applied.
				assertBlocked()
				apply()
				if b.deleting {
					t.Fatal("input guard retained after response processed")
				}
				if code == http.StatusNoContent {
					waitResult(t, b)()
					b.workers.Wait()
					wantPage := machinesPage
					if page != machinesPage {
						wantPage = machinePage
					}
					if b.page != wantPage || b.loading || b.cfg != nil || b.state != nil {
						t.Fatal("success did not clean up and refresh")
					}
				} else {
					if b.page != page || !b.dialog || b.loading || b.machine == nil || (page != machinesPage && b.cfg == nil) {
						t.Fatal("failure did not preserve view and show error")
					}
					press(b, tcell.KeyEnter, 0) // Dismiss the existing error modal.
				}
				if b.page == machinesPage {
					press(b, tcell.KeyRune, '/')
					if !b.dialog {
						t.Fatal("filter keyboard behavior not restored")
					}
				} else {
					previous := b.page
					press(b, tcell.KeyEscape, 0)
					if b.page != previous-1 {
						t.Fatal("Escape navigation not restored")
					}
				}
				if calls.Load() != 1 {
					t.Fatal("duplicate DELETE")
				}
			})
		}
	}
}

func TestDeleteShutdownDropsQueuedResult(t *testing.T) {
	b := deletionBrowser(t, configurationPage, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	confirmDeletion(t, b)
	apply := waitResult(t, b)
	b.cancel()
	apply()
	if b.page != configurationPage || b.cfg == nil {
		t.Fatal("shutdown applied queued result")
	}
}

// Keep cancellation coverage explicit for a parent context, as used by Run.
func TestDeleteParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
	b := newBrowser(ctx, c, "test")
	b.machineRows = []object{{"id": deleteMachineID, "name": "host"}}
	b.renderTable()
	confirmDeletion(t, b)
	<-started
	cancel()
	done := make(chan struct{})
	go func() { b.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join DELETE worker")
	}
	if len(b.results) != 0 {
		t.Fatal("canceled result published")
	}
}

func TestDeletedRowRemovedWhenRefreshFails(t *testing.T) {
	for _, page := range []int{machinesPage, machinePage, configurationPage} {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			b := deletionBrowser(t, page, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			})
			if page == machinesPage {
				b.machineRows = append(b.machineRows, object{"id": "last", "name": "last"})
				b.renderTable()
			} else {
				b.configRows = append(b.configRows, object{"name": "last", "desiredRevision": "r"})
				if page == machinePage {
					b.renderTable()
				}
			}
			confirmDeletion(t, b)
			waitResult(t, b)()
			waitResult(t, b)()
			b.workers.Wait()
			if !b.dialog {
				t.Fatal("refresh failure hidden")
			}
			if b.page == machinesPage {
				row, _ := b.machines.GetSelection()
				if len(b.visibleMachines) != 1 || text(b.visibleMachines[0], "id") != "last" || row != 1 {
					t.Fatal("invalid machine selection")
				}
			} else {
				row, _ := b.configurations.GetSelection()
				if len(b.visibleConfigs) != 1 || text(b.visibleConfigs[0], "name") != "last" || row != 1 {
					t.Fatal("invalid configuration selection")
				}
			}
		})
	}
}
