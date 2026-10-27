package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-registry/internal/clidoc"
	"github.com/Bpoe/dsc-registry/internal/client"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func testClient(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := client.New(server.URL, "admin", time.Second, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func waitResult(t *testing.T, b *browser) func() {
	t.Helper()
	select {
	case apply := <-b.results:
		return apply
	case <-time.After(3 * time.Second):
		t.Fatal("request did not complete")
		return nil
	}
}
func TestStatuses(t *testing.T) {
	for _, test := range []struct{ state, outcome, want string }{
		{"stale", "succeeded", "Stale"}, {"stale", "failed", "Stale"}, {"unreported", "failed", "Unreported"},
		{"current", "succeeded", "Succeeded"}, {"current", "failed", "Failed"}, {"current", "canceled", "Canceled"},
		{"current", "unexpected", "Unknown"}, {"removed", "succeeded", "Unknown"}, {"", "", "Unknown"},
	} {
		if got := statusLabel(object{"revisionState": test.state, "outcome": test.outcome}); got != test.want {
			t.Errorf("%+v: %s", test, got)
		}
	}
	status := object{"configurations": []any{object{"name": "removed", "revisionState": "removed"}, object{"name": "active", "desiredRevision": "r", "revisionState": "stale"}}}
	if got := assigned(status); len(got) != 1 || got[0]["name"] != "active" {
		t.Fatalf("assigned: %v", got)
	}
	if len(assigned(nil)) != 0 {
		t.Fatal("nil status")
	}
}
func TestFormattingPreservesData(t *testing.T) {
	docs, err := clidoc.Decode([]byte(`{"name":"cfg","revision":"new","template":{"integer":900719925474099312345,"fraction":1.234567890123456789,"literal":"[red]secret[\"region\"]"},"parameters":{"empty":{},"n":1e999}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := configurationText(object{"name": "host"}, docs[0], object{"revisionState": "unreported"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"900719925474099312345", "1.234567890123456789", "1e999", "[red]secret", "Template (YAML)", "Parameters (YAML)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	docs[0]["parameters"] = nil
	got, err = configurationText(nil, docs[0], nil)
	if err != nil || !strings.Contains(got, "No parameters.") {
		t.Fatalf("%s: %v", got, err)
	}
	docs[0]["template"] = make(chan int)
	if _, err = configurationText(nil, docs[0], nil); err == nil {
		t.Fatal("format error hidden")
	}
}
func TestUntrustedDisplay(t *testing.T) {
	raw := "[red]name[\"link\"]\x1b]52;c;secret\a\u009b31m\u202e\n\t"
	got := safe(raw)
	for _, r := range got {
		if r == 27 || r == 7 || r == 0x9b || r == 0x202e {
			t.Fatalf("unsafe character: %q", got)
		}
	}
	if strings.ContainsAny(cellText(raw), "\n\t") {
		t.Fatal("multiline table cell")
	}
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(120, 4)
	table := table()
	table.SetRect(0, 0, 120, 4)
	table.SetCell(0, 0, tview.NewTableCell(tview.Escape(cellText("[red]name[\"link\"]"))))
	table.Draw(screen)
	var rendered strings.Builder
	for x := 0; x < 30; x++ {
		r, _, _, _ := screen.GetContent(x, 0)
		rendered.WriteRune(r)
	}
	if !strings.Contains(rendered.String(), "[red]name[\"link\"]") {
		t.Fatalf("markup interpreted: %q", rendered.String())
	}

	v := view().SetText(got)
	if v.GetText(false) != got {
		t.Fatal("text changed")
	}
}
func TestRetainedReport(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/machines/m/reports/retained/content" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, `{"durationMs":12,"dscResult":{"n":900719925474099312345,"literal":"[red]hello"}}`)
	})
	machine, cfg := object{"id": "m"}, object{"revision": "new"}
	state := object{"latestReceivedReport": object{"id": "retained", "configurationRevision": "old", "outcome": "succeeded"}}
	got, err := reportText(t.Context(), c, machine, cfg, state)
	if err != nil || !strings.Contains(got, "STALE") || !strings.Contains(got, "900719925474099312345") || !strings.Contains(got, "Duration (ms): 12") {
		t.Fatalf("%s: %v", got, err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if _, err := reportText(t.Context(), c, machine, cfg, nil); err == nil {
		t.Fatal("missing report accepted")
	}
	if calls != 1 {
		t.Fatal("missing report issued request")
	}
}
func TestPaginationFilteringAndEmpty(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/machines" {
			t.Errorf("extra status request: %s", r.URL)
		}
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"machines":[{"name":"Alpha","id":"a"}],"nextCursor":"next"}`)
		} else {
			fmt.Fprint(w, `{"machines":[{"name":"Beta","id":"b"}]}`)
		}
	})
	b := newBrowser(t.Context(), c, "test")
	defer b.cancel()
	b.refresh()
	waitResult(t, b)()
	b.workers.Wait()
	if calls != 2 || len(b.visibleMachines) != 2 {
		t.Fatalf("calls=%d rows=%v", calls, b.visibleMachines)
	}
	b.machineFilter = "ET"
	b.renderTable()
	if len(b.visibleMachines) != 1 || b.visibleMachines[0]["name"] != "Beta" {
		t.Fatal(b.visibleMachines)
	}
	b.machineFilter = "absent"
	b.renderTable()
	if len(b.visibleMachines) != 0 || !strings.Contains(b.footer.GetText(false), "No machines") {
		t.Fatal("missing empty state")
	}
}
func TestBackPreservesWidgetsAndSelection(t *testing.T) {
	b := newBrowser(t.Context(), nil, "test")
	defer b.cancel()
	for i := 0; i < 100; i++ {
		b.machineRows = append(b.machineRows, object{"id": fmt.Sprint(i), "name": fmt.Sprint(i)})
	}
	b.renderTable()
	b.machines.Select(51, 0).SetOffset(45, 0)
	b.show(machinePage)
	b.configRows = []object{{"name": "a", "desiredRevision": "r"}, {"name": "b", "desiredRevision": "r"}}
	b.renderTable()
	b.configurations.Select(2, 0).SetOffset(1, 0)
	b.show(configurationPage)
	b.show(reportPage)
	b.keys(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
	if b.page != configurationPage {
		t.Fatal(b.page)
	}
	b.keys(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
	row, _ := b.configurations.GetSelection()
	offset, _ := b.configurations.GetOffset()
	if row != 2 || offset != 1 {
		t.Fatalf("configuration selection=%d offset=%d", row, offset)
	}
	b.keys(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
	row, _ = b.machines.GetSelection()
	offset, _ = b.machines.GetOffset()
	if row != 51 || offset != 45 {
		t.Fatalf("machine selection=%d offset=%d", row, offset)
	}
	b.renderTable()
	row, _ = b.machines.GetSelection()
	if row != 51 {
		t.Fatal("refresh lost selection")
	}
}
func TestFailureAndOutdatedResults(t *testing.T) {
	for _, code := range []int{401, 403, 404, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, `{"error":{"code":"unavailable"}}`)
			})
			b := newBrowser(t.Context(), c, "test")
			defer b.cancel()
			b.refresh()
			apply := waitResult(t, b)
			apply()
			b.workers.Wait()
			if !b.dialog || b.loading {
				t.Fatal("failure not shown")
			}
			if _, err := reportText(t.Context(), c, object{"id": "m"}, nil, object{"latestReceivedReport": object{"id": "r"}}); err == nil {
				t.Fatal("retrieval failure hidden")
			}
		})
	}
	b := newBrowser(t.Context(), nil, "test")
	defer b.cancel()
	applied := false
	b.load(func(context.Context) (func(), error) { return func() { applied = true }, nil })
	apply := waitResult(t, b)
	b.show(machinePage)
	apply()
	b.workers.Wait()
	if applied {
		t.Fatal("outdated result applied")
	}
}
func TestConfigurationRefreshAndReportNavigation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			fmt.Fprint(w, `{"configurations":[{"name":"cfg","desiredRevision":"r","revisionState":"unreported"}]}`)
		} else {
			fmt.Fprint(w, `{"name":"cfg","revision":"r","template":{"resources":[]},"parameters":{}}`)
		}
	})
	b := newBrowser(t.Context(), c, "test")
	defer b.cancel()
	b.machine = object{"id": "m", "name": "host"}
	b.state = object{"name": "cfg"}
	b.show(configurationPage)
	b.refresh()
	waitResult(t, b)()
	b.workers.Wait()
	if b.cfg["revision"] != "r" || !strings.Contains(b.configuration.GetText(false), "resources: []") {
		t.Fatal("configuration not rendered")
	}
	b.keys(tcell.NewEventKey(tcell.KeyRune, 'r', 0))
	if b.page != configurationPage || !b.dialog {
		t.Fatal("missing report should remain on configuration")
	}
}
func TestCancelInFlightSimulation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(canceled) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	screen := tcell.NewSimulationScreen("")
	done := make(chan error, 1)
	go func() { done <- runScreen(ctx, c, "test", screen) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("load did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request not canceled")
	}
}
func TestCompleteNavigation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines":
			fmt.Fprint(w, `{"machines":[{"name":"host","id":"m"}]}`)
		case "/v1/machines/m/status":
			fmt.Fprint(w, `{"outcome":"unknown","configurations":[{"name":"cfg","desiredRevision":"new","revisionState":"stale","latestReceivedReport":{"id":"old-report","configurationRevision":"old","outcome":"succeeded"}},{"name":"removed","revisionState":"removed"}]}`)
		case "/v1/machines/m/configurations/cfg":
			fmt.Fprint(w, `{"name":"cfg","revision":"new","template":{"resources":[]}}`)
		case "/v1/machines/m/reports/old-report/content":
			fmt.Fprint(w, `{"durationMs":2,"dscResult":{}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	})
	b := newBrowser(t.Context(), c, "test")
	defer b.cancel()
	finish := func() { t.Helper(); waitResult(t, b)(); b.workers.Wait() }
	enter := tcell.NewEventKey(tcell.KeyEnter, 0, 0)
	b.refresh()
	finish()
	b.machines.InputHandler()(enter, func(tview.Primitive) {})
	finish()
	if b.page != machinePage || len(b.visibleConfigs) != 1 || b.configurations.GetCell(1, 1).Text != "Stale" {
		t.Fatal("machine detail did not show assigned stale configuration")
	}
	b.configurations.InputHandler()(enter, func(tview.Primitive) {})
	finish()
	if b.page != configurationPage || b.cfg["revision"] != "new" {
		t.Fatal("configuration not opened")
	}
	b.keys(tcell.NewEventKey(tcell.KeyRune, 'r', 0))
	finish()
	if b.page != reportPage || !strings.Contains(b.report.GetText(false), "STALE") {
		t.Fatal("stale report not opened")
	}
	for _, want := range []int{configurationPage, machinePage, machinesPage} {
		b.keys(tcell.NewEventKey(tcell.KeyEscape, 0, 0))
		if b.page != want {
			t.Fatal(b.page)
		}
	}
}

func TestSimulationFailureDoesNotBlockShutdown(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthorized"}}`)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runScreen(ctx, c, "test", tcell.NewSimulationScreen("")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("error dialog blocked shutdown")
	}
}
