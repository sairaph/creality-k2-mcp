// Printer resolution for tools (plan decision 5): an optional printer
// argument (id or name, case insensitive), defaulting to the one enabled
// printer, with disabled printers invisible.
package domain

import (
	"fmt"
	"strings"
)

// ResolveCode identifies the kind of resolution failure, so a tool layer can
// map it to a render error code without inspecting message text.
type ResolveCode string

const (
	// CodeNotFound: the query matched no enabled printer.
	CodeNotFound ResolveCode = "not_found"
	// CodeAmbiguous: no query was given and more than one printer is
	// enabled, or the query matched more than one.
	CodeAmbiguous ResolveCode = "ambiguous"
)

// ResolveError reports why ResolvePrinter could not return exactly one
// printer. Candidates is populated for CodeAmbiguous so the caller can list
// them.
type ResolveError struct {
	Code       ResolveCode
	Query      string
	Candidates []Printer
}

func (e *ResolveError) Error() string {
	switch e.Code {
	case CodeAmbiguous:
		names := make([]string, 0, len(e.Candidates))
		for _, p := range e.Candidates {
			names = append(names, fmt.Sprintf("%s (%s)", p.ID, p.Name))
		}
		if e.Query == "" {
			return fmt.Sprintf("multiple printers are enabled; specify one: %s", strings.Join(names, ", "))
		}
		return fmt.Sprintf("%q matches more than one printer: %s", e.Query, strings.Join(names, ", "))
	case CodeNotFound:
		if e.Query == "" {
			return "no printer is enabled"
		}
		return fmt.Sprintf("no enabled printer matches %q", e.Query)
	default:
		return "printer resolution failed"
	}
}

// ResolvePrinter picks the printer a tool call should act on. Disabled
// printers are never matched, whether by an explicit query or by the
// single-enabled-printer default:
//
//   - query == "": exactly one enabled printer is used; zero is CodeNotFound;
//     more than one is CodeAmbiguous listing every enabled printer.
//   - query != "": matched case-insensitively against id or name among
//     enabled printers; zero matches is CodeNotFound; more than one match
//     (e.g. two enabled printers that happen to share a name) is
//     CodeAmbiguous listing the matches.
func ResolvePrinter(printers []Printer, query string) (*Printer, error) {
	enabled := make([]Printer, 0, len(printers))
	for _, p := range printers {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}

	query = strings.TrimSpace(query)
	if query == "" {
		switch len(enabled) {
		case 0:
			return nil, &ResolveError{Code: CodeNotFound}
		case 1:
			p := enabled[0]
			return &p, nil
		default:
			return nil, &ResolveError{Code: CodeAmbiguous, Candidates: enabled}
		}
	}

	var matches []Printer
	for _, p := range enabled {
		if strings.EqualFold(p.ID, query) || strings.EqualFold(p.Name, query) {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 0:
		return nil, &ResolveError{Code: CodeNotFound, Query: query}
	case 1:
		p := matches[0]
		return &p, nil
	default:
		return nil, &ResolveError{Code: CodeAmbiguous, Query: query, Candidates: matches}
	}
}
