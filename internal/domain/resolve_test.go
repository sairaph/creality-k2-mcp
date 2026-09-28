package domain

import "testing"

func printerAt(id, name string, enabled bool) Printer {
	return Printer{ID: id, Name: name, Host: "192.168.1.1", MoonrakerPort: DefaultMoonrakerPort, Enabled: enabled}
}

func TestResolvePrinterNoQuery(t *testing.T) {
	t.Run("one enabled", func(t *testing.T) {
		printers := []Printer{printerAt("k2-a", "K2-A", true), printerAt("k2-b", "K2-B", false)}
		got, err := ResolvePrinter(printers, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != "k2-a" {
			t.Errorf("got %q, want k2-a", got.ID)
		}
	})

	t.Run("none enabled", func(t *testing.T) {
		printers := []Printer{printerAt("k2-a", "K2-A", false)}
		_, err := ResolvePrinter(printers, "")
		var rerr *ResolveError
		if err == nil {
			t.Fatal("expected an error")
		}
		if !asResolveError(err, &rerr) || rerr.Code != CodeNotFound {
			t.Errorf("error = %v, want CodeNotFound", err)
		}
	})

	t.Run("several enabled", func(t *testing.T) {
		printers := []Printer{printerAt("k2-a", "K2-A", true), printerAt("k2-b", "K2-B", true)}
		_, err := ResolvePrinter(printers, "")
		var rerr *ResolveError
		if !asResolveError(err, &rerr) || rerr.Code != CodeAmbiguous {
			t.Errorf("error = %v, want CodeAmbiguous", err)
		}
		if len(rerr.Candidates) != 2 {
			t.Errorf("Candidates = %v, want 2 entries", rerr.Candidates)
		}
	})
}

func TestResolvePrinterWithQuery(t *testing.T) {
	printers := []Printer{
		printerAt("k2-a", "Kitchen K2", true),
		printerAt("k2-b", "Garage K2", true),
		printerAt("k2-c", "Disabled K2", false),
	}

	t.Run("match by id case insensitive", func(t *testing.T) {
		got, err := ResolvePrinter(printers, "K2-A")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != "k2-a" {
			t.Errorf("got %q, want k2-a", got.ID)
		}
	})

	t.Run("match by name case insensitive", func(t *testing.T) {
		got, err := ResolvePrinter(printers, "garage k2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != "k2-b" {
			t.Errorf("got %q, want k2-b", got.ID)
		}
	})

	t.Run("disabled printer invisible even by exact id", func(t *testing.T) {
		_, err := ResolvePrinter(printers, "k2-c")
		var rerr *ResolveError
		if !asResolveError(err, &rerr) || rerr.Code != CodeNotFound {
			t.Errorf("error = %v, want CodeNotFound", err)
		}
	})

	t.Run("unknown query", func(t *testing.T) {
		_, err := ResolvePrinter(printers, "nope")
		var rerr *ResolveError
		if !asResolveError(err, &rerr) || rerr.Code != CodeNotFound {
			t.Errorf("error = %v, want CodeNotFound", err)
		}
	})

	t.Run("ambiguous name match", func(t *testing.T) {
		dup := []Printer{printerAt("k2-a", "K2", true), printerAt("k2-b", "K2", true)}
		_, err := ResolvePrinter(dup, "k2")
		var rerr *ResolveError
		if !asResolveError(err, &rerr) || rerr.Code != CodeAmbiguous {
			t.Errorf("error = %v, want CodeAmbiguous", err)
		}
	})
}

// asResolveError is a small helper since errors.As needs an addressable
// target of the exact type.
func asResolveError(err error, target **ResolveError) bool {
	rerr, ok := err.(*ResolveError)
	if !ok {
		return false
	}
	*target = rerr
	return true
}
