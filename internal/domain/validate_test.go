package domain

import (
	"strings"
	"testing"
)

func TestValidateHost(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"ipv4", "192.168.1.100", false},
		{"ipv4 loopback", "127.0.0.1", false},
		{"ipv6 bare", "2001:db8::1", false},
		{"ipv6 loopback", "::1", false},
		{"hostname simple", "k2-5885", false},
		{"hostname dotted", "k2-5885.local", false},
		{"hostname trailing dot", "k2-5885.local.", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"leading hyphen label", "-k2-5885", true},
		{"trailing hyphen label", "k2-5885-", true},
		{"underscore not allowed", "k2_5885", true},
		{"space", "k2 5885", true},
		{"empty label", "k2-5885..local", true},
		{"label too long", strings.Repeat("a", 64), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateHost(c.host)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateHost(%q) error = %v, wantErr %v", c.host, err, c.wantErr)
			}
		})
	}
}

func TestValidatePort(t *testing.T) {
	cases := []struct {
		port    int
		wantErr bool
	}{
		{1, false},
		{7125, false},
		{65535, false},
		{0, true},
		{-1, true},
		{65536, true},
	}
	for _, c := range cases {
		if err := ValidatePort(c.port); (err != nil) != c.wantErr {
			t.Errorf("ValidatePort(%d) error = %v, wantErr %v", c.port, err, c.wantErr)
		}
	}
}

func TestDeriveID(t *testing.T) {
	cases := []struct {
		hostname string
		want     string
	}{
		{"K2-5885", "k2-5885"},
		{"k2-5885", "k2-5885"},
		{"  K2-5885  ", "k2-5885"},
		{"F021-ABCD", "f021-abcd"},
	}
	for _, c := range cases {
		if got := DeriveID(c.hostname); got != c.want {
			t.Errorf("DeriveID(%q) = %q, want %q", c.hostname, got, c.want)
		}
	}
}

func validPrinter() Printer {
	return Printer{
		ID:            "k2-5885",
		Name:          "K2-5885",
		Host:          "192.168.1.102",
		MoonrakerPort: DefaultMoonrakerPort,
		Hostname:      "K2-5885",
		Enabled:       true,
	}
}

func TestValidatePrinter(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Printer)
		wantErr bool
	}{
		{"valid", func(p *Printer) {}, false},
		{"empty id", func(p *Printer) { p.ID = "" }, true},
		{"uppercase id", func(p *Printer) { p.ID = "K2-5885" }, true},
		{"id with slash", func(p *Printer) { p.ID = "k2/5885" }, true},
		{"empty name", func(p *Printer) { p.Name = "" }, true},
		{"blank name", func(p *Printer) { p.Name = "   " }, true},
		{"bad host", func(p *Printer) { p.Host = "not a host!" }, true},
		{"bad port zero", func(p *Printer) { p.MoonrakerPort = 0 }, true},
		{"bad port too big", func(p *Printer) { p.MoonrakerPort = 70000 }, true},
		{"bad hostname", func(p *Printer) { p.Hostname = "not a hostname!" }, true},
		{"empty hostname rejected", func(p *Printer) { p.Hostname = "" }, true},
		{"id too long", func(p *Printer) { p.ID = strings.Repeat("a", 65) }, true},
		{"id at max length ok", func(p *Printer) { p.ID = strings.Repeat("a", 64) }, false},
		{"id trailing dot", func(p *Printer) { p.ID = "k2-5885." }, true},
		{"id reserved device name con", func(p *Printer) { p.ID = "con" }, true},
		{"id reserved device name with extension", func(p *Printer) { p.ID = "con.txt" }, true},
		{"id reserved device name com1", func(p *Printer) { p.ID = "com1" }, true},
		{"id reserved device name lpt9", func(p *Printer) { p.ID = "lpt9" }, true},
		{"id looks reserved but is not", func(p *Printer) { p.ID = "console" }, false},
		{"id looks reserved but is not comma", func(p *Printer) { p.ID = "com10" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validPrinter()
			c.mutate(&p)
			err := ValidatePrinter(p)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidatePrinter() error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestValidateRegistryDuplicateHostname(t *testing.T) {
	a := validPrinter()
	b := validPrinter()
	b.ID = "k2-5885-dup"
	b.Name = "K2-5885 dup"

	reg := Registry{Printers: []Printer{a, b}}
	if err := ValidateRegistry(reg); err == nil {
		t.Error("ValidateRegistry() with duplicate hostnames = nil, want error")
	}
}

func TestValidateRegistryDuplicateID(t *testing.T) {
	a := validPrinter()
	b := validPrinter()
	b.Hostname = "K2-9999"
	// same id as a
	reg := Registry{Printers: []Printer{a, b}}
	if err := ValidateRegistry(reg); err == nil {
		t.Error("ValidateRegistry() with duplicate ids = nil, want error")
	}
}

func TestValidateRegistryDistinctHostnamesOK(t *testing.T) {
	a := validPrinter()
	b := validPrinter()
	b.ID = "k2-9999"
	b.Name = "K2-9999"
	b.Hostname = "K2-9999"
	reg := Registry{Printers: []Printer{a, b}}
	if err := ValidateRegistry(reg); err != nil {
		t.Errorf("ValidateRegistry() = %v, want nil", err)
	}
}

func TestIsReservedDeviceName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"CON", true}, {"con", true}, {"Con.txt", true},
		{"PRN", true}, {"AUX", true}, {"NUL", true},
		{"COM1", true}, {"com9", true}, {"LPT1", true}, {"lpt9", true},
		{"COM0", false}, {"COM10", false}, {"LPT0", false}, {"LPT10", false},
		{"console", false}, {"printer", false}, {"", false},
	}
	for _, c := range cases {
		if got := IsReservedDeviceName(c.name); got != c.want {
			t.Errorf("IsReservedDeviceName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasTrailingDotOrSpace(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"abc", false}, {"abc.", true}, {"abc ", true}, {"a.b.c", false}, {"", false},
	}
	for _, c := range cases {
		if got := HasTrailingDotOrSpace(c.name); got != c.want {
			t.Errorf("HasTrailingDotOrSpace(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestValidateRegistryRejectsEmptyHostname(t *testing.T) {
	a := validPrinter()
	a.Hostname = ""
	reg := Registry{Printers: []Printer{a}}
	if err := ValidateRegistry(reg); err == nil {
		t.Error("ValidateRegistry() with a blank hostname = nil, want error")
	}
}
