package domain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func mustMarshalForTest(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRegistryPathProjectWinsWhenPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows reads USERPROFILE

	dir := t.TempDir()
	projectPath := ProjectRegistryPath(dir)
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`{"version":1,"printers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	path, isProject, err := RegistryPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !isProject {
		t.Error("isProject = false, want true")
	}
	if path != projectPath {
		t.Errorf("path = %q, want %q", path, projectPath)
	}
}

func TestRegistryPathFallsBackToGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir() // no project file written
	path, isProject, err := RegistryPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if isProject {
		t.Error("isProject = true, want false")
	}
	want, err := GlobalRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestLoadRegistryFileMissing(t *testing.T) {
	dir := t.TempDir()
	reg, warnings, err := LoadRegistryFile(filepath.Join(dir, "printers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(reg.Printers) != 0 {
		t.Errorf("Printers = %v, want empty", reg.Printers)
	}
}

func TestSaveAndLoadRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "printers.json")

	reg := Registry{Printers: []Printer{validPrinter()}}
	if err := SaveRegistry(path, reg); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 && !isWindows() {
		t.Errorf("file mode = %v, want 0600", mode)
	}

	loaded, warnings, err := LoadRegistryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(loaded.Printers) != 1 || loaded.Printers[0].ID != "k2-5885" {
		t.Errorf("Printers = %+v, want one entry k2-5885", loaded.Printers)
	}
	if loaded.Version != registryVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, registryVersion)
	}
}

func TestLoadRegistryFileDedupesByHostnameWithWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "printers.json")

	a := validPrinter()
	b := validPrinter()
	b.ID = "k2-5885-second"
	b.Name = "second entry"
	// b.Hostname is the same as a's: "K2-5885", differing only in case to
	// prove the comparison is case insensitive.
	b.Hostname = "k2-5885"

	reg := Registry{Version: registryVersion, Printers: []Printer{a, b}}
	data := mustMarshalForTest(t, reg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, warnings, err := LoadRegistryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Printers) != 1 {
		t.Fatalf("Printers = %+v, want exactly one kept", loaded.Printers)
	}
	if loaded.Printers[0].ID != "k2-5885" {
		t.Errorf("kept printer = %q, want the first entry k2-5885", loaded.Printers[0].ID)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
}

func TestLoadRegistryFileDropsInvalidEntryWithWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "printers.json")

	good := validPrinter()
	bad := validPrinter()
	bad.ID = "k2-9999"
	bad.Hostname = "K2-9999"
	bad.Host = "not a host!"

	reg := Registry{Version: registryVersion, Printers: []Printer{good, bad}}
	data := mustMarshalForTest(t, reg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, warnings, err := LoadRegistryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Printers) != 1 || loaded.Printers[0].ID != good.ID {
		t.Fatalf("Printers = %+v, want exactly the valid entry %q", loaded.Printers, good.ID)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
}

func TestAddPrinterRejectsDuplicateHostname(t *testing.T) {
	reg := Registry{Printers: []Printer{validPrinter()}}
	dup := validPrinter()
	dup.ID = "k2-5885-other"
	dup.Name = "other"

	_, err := AddPrinter(reg, dup)
	if err == nil {
		t.Error("AddPrinter() with duplicate hostname = nil error, want error")
	}
}

func TestAddPrinterRejectsDuplicateID(t *testing.T) {
	reg := Registry{Printers: []Printer{validPrinter()}}
	dup := validPrinter()
	dup.Hostname = "K2-9999"

	_, err := AddPrinter(reg, dup)
	if err == nil {
		t.Error("AddPrinter() with duplicate id = nil error, want error")
	}
}

func TestAddPrinterRejectsInvalidEntry(t *testing.T) {
	reg := Registry{}
	bad := validPrinter()
	bad.Host = "not a host!"
	if _, err := AddPrinter(reg, bad); err == nil {
		t.Error("AddPrinter() with invalid host = nil error, want error")
	}
}

func TestAddPrinterAppends(t *testing.T) {
	reg := Registry{}
	next, err := AddPrinter(reg, validPrinter())
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Printers) != 1 {
		t.Fatalf("Printers = %+v, want one entry", next.Printers)
	}
}

func TestRegistryEnabled(t *testing.T) {
	reg := Registry{Printers: []Printer{
		printerAt("k2-a", "A", true),
		printerAt("k2-b", "B", false),
		printerAt("k2-c", "C", true),
	}}
	enabled := reg.Enabled()
	if len(enabled) != 2 {
		t.Fatalf("Enabled() = %+v, want 2 entries", enabled)
	}
}

func TestNewPrinterDerivesIDAndDefaults(t *testing.T) {
	p := NewPrinter("K2-5885", "192.168.1.102")
	if p.ID != "k2-5885" {
		t.Errorf("ID = %q, want k2-5885", p.ID)
	}
	if p.Name != "K2-5885" {
		t.Errorf("Name = %q, want K2-5885", p.Name)
	}
	if p.MoonrakerPort != DefaultMoonrakerPort {
		t.Errorf("MoonrakerPort = %d, want %d", p.MoonrakerPort, DefaultMoonrakerPort)
	}
	if p.AddedAt.IsZero() {
		t.Error("AddedAt is zero, want set")
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }
