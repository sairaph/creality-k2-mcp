package domain

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// rfc1123Label matches one label of an RFC 1123 hostname: alphanumeric,
// interior hyphens allowed, 1 to 63 characters, not starting or ending with a
// hyphen.
var rfc1123Label = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// ValidateHost reports whether host is usable as a printer address: an IPv4
// literal, an IPv6 literal (bare, no brackets), or an RFC 1123 hostname.
func ValidateHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	return validateHostname(host)
}

func validateHostname(host string) error {
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return fmt.Errorf("invalid host %q: must be 1 to 253 characters", host)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("invalid host %q: label %q must be 1 to 63 characters", host, label)
		}
		if !rfc1123Label.MatchString(label) {
			return fmt.Errorf("invalid host %q: label %q must be alphanumeric or hyphens, and not start or end with a hyphen", host, label)
		}
	}
	return nil
}

// ValidatePort reports whether port is a usable TCP port number.
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d must be between 1 and 65535", port)
	}
	return nil
}

// DeriveID derives a stable registry id from a Klipper hostname, e.g.
// "K2-5885" becomes "k2-5885". The id never changes once a printer is added,
// even if the user later edits the display name.
func DeriveID(hostname string) string {
	return strings.ToLower(strings.TrimSpace(hostname))
}

// hostnameKey normalizes a hostname for duplicate detection: case
// insensitive, trailing dot ignored. An empty key never collides with
// anything; ValidatePrinter now rejects an empty hostname before a Printer
// reaches this comparison in ValidateRegistry, AddPrinter or
// dedupeByHostname, so this only guards a Printer value nobody has validated
// yet (the ad hoc K2_MCP_HOST override, which is never run through these
// registry-file checks; see env.go and lock.go).
func hostnameKey(hostname string) string {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	return strings.TrimSuffix(hostname, ".")
}

// idPattern matches a registry id: lowercase alphanumeric, hyphen, underscore
// or dot, 1 to 64 characters.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// reservedDeviceNames is the classic MS-DOS/Windows reserved device name
// list: a file or directory literally named one of these (with or without an
// extension, e.g. "con" or "con.txt") cannot be created on NTFS or FAT, so
// refusing them here keeps every id usable as a real path component on
// Windows (review backlog item 38). internal/daemon's recording id segments
// reuse this same list via IsReservedDeviceName rather than duplicating it.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// IsReservedDeviceName reports whether name is one of the Windows reserved
// device names (case insensitive), ignoring anything from the first '.'
// onward so "CON.txt" is caught the same as "CON" (review backlog item 38).
func IsReservedDeviceName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return reservedDeviceNames[strings.ToUpper(base)]
}

// HasTrailingDotOrSpace reports whether name ends with '.' or ' '. Windows
// silently strips a trailing dot or space from a path component, so a name
// that differs from another only by one would collide with it on that
// platform (review backlog item 38).
func HasTrailingDotOrSpace(name string) bool {
	return strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ")
}

// ValidatePrinter checks one registry entry in isolation (no comparison
// against other entries; that is ValidateRegistry's job).
func ValidatePrinter(p Printer) error {
	if p.ID == "" {
		return fmt.Errorf("printer id must not be empty")
	}
	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("printer id %q must be lowercase alphanumeric, hyphen, underscore or dot", p.ID)
	}
	if HasTrailingDotOrSpace(p.ID) {
		return fmt.Errorf("printer id %q must not end with a dot or space", p.ID)
	}
	if IsReservedDeviceName(p.ID) {
		return fmt.Errorf("printer id %q must not be a Windows reserved device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9)", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("printer %q: name must not be empty", p.ID)
	}
	if err := ValidateHost(p.Host); err != nil {
		return fmt.Errorf("printer %q: %w", p.ID, err)
	}
	if err := ValidatePort(p.MoonrakerPort); err != nil {
		return fmt.Errorf("printer %q: %w", p.ID, err)
	}
	if strings.TrimSpace(p.Hostname) == "" {
		return fmt.Errorf("printer %q: hostname must not be empty", p.ID)
	}
	if err := ValidateHost(p.Hostname); err != nil {
		return fmt.Errorf("printer %q: hostname: %w", p.ID, err)
	}
	return nil
}

// ValidateRegistry checks every entry and the invariants that span entries:
// unique ids, and no two entries resolving to the same Klipper hostname
// (safety-architecture 3.4; the lock and the cross-process guard are keyed by
// hostname, so two entries for one printer would let them run concurrently
// unserialized).
func ValidateRegistry(reg Registry) error {
	ids := make(map[string]bool, len(reg.Printers))
	hostnames := make(map[string]string, len(reg.Printers))
	for _, p := range reg.Printers {
		if err := ValidatePrinter(p); err != nil {
			return err
		}
		if ids[p.ID] {
			return fmt.Errorf("duplicate printer id %q", p.ID)
		}
		ids[p.ID] = true

		if key := hostnameKey(p.Hostname); key != "" {
			if existing, ok := hostnames[key]; ok {
				return fmt.Errorf("printer %q and %q both resolve to hostname %q", existing, p.ID, p.Hostname)
			}
			hostnames[key] = p.ID
		}
	}
	return nil
}
