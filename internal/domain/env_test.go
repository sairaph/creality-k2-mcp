package domain

import "testing"

func TestEnvOverrideUnset(t *testing.T) {
	t.Setenv(EnvHost, "")
	_, ok, err := EnvOverride()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok = true with K2_MCP_HOST unset, want false")
	}
}

func TestEnvOverrideDefaults(t *testing.T) {
	t.Setenv(EnvHost, "192.168.1.50")
	t.Setenv(EnvPort, "")
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvAllowControl, "")

	p, ok, err := EnvOverride()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.Host != "192.168.1.50" {
		t.Errorf("Host = %q, want 192.168.1.50", p.Host)
	}
	if p.MoonrakerPort != DefaultMoonrakerPort {
		t.Errorf("MoonrakerPort = %d, want %d", p.MoonrakerPort, DefaultMoonrakerPort)
	}
	if !p.Enabled {
		t.Error("Enabled = false, want true")
	}
	if p.AllowControl {
		t.Error("AllowControl = true, want false (K2_MCP_ALLOW_CONTROL not set)")
	}
	if p.ID != EnvPrinterID {
		t.Errorf("ID = %q, want %q", p.ID, EnvPrinterID)
	}
}

func TestEnvOverrideFull(t *testing.T) {
	t.Setenv(EnvHost, "printer.local")
	t.Setenv(EnvPort, "7126")
	t.Setenv(EnvAPIKey, "secret")
	t.Setenv(EnvAllowControl, "1")

	p, ok, err := EnvOverride()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.MoonrakerPort != 7126 {
		t.Errorf("MoonrakerPort = %d, want 7126", p.MoonrakerPort)
	}
	if p.APIKey != "secret" {
		t.Errorf("APIKey = %q, want secret", p.APIKey)
	}
	if !p.AllowControl {
		t.Error("AllowControl = false, want true")
	}
}

func TestEnvOverrideAllowControlOnlyExactlyOne(t *testing.T) {
	t.Setenv(EnvHost, "printer.local")
	t.Setenv(EnvAllowControl, "true")

	p, ok, err := EnvOverride()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.AllowControl {
		t.Error("AllowControl = true for K2_MCP_ALLOW_CONTROL=true, want false (only \"1\" enables it)")
	}
}

func TestEnvOverrideInvalidHost(t *testing.T) {
	t.Setenv(EnvHost, "not a host!")
	_, _, err := EnvOverride()
	if err == nil {
		t.Error("EnvOverride() with invalid host = nil error, want error")
	}
}

func TestEnvOverrideInvalidPort(t *testing.T) {
	t.Setenv(EnvHost, "printer.local")
	t.Setenv(EnvPort, "not-a-number")
	if _, _, err := EnvOverride(); err == nil {
		t.Error("EnvOverride() with non-numeric port = nil error, want error")
	}

	t.Setenv(EnvPort, "70000")
	if _, _, err := EnvOverride(); err == nil {
		t.Error("EnvOverride() with out-of-range port = nil error, want error")
	}
}
