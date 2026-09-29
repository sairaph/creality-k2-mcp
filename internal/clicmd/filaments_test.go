package clicmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// boxsWS reports one CFS unit with invented slots (plan decision V1) and
// records that only reads happened: it has no write methods at all.
type boxsWS struct {
	fakeWS9999Client
	err error
}

func (b boxsWS) BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error) {
	if b.err != nil {
		return crealityws.BoxsInfo{}, b.err
	}
	one, zero := 1, 0
	mn, mx := 190.0, 240.0
	slot := func(id int, name, rfid, color string) crealityws.SlotMaterial {
		return crealityws.SlotMaterial{ID: id, Vendor: "Acme", Type: "PLA", Name: name, RFID: rfid, Color: color, MinTemp: &mn, MaxTemp: &mx,
			State: &one, Selected: &zero, EditStatus: &one}
	}
	return crealityws.BoxsInfo{
		MaterialBoxs: []crealityws.MaterialBox{{ID: 1, Type: 0, State: 1, Name: "TESTBOX", Materials: []crealityws.SlotMaterial{
			slot(0, "Acme Test PLA", "99001", "#0ff0000"), slot(1, "Acme Test PLA", "99001", "#0ff0000")}}},
		SameMaterialOK: true,
		SameMaterial:   []crealityws.SameGroup{{Code: "099001", Color: "0ff0000", Name: "PLA", Slots: []crealityws.SlotRef{{BoxID: 1, MaterialID: 0}, {BoxID: 1, MaterialID: 1}}}},
	}, nil
}

func filamentsDeps(t *testing.T, stdout, stderr *bytes.Buffer, ws printerstate.WS9999Client) Deps {
	t.Helper()
	moon := &fakeMoonrakerClient{serverInfoErr: context.DeadlineExceeded}
	return Deps{Stdout: stdout, Stderr: stderr, PrinterClients: func(p domain.Printer) printerstate.Deps {
		return printerstate.Deps{Moonraker: moon, WS9999: ws}
	}}
}

func registerFilamentsPrinter(t *testing.T) {
	t.Helper()
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125, Hostname: "K2-5885", Enabled: true})
}

func TestRunFilamentsText(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{}), nil)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"K2-5885", "Unit T1 (TESTBOX)", "T1A", "defined, Acme Test PLA, PLA, #ff0000", "editable", "T1A+T1B (PLA #ff0000)", "cannot be detected"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunFilamentsJSON(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{}), []string{"--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var view struct {
		NamesAvailable bool `json:"names_available"`
		Units          []struct {
			Unit  string `json:"unit"`
			Slots []struct {
				Slot     string `json:"slot"`
				Status   string `json:"status"`
				Editable bool   `json:"editable"`
			} `json:"slots"`
		} `json:"units"`
		RefillGroups []string `json:"refill_groups"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout.String())
	}
	if !view.NamesAvailable || len(view.Units) != 1 || len(view.Units[0].Slots) != 2 || view.Units[0].Slots[0].Status != "defined" || len(view.RefillGroups) != 1 {
		t.Fatalf("view = %+v", view)
	}
}

// With port 9999 unreachable the command still succeeds and says plainly that
// names are missing.
func TestRunFilamentsNinetyNineDown(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{err: errors.New("no route")}), nil)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Problem: port 9999 could not be read") || !strings.Contains(out, "names are missing") {
		t.Errorf("output:\n%s", out)
	}
}

// A client that cannot read 9999 filament data at all is the same fallback.
func TestRunFilamentsClientWithoutFilamentReads(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, fakeWS9999Client{}), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), "Problem:") {
		t.Errorf("output:\n%s", stdout.String())
	}
}

func TestRunFilamentsUsageAndUnknownPrinter(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{}), []string{"a", "b"}); code != 2 || !strings.Contains(stderr.String(), "usage: filaments") {
		t.Fatalf("two args: code %d stderr %s", code, stderr.String())
	}
	stderr.Reset()
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{}), []string{"nope"}); code != 1 || !strings.Contains(stderr.String(), "filaments:") {
		t.Fatalf("unknown printer: code %d stderr %s", code, stderr.String())
	}
	stdout.Reset()
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{}), []string{"-h"}); code != 0 || !strings.Contains(stdout.String(), "usage: filaments") {
		t.Fatalf("help: code %d", code)
	}
}

// Flags may follow the printer argument, as documented.
func TestRunFilamentsAndStatusAcceptTheJSONFlagAfterThePrinter(t *testing.T) {
	registerFilamentsPrinter(t)
	for name, run := range map[string]func(Deps, []string) int{
		"filaments": func(d Deps, a []string) int { return RunFilaments(context.Background(), d, a) },
		"status":    func(d Deps, a []string) int { return RunStatus(context.Background(), d, a) },
	} {
		var stdout, stderr bytes.Buffer
		if code := run(filamentsDeps(t, &stdout, &stderr, boxsWS{}), []string{"k2-5885", "--json"}); code != 0 {
			t.Fatalf("%s k2-5885 --json: exit %d: %s", name, code, stderr.String())
		}
		var v map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
			t.Fatalf("%s: not JSON: %v\n%s", name, err, stdout.String())
		}
	}
}

// The command shows the derived state and the CFS state, like get_filaments.
func TestRunFilamentsShowsTheStateAndWhyEditsAreBlocked(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, boxsWS{}), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := stdout.String()
	// The fake Moonraker answers offline, so no edit is allowed and the reason is shown.
	for _, want := range []string{"State: offline", "Editing a slot is not possible right now", "the printer does not allow an edit right now", "not editable"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunFilamentsDoesNotRepeatTheBrand(t *testing.T) {
	registerFilamentsPrinter(t)
	var stdout, stderr bytes.Buffer
	ws := brandedWS{}
	if code := RunFilaments(context.Background(), filamentsDeps(t, &stdout, &stderr, ws), nil); code != 0 {
		t.Fatal(stderr.String())
	}
	if strings.Contains(stdout.String(), "Generic Generic") || !strings.Contains(stdout.String(), "Generic PETG") {
		t.Errorf("output:\n%s", stdout.String())
	}
}

type brandedWS struct{ boxsWS }

func (brandedWS) BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error) {
	b, err := boxsWS{}.BoxsInfo(ctx)
	for i := range b.MaterialBoxs[0].Materials {
		b.MaterialBoxs[0].Materials[i].Vendor, b.MaterialBoxs[0].Materials[i].Name = "Generic", "Generic PETG"
	}
	return b, err
}
