// Package gcodeinfo parses G-code header/footer comments and local 3MF
// project archives, with no network access of its own: the caller supplies
// the bytes (a local file read, or a Moonraker HeadRange read from
// internal/moonraker for a printer-hosted file).
//
// The primary dialect this package understands is the Creality Print /
// OrcaSlicer family: a commented HEADER_BLOCK and THUMBNAIL_BLOCK near the
// top of the file, MINX..MAXZ bounds, and a large "key = value" comment
// dump (Creality Print's CONFIG_BLOCK) near the bottom that doubles as the
// same settings a 3MF's Metadata/project_settings.config carries. This is
// what a live K2 running Creality Print actually emits
// (references/printer-snapshot/extra/light_test_20260928.md, "HTTP Range on
// file download") and what a captured F021 sample gcode
// (references/K2_Series_Klipper/gcodes/F021/3DBench_PLA_14m.gcode) confirms
// byte-for-byte, including a real-world quirk this package must tolerate:
// two settings comments concatenated on one line with no separating
// newline ("; filament cost = 0.22; total filament used [g] = 10.87"). For
// that reason every field extractor here searches the whole head/tail text
// with an unanchored regex rather than matching strict single-line comments
// the way the Python reference server did (references/analysis/01-python-
// server.md section 5): that server's five line-anchored regexes silently
// returned nothing for any other dialect and gave no "not recognised"
// signal, which this package fixes by always reporting a Dialect, including
// "unknown".
//
// Cura, PrusaSlicer and SuperSlicer comment dialects are recognised where
// doing so is cheap (a generator line, or Cura's ";TIME:"/";Layer height:"
// colon-style comments), matching the slicer families Klipper's own
// metadata.py already knows about (references/K2_Series_Klipper/klippy/
// extras/metadata.py). Anything else reports "dialect: unknown" rather than
// a silently near-empty result.
//
// Every exported type here carries yaml tags so it can be dropped straight
// into a YAML frontmatter document. Thumbnail image bytes are always
// returned as a separate slice, never inlined into a yaml-tagged struct, so
// the metadata result stays small and frontmatter-safe; the tool layer is
// expected to turn that separate slice into MCP image content.
package gcodeinfo
