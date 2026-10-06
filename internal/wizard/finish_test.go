package wizard

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// The finish screen's status words must be the library's unattended wording:
// feed the same results to installer.PrintResults and to ClientLines and
// compare line by line.
func TestClientLinesMatchTheLibraryResultWording(t *testing.T) {
	results := []harness.Result{
		{HarnessID: "a", Name: "Applied Client", State: harness.Applied},
		{HarnessID: "b", Name: "Noop Client", State: harness.ApplyNoop},
		{HarnessID: "c", Name: "Conflict Client", State: harness.ApplyConflict},
		{HarnessID: "d", Name: "Skipped Client", State: harness.ApplySkipped, Reason: "no project support"},
		{HarnessID: "e", Name: "Failed Client", State: harness.ApplyFailed, Reason: "permission denied"},
	}
	var lib bytes.Buffer
	installer.PrintResults(&lib, results, true, false)

	var ours strings.Builder
	for _, l := range ClientLines(false, results, nil) {
		fmt.Fprintf(&ours, "  %-22s %s\n", l.Name, l.Status)
	}
	if ours.String() != lib.String() {
		t.Errorf("ClientLines differs from installer.PrintResults:\nours:\n%s\nlibrary:\n%s", ours.String(), lib.String())
	}
}

// The dry-run words must equal installer.PrintChanges (without paths, which the
// finish screen never shows).
func TestClientLinesMatchTheLibraryChangeWording(t *testing.T) {
	changes := []harness.Change{
		{HarnessID: "a", Name: "Add Client", State: harness.Applied, Action: "add"},
		{HarnessID: "b", Name: "Noop Client", State: harness.ApplyNoop, Action: "none"},
		{HarnessID: "c", Name: "Update Client", State: harness.Applied, Action: "update"},
		{HarnessID: "d", Name: "Reason Client", State: harness.ApplyConflict, Reason: "different entry"},
		{HarnessID: "e", Name: "Bare Client", State: harness.ApplySkipped},
		{HarnessID: "f", State: harness.Applied, Action: "add"}, // no name: the id is shown
	}
	var lib bytes.Buffer
	installer.PrintChanges(&lib, changes, harness.Scope{})

	var ours strings.Builder
	for _, l := range ClientLines(true, nil, changes) {
		fmt.Fprintf(&ours, "  %-22s %s\n", l.Name, l.Status)
	}
	if ours.String() != lib.String() {
		t.Errorf("ClientLines differs from installer.PrintChanges:\nours:\n%s\nlibrary:\n%s", ours.String(), lib.String())
	}
}

func TestClientLineTones(t *testing.T) {
	lines := ClientLines(false, []harness.Result{
		{State: harness.Applied}, {State: harness.ApplyNoop}, {State: harness.ApplyConflict},
		{State: harness.ApplySkipped}, {State: harness.ApplyFailed},
	}, nil)
	want := []Tone{ToneOK, ToneOK, ToneWarn, ToneWarn, ToneError}
	for i, l := range lines {
		if l.Tone != want[i] {
			t.Errorf("line %d tone = %v, want %v", i, l.Tone, want[i])
		}
	}
}

func TestPrintersSummary(t *testing.T) {
	mk := func(name string, enabled, control, addable bool) PrinterRow {
		p := domain.NewPrinter(name, "192.168.1.1")
		p.Enabled, p.AllowControl = enabled, control
		return PrinterRow{Printer: p, Addable: addable}
	}
	cases := []struct {
		rows []PrinterRow
		want string
	}{
		{nil, "none enabled"},
		{[]PrinterRow{mk("A", false, false, true)}, "none enabled"},
		{[]PrinterRow{mk("K2-5885", true, true, true)}, "1 enabled (K2-5885, control on)"},
		{[]PrinterRow{mk("K2-5885", true, false, true)}, "1 enabled (K2-5885, control off)"},
		{[]PrinterRow{mk("A", true, true, true), mk("B", true, false, true), mk("C", true, true, false)}, "2 enabled (A, control on; B, control off)"},
	}
	for _, c := range cases {
		if got := printersSummary(c.rows); got != c.want {
			t.Errorf("printersSummary = %q, want %q", got, c.want)
		}
	}
}

func finishInput() FinishInput {
	p := domain.NewPrinter("K2-5885", "192.168.1.10")
	p.Enabled = true
	return FinishInput{
		Printers:   []PrinterRow{{Printer: p, Addable: true}},
		Preset:     domain.PresetMonitor,
		Detections: sampleClients(),
		Results: []harness.Result{
			{HarnessID: "claude-code", Name: "Claude Code", State: harness.ApplyNoop},
			{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.Applied},
		},
	}
}

func TestFinishLinesLayout(t *testing.T) {
	in := finishInput()
	text := ansi.Strip(strings.Join(FinishLines(100, in), "\n"))
	for _, want := range []string{
		"  Setup complete.",
		"  Printers        1 enabled (K2-5885, control off)",
		"  Tool preset     monitor",
		"  AI clients      2 registered",
		"  Claude Code     already registered",
		"  Claude Desktop  registered",
		"  Claude Desktop: restart Claude Desktop so it picks up the server",
		"  Next: run `creality-k2-mcp` to open the app, or ask your AI \"what is my printer doing?\".",
		"  Change any of this later with `creality-k2-mcp install`.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("finish text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "restart Claude Code") {
		t.Errorf("a client that was already registered needs no restart line:\n%s", text)
	}
	if strings.Contains(text, "not selected") {
		t.Errorf("both relevant clients are listed, so there is no other-clients line:\n%s", text)
	}
}

func TestFinishLinesCountsOtherClients(t *testing.T) {
	in := finishInput()
	in.Results = in.Results[:1]
	text := ansi.Strip(strings.Join(FinishLines(100, in), "\n"))
	if !strings.Contains(text, "1 other client was not selected.") {
		t.Errorf("the unselected relevant client should be counted:\n%s", text)
	}
	if !strings.Contains(text, "1 registered") {
		t.Errorf("summary should count the one registered client:\n%s", text)
	}
}

func TestFinishLinesDryRun(t *testing.T) {
	in := finishInput()
	in.DryRun = true
	in.Results = nil
	in.Changes = []harness.Change{
		{HarnessID: "claude-code", Name: "Claude Code", State: harness.ApplyNoop},
		{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.Applied, Action: "update"},
	}
	text := ansi.Strip(strings.Join(FinishLines(100, in), "\n"))
	for _, want := range []string{"Dry run: nothing was written.", "2 would be registered", "already registered", "would update", "without --dry-run"} {
		if !strings.Contains(text, want) {
			t.Errorf("dry-run finish text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "restart") || strings.Contains(text, "Next:") {
		t.Errorf("a dry run has no restart lines and no next steps:\n%s", text)
	}
}

func TestFinishLinesNoClientSelected(t *testing.T) {
	in := finishInput()
	in.Results = nil
	text := ansi.Strip(strings.Join(FinishLines(100, in), "\n"))
	if !strings.Contains(text, "none selected") {
		t.Errorf("want none selected:\n%s", text)
	}
}

func TestFinishLinesFitNarrowTerminals(t *testing.T) {
	in := finishInput()
	for _, line := range FinishLines(60, in) {
		if got := ansi.StringWidth(line); got > 59 {
			t.Errorf("line is %d columns wide, limit 59: %q", got, ansi.Strip(line))
		}
	}
}

func TestCancelLine(t *testing.T) {
	cases := []struct {
		dry, wrote, interrupted bool
		command, want           string
	}{
		{true, true, false, "install", "  Dry run cancelled; nothing was written."},
		{false, false, false, "install", "  Setup cancelled; nothing was written."},
		{false, true, false, "install", "  Setup cancelled. The printers and settings you saved were kept; no AI client was changed."},
		{false, true, true, "install", "  Registration was interrupted; run creality-k2-mcp install again."},
		{false, false, true, "add", "  Registration was interrupted; run creality-k2-mcp add again."},
	}
	for _, c := range cases {
		if got := CancelLine(c.dry, c.wrote, c.interrupted, c.command); got != c.want {
			t.Errorf("CancelLine(%v,%v,%v) = %q, want %q", c.dry, c.wrote, c.interrupted, got, c.want)
		}
	}
}

func TestPrintOutcome(t *testing.T) {
	run := func(o Outcome) string {
		var b bytes.Buffer
		PrintOutcome(&b, o)
		return ansi.Strip(b.String())
	}
	fin := finishInput()
	done := &installer.ResultsState{Done: true, Results: fin.Results}
	hs := &installer.HarnessState{Detections: fin.Detections}
	ps := &PrinterState{Rows: fin.Printers}
	ss := &SettingsState{}

	finished := run(Outcome{Printers: ps, Settings: ss, Clients: hs, Results: done, Width: 90})
	if !strings.Contains(finished, "Setup complete.") || !strings.Contains(finished, "Next:") {
		t.Errorf("finished run should print the summary:\n%s", finished)
	}

	// A failed client still prints the rows; a failure with nothing to show prints nothing.
	failed := run(Outcome{Printers: ps, Settings: ss, Clients: hs, Results: done, Failure: fmt.Errorf("boom")})
	if !strings.Contains(failed, "Registration failed.") || !strings.Contains(failed, "Claude Code") {
		t.Errorf("a failure with results keeps the rows:\n%s", failed)
	}
	if got := run(Outcome{Results: &installer.ResultsState{Done: true}, Failure: fmt.Errorf("boom")}); got != "" {
		t.Errorf("a failure with nothing to show printed %q", got)
	}
	if got := run(Outcome{Results: &installer.ResultsState{}, Failure: fmt.Errorf("boom")}); got != "" {
		t.Errorf("a failure before registration printed %q", got)
	}

	// Cancelled variants.
	pw := &PrinterState{Wrote: true}
	for _, c := range []struct {
		name string
		o    Outcome
		want string
	}{
		{"nothing saved", Outcome{Printers: &PrinterState{}, Settings: ss, Results: &installer.ResultsState{}}, "Setup cancelled; nothing was written."},
		{"printers saved", Outcome{Printers: pw, Settings: ss, Results: &installer.ResultsState{}}, "The printers and settings you saved were kept"},
		{"settings saved", Outcome{Printers: &PrinterState{}, Settings: &SettingsState{Wrote: true}, Results: &installer.ResultsState{}}, "The printers and settings you saved were kept"},
		{"dry run", Outcome{DryRun: true, Printers: pw, Results: &installer.ResultsState{}}, "Dry run cancelled; nothing was written."},
		{"interrupted", Outcome{Printers: pw, OnApplyStep: true, Results: &installer.ResultsState{}}, "Registration was interrupted; run creality-k2-mcp install again."},
		{"dry run on the last step", Outcome{DryRun: true, OnApplyStep: true, Results: &installer.ResultsState{}}, "Dry run cancelled; nothing was written."},
	} {
		if got := run(c.o); !strings.Contains(got, c.want) {
			t.Errorf("%s: output %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFinishedMeansDoneWithoutFailure(t *testing.T) {
	// A done run with a failure and results prints the failed heading, never the complete one.
	fin := finishInput()
	var b bytes.Buffer
	PrintOutcome(&b, Outcome{Printers: &PrinterState{Rows: fin.Printers}, Settings: &SettingsState{}, Clients: &installer.HarnessState{Detections: fin.Detections},
		Results: &installer.ResultsState{Done: true, Results: fin.Results}, Failure: fmt.Errorf("x")})
	if strings.Contains(ansi.Strip(b.String()), "Setup complete.") {
		t.Errorf("a failed run must not say complete:\n%s", b.String())
	}
}
