package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/gcodeinfo"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// This file implements the "Files" tool group (dev_docs/plan-v0.1.0.md's
// tool surface, T8): list_gcode_files, get_gcode_file, inspect_local_gcode,
// inspect_local_3mf (monitor, read-only) and upload_gcode_file,
// delete_gcode_file (control, destructive, through internal/policy's
// two-phase proposal_token flow - dev_docs/safety-architecture.md 4.2 and
// section 10 D3). Every tool still resolves a printer and embeds
// printerstate.StateBlock in its frontmatter (safety-architecture.md
// section 5), even inspect_local_gcode/inspect_local_3mf, which touch no
// printer state themselves: this keeps the printer's context (what is
// currently printing, whether control tools are available) alongside a
// local file's inspection, and matches every other tool in this project.

func registerFilesTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "list_gcode_files", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "list_gcode_files",
		Description: "Lists the gcode files on this printer's gcodes root, paginated, with each file's size and " +
			"last-modified time, the gcodes root's total/used/free disk usage in the frontmatter, and the file " +
			"currently printing (if any) marked in its own row and named separately in the frontmatter. Sortable " +
			"by modified (default, newest first), name (A-Z) or size (largest first) via sort_by. Call this to " +
			"pick a file before start_print, get_gcode_file or delete_gcode_file, or just to check free space " +
			"before uploading. Related: get_gcode_file for one file's full slicer header and thumbnail, " +
			"get_current_job for the file actually printing right now.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listGCodeFilesHandler(s))

	registerTool(s, domain.ToolInfo{Name: "get_gcode_file", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "get_gcode_file",
		Description: "Returns one gcode file's Moonraker metadata plus slicer header fields parsed from the " +
			"start and end of the file itself (dialect, generator, total layers, filament and bounds, and the " +
			"settings/estimate block), and returns the largest embedded thumbnail as image content when the file " +
			"carries one. Only the first 1 MiB and, for a file bigger than that, the last 256 KiB are read over " +
			"the network. A warning is added when the slicer dialect could not be recognised. Related: " +
			"list_gcode_files to find a filename first, inspect_local_gcode for the same inspection on a file " +
			"that is still only on the user's own machine.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getGCodeFileHandler(s))

	registerTool(s, domain.ToolInfo{Name: "inspect_local_gcode", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "inspect_local_gcode",
		Description: "Inspects a gcode file on the machine this server runs on (never the printer): validates " +
			"the path, then parses the same slicer header fields get_gcode_file reports (dialect, generator, " +
			"layers, filament, bounds, estimate) from a bounded head and tail read, and returns the largest " +
			"embedded thumbnail as image content when present. path may be absolute or relative to this server's " +
			"own working directory; it must be a regular file under a sane size limit. This never touches a " +
			"printer or uploads anything. Related: upload_gcode_file to actually send the file to a printer's " +
			"gcodes root afterward, inspect_local_3mf for a slicer project archive instead of a plain gcode file.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, inspectLocalGCodeHandler(s))

	registerTool(s, domain.ToolInfo{Name: "inspect_local_3mf", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "inspect_local_3mf",
		Description: "Inspects a 3MF slicer project archive on the machine this server runs on (never the " +
			"printer): validates the path, safely opens it as a zip (bounded entry count and size, rejecting " +
			"path traversal), and returns its plates, model objects, printer/filament settings and the first " +
			"plate's thumbnail (or the legacy single thumbnail) as image content. Settings are a filtered, " +
			"curated printer/filament/process summary by default; pass full_settings: true for every settings " +
			"key from the project instead, capped in size, with settings_count always reporting the true total " +
			"either way. This never touches a printer. Related: inspect_local_gcode for a plain gcode file " +
			"instead of a project archive.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, inspectLocal3MFHandler(s))

	registerTool(s, domain.ToolInfo{Name: "upload_gcode_file", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "upload_gcode_file",
		Description: "Uploads a local gcode file to this printer's gcodes root. This never starts a print by " +
			"itself; call start_print separately afterward if that is wanted. Uploading over the file currently " +
			"printing is refused outright. Uploading over any other existing file requires the two-step proposal " +
			"flow: call once with no confirm_token to get a proposal, then again with the same path and filename " +
			"plus that confirm_token to actually overwrite. Uploading to a new filename needs no confirm_token at " +
			"all and sends immediately. Related: inspect_local_gcode to check the file first, list_gcode_files to " +
			"see what is already there, delete_gcode_file to remove a file instead.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, uploadGCodeFileHandler(s))

	registerTool(s, domain.ToolInfo{Name: "delete_gcode_file", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "delete_gcode_file",
		Description: "Deletes one file from this printer's gcodes root. Deleting the file currently printing is " +
			"refused outright. Every other delete requires the two-step proposal flow: call once with no " +
			"confirm_token to see what will happen, then again with the same filename plus that confirm_token to " +
			"actually remove it. This cannot be undone. Related: list_gcode_files to confirm the filename first, " +
			"upload_gcode_file to replace a file instead of just removing it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, deleteGCodeFileHandler(s))
}

// filesClient is the subset of *moonraker.Client the read tools in this file
// need beyond printerstate.MoonrakerClient (which stops well short of the
// file-manager endpoints): Directory and HeadRange/TailRange for
// list_gcode_files/get_gcode_file, List and Metadata for completeness with
// the same pattern. deps.Moonraker's static type is the narrower
// printerstate.MoonrakerClient interface, so this type assertion recovers
// the richer one; both production (DefaultPrinterClients) and every test
// (clientsFor) hand back a real *moonraker.Client, which always satisfies it
// - the same pattern tools_status.go's historyTotalsClient already uses.
type filesClient interface {
	List(ctx context.Context) ([]moonraker.GCodeFile, error)
	Metadata(ctx context.Context, filename string) (moonraker.FileMetadata, error)
	Directory(ctx context.Context, path string) (moonraker.Directory, error)
	HeadRange(ctx context.Context, filename string, n int64) ([]byte, error)
	TailRange(ctx context.Context, filename string, n int64) ([]byte, error)
}

// getGCodeFileTailBytes bounds the tail read get_gcode_file makes for a file
// bigger than the head cap: a settings/estimate block sits in the last few
// KiB of a Creality Print/OrcaSlicer file (references/analysis header
// captures), so this is much smaller than gcodeinfo.DefaultMaxHeadBytes.
const getGCodeFileTailBytes int64 = 256 << 10 // 256 KiB

// maxLocalFileBytes bounds every local file this file's tools will open at
// all (inspect_local_gcode, inspect_local_3mf, upload_gcode_file): a sanity
// ceiling independent of gcodeinfo's own bounded head/tail reads and 3MF
// zip-entry caps, refusing something absurdly large - or a device node
// masquerading as a regular file - before ever opening it.
const maxLocalFileBytes = 4 << 30 // 4 GiB

// resolveLocalFile validates path (absolute, or relative to this process's
// own working directory) and returns its absolute form and size: it must
// exist, be a regular file (not a directory, and not a special file such as
// a device or named pipe), and be at most maxLocalFileBytes. It uses
// os.Lstat and refuses a symlink outright, the same posture as
// daemon.VerifyDir (dev_docs/review-backlog.md item 59): a path handed to
// these tools should never be silently followed through a link a caller
// does not control.
func resolveLocalFile(path string) (absPath string, size int64, errRes *mcp.CallToolResult) {
	if strings.TrimSpace(path) == "" {
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: "path must not be empty",
			Hint:    "Pass an absolute path, or one relative to this server's own working directory.",
		})
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("could not resolve path %q: %v", path, err),
		})
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, render.ErrorResult(render.Error{
				Code:    render.CodeNotFound,
				Message: fmt.Sprintf("no file at %s", abs),
				Hint:    "Check the path; it can be absolute or relative to this server's own working directory.",
			})
		}
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("could not stat %s: %v", abs, err),
		})
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("%s is a symlink, refusing to trust it", abs),
			Hint:    "Pass the real file path directly; symlinks are refused the same way the daemon's own directory checks refuse them.",
		})
	}
	if info.IsDir() {
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("%s is a directory, not a file", abs),
		})
	}
	if !info.Mode().IsRegular() {
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("%s is not a regular file", abs),
		})
	}
	if info.Size() > maxLocalFileBytes {
		return "", 0, render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("%s is %d bytes, over this server's %d byte per-file limit", abs, info.Size(), maxLocalFileBytes),
		})
	}
	return abs, info.Size(), nil
}

// largestThumbnail returns the thumbnail with the greatest pixel area among
// thumbs that actually carry image data (a truncated or rejected thumbnail
// has none), or nil if none qualify.
func largestThumbnail(thumbs []gcodeinfo.Thumbnail) *gcodeinfo.Thumbnail {
	var best *gcodeinfo.Thumbnail
	for i := range thumbs {
		t := &thumbs[i]
		if len(t.Data) == 0 {
			continue
		}
		if best == nil || t.Width*t.Height > best.Width*best.Height {
			best = t
		}
	}
	return best
}

// firstPlateThumbnail returns a 3MF's lowest-indexed plate thumbnail (label
// "plate_N"), falling back to the legacy single "thumbnail" entry, then to
// any thumbnail carrying data, matching inspect_local_3mf's contract ("the
// first plate's thumbnail, or the legacy single thumbnail").
func firstPlateThumbnail(thumbs []gcodeinfo.Thumbnail) *gcodeinfo.Thumbnail {
	var best *gcodeinfo.Thumbnail
	bestIndex := -1
	for i := range thumbs {
		t := &thumbs[i]
		if len(t.Data) == 0 || !strings.HasPrefix(t.Label, "plate_") {
			continue
		}
		idx, err := parsePlateIndex(t.Label)
		if err != nil {
			continue
		}
		if best == nil || idx < bestIndex {
			best, bestIndex = t, idx
		}
	}
	if best != nil {
		return best
	}
	for i := range thumbs {
		if len(thumbs[i].Data) > 0 && thumbs[i].Label == "thumbnail" {
			return &thumbs[i]
		}
	}
	for i := range thumbs {
		if len(thumbs[i].Data) > 0 {
			return &thumbs[i]
		}
	}
	return nil
}

func parsePlateIndex(label string) (int, error) {
	var idx int
	_, err := fmt.Sscanf(strings.TrimPrefix(label, "plate_"), "%d", &idx)
	return idx, err
}

// --- list_gcode_files ---

const (
	sortByModified = "modified"
	sortByName     = "name"
	sortBySize     = "size"
)

type listGCodeFilesInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Page    *int    `json:"page,omitempty" jsonschema:"1-indexed page number; defaults to 1"`
	SortBy  *string `json:"sort_by,omitempty" jsonschema:"sort order: modified (default, newest first), name (A-Z), or size (largest first)"`
}

type gcodeFileRow struct {
	Filename   string
	SizeBytes  int64
	ModifiedAt float64
	Printing   bool
}

func sortGCodeFileRows(rows []gcodeFileRow, sortBy string) {
	switch sortBy {
	case sortByName:
		sort.Slice(rows, func(i, k int) bool { return rows[i].Filename < rows[k].Filename })
	case sortBySize:
		sort.Slice(rows, func(i, k int) bool { return rows[i].SizeBytes > rows[k].SizeBytes })
	default: // sortByModified: newest first
		sort.Slice(rows, func(i, k int) bool { return rows[i].ModifiedAt > rows[k].ModifiedAt })
	}
}

func renderGCodeFileRows(files []gcodeFileRow) (string, error) {
	if len(files) == 0 {
		return "(no gcode files)\n", nil
	}
	var b strings.Builder
	b.WriteString("| filename | size_bytes | modified_at | printing |\n|---|---|---|---|\n")
	for _, f := range files {
		printing := ""
		if f.Printing {
			printing = "yes"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s |\n", escapeTableCell(f.Filename), f.SizeBytes, formatUnixTime(f.ModifiedAt), printing)
	}
	return b.String(), nil
}

type listGCodeFilesFront struct {
	printerstate.StateBlock `yaml:",inline"`
	render.PageMeta         `yaml:",inline"`
	SortBy                  string `yaml:"sort_by"`
	DiskTotalBytes          int64  `yaml:"disk_total_bytes,omitempty"`
	DiskUsedBytes           int64  `yaml:"disk_used_bytes,omitempty"`
	DiskFreeBytes           int64  `yaml:"disk_free_bytes,omitempty"`
	CurrentPrintFile        string `yaml:"current_print_file,omitempty"`
}

func (f *listGCodeFilesFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func listGCodeFilesHandler(s *Server) func(context.Context, *mcp.CallToolRequest, listGCodeFilesInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in listGCodeFilesInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}

		sortBy := sortByModified
		if in.SortBy != nil {
			sortBy = strings.ToLower(strings.TrimSpace(*in.SortBy))
		}
		if sortBy != sortByModified && sortBy != sortByName && sortBy != sortBySize {
			return render.ErrorResult(render.Error{
				Code:    render.CodeInvalidInput,
				Message: fmt.Sprintf("unknown sort_by %q", sortBy),
				Hint:    fmt.Sprintf("Use one of %q, %q, %q.", sortByModified, sortByName, sortBySize),
			}), nil, nil
		}

		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		fc, ok := deps.Moonraker.(filesClient)
		if !ok {
			return failure("list gcode files", errors.New("this printer's client does not support the file manager endpoints"), ""), nil, nil
		}
		dir, err := fc.Directory(ctx, "gcodes")
		if err != nil {
			return failure("list gcode files", err, ""), nil, nil
		}

		currentFile := ""
		if snap.PrintStats != nil {
			currentFile = snap.PrintStats.Filename
		}

		rows := make([]gcodeFileRow, len(dir.Files))
		for i, f := range dir.Files {
			rows[i] = gcodeFileRow{
				Filename:   f.Filename,
				SizeBytes:  f.Size,
				ModifiedAt: f.Modified,
				Printing:   currentFile != "" && f.Filename == currentFile,
			}
		}
		sortGCodeFileRows(rows, sortBy)

		page := 1
		if in.Page != nil {
			page = *in.Page
		}
		window, meta, nextHint, err := paginatePage(rows, page, renderGCodeFileRows)
		if err != nil {
			return failure("paginate gcode files", err, ""), nil, nil
		}
		tableRows, err := renderGCodeFileRows(window)
		if err != nil {
			return failure("render gcode files", err, ""), nil, nil
		}

		front := &listGCodeFilesFront{
			StateBlock:       block,
			PageMeta:         meta,
			SortBy:           sortBy,
			DiskTotalBytes:   dir.DiskUsage.Total,
			DiskUsedBytes:    dir.DiskUsage.Used,
			DiskFreeBytes:    dir.DiskUsage.Free,
			CurrentPrintFile: currentFile,
		}
		body := tableRows + nextHint
		if len(rows) == 0 {
			body = "No gcode files are on this printer's gcodes root."
		}
		return successResult(front, nil, body), nil, nil
	}
}

// --- get_gcode_file ---

type getGCodeFileInput struct {
	Printer  *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Filename string  `json:"filename" jsonschema:"file name in the printer's gcodes root"`
}

type gcodeFileFront struct {
	printerstate.StateBlock `yaml:",inline"`
	Filename                string `yaml:"filename"`
	SizeBytes               int64  `yaml:"size_bytes"`
	ModifiedAt              string `yaml:"modified_at,omitempty"`
	Printing                bool   `yaml:"printing,omitempty"`
	Slicer                  string `yaml:"slicer,omitempty"`
	SlicerVersion           string `yaml:"slicer_version,omitempty"`
	HeaderBytesRead         int    `yaml:"header_bytes_read"`
	gcodeinfo.GCodeInfo     `yaml:",inline"`
}

func (f *gcodeFileFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func getGCodeFileBody(front *gcodeFileFront) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d bytes, dialect %s", front.Filename, front.SizeBytes, front.Dialect)
	if front.Generator != "" {
		fmt.Fprintf(&b, ", generated by %s", strings.TrimSpace(front.Generator+" "+front.GeneratorVersion))
	}
	if front.TotalLayers != nil {
		fmt.Fprintf(&b, ", %d layers", *front.TotalLayers)
	}
	b.WriteString(".")
	if front.Printing {
		b.WriteString(" This is the file currently printing.")
	}
	if len(front.Warnings) > 0 {
		fmt.Fprintf(&b, " Warnings: %s.", strings.Join(front.Warnings, "; "))
	}
	if len(front.Thumbnails) == 0 {
		b.WriteString(" No embedded thumbnail was found.")
	}
	return b.String()
}

func getGCodeFileHandler(s *Server) func(context.Context, *mcp.CallToolRequest, getGCodeFileInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in getGCodeFileInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		if strings.TrimSpace(in.Filename) == "" {
			return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: "filename must not be empty"}), nil, nil
		}

		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		fc, ok := deps.Moonraker.(filesClient)
		if !ok {
			return failure("get gcode file", errors.New("this printer's client does not support the file manager endpoints"), ""), nil, nil
		}
		md, err := fc.Metadata(ctx, in.Filename)
		if err != nil {
			return failure(fmt.Sprintf("get metadata for %s", in.Filename), err, ""), nil, nil
		}
		head, err := fc.HeadRange(ctx, in.Filename, gcodeinfo.DefaultMaxHeadBytes)
		if err != nil {
			return failure(fmt.Sprintf("read the header of %s", in.Filename), err, ""), nil, nil
		}
		// When the whole file already fit inside the head read, that read
		// already covers the tail too (a smaller settings/estimate block at
		// the end of a small file), so no second network round trip is
		// needed. A larger file gets a separate bounded tail read, the same
		// bounded head+tail approach InspectLocalGCode uses locally.
		var tail []byte
		if md.Size > int64(len(head)) {
			tail, err = fc.TailRange(ctx, in.Filename, getGCodeFileTailBytes)
			if err != nil {
				return failure(fmt.Sprintf("read the tail of %s", in.Filename), err, ""), nil, nil
			}
		}
		info, thumbs, err := gcodeinfo.ParseHeader(head, tail)
		if err != nil {
			return failure(fmt.Sprintf("parse the header of %s", in.Filename), err, ""), nil, nil
		}
		if info.Dialect == gcodeinfo.DialectUnknown {
			info.Warnings = append(info.Warnings, "this file's slicer dialect could not be recognised; header fields may be incomplete")
		}

		currentFile := ""
		if snap.PrintStats != nil {
			currentFile = snap.PrintStats.Filename
		}

		front := &gcodeFileFront{
			StateBlock:      block,
			Filename:        in.Filename,
			SizeBytes:       md.Size,
			ModifiedAt:      formatUnixTime(md.Modified),
			Printing:        currentFile != "" && currentFile == in.Filename,
			Slicer:          md.Slicer,
			SlicerVersion:   md.SlicerVersion,
			HeaderBytesRead: len(head),
			GCodeInfo:       *info,
		}
		body := getGCodeFileBody(front)
		if thumb := largestThumbnail(thumbs); thumb != nil {
			return imageResult(front, nil, body, thumb.Mime, thumb.Data), nil, nil
		}
		return successResult(front, nil, body), nil, nil
	}
}

// --- inspect_local_gcode ---

type inspectLocalGCodeInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Path    string  `json:"path" jsonschema:"local gcode file path, absolute or relative to this server's own working directory"`
}

type localGCodeFront struct {
	printerstate.StateBlock  `yaml:",inline"`
	gcodeinfo.LocalGCodeFile `yaml:",inline"`
}

func (f *localGCodeFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func localGCodeBody(front *localGCodeFront) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d bytes, dialect %s.", front.Path, front.SizeBytes, front.Dialect)
	if len(front.Warnings) > 0 {
		fmt.Fprintf(&b, " Warnings: %s.", strings.Join(front.Warnings, "; "))
	}
	if len(front.Thumbnails) == 0 {
		b.WriteString(" No embedded thumbnail was found.")
	}
	b.WriteString(" Call upload_gcode_file to send this file to a printer's gcodes root.")
	return b.String()
}

func inspectLocalGCodeHandler(s *Server) func(context.Context, *mcp.CallToolRequest, inspectLocalGCodeInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in inspectLocalGCodeInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		absPath, _, errRes2 := resolveLocalFile(in.Path)
		if errRes2 != nil {
			return errRes2, nil, nil
		}

		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		result, thumbs, err := gcodeinfo.InspectLocalGCode(absPath, gcodeinfo.ReadLimits{})
		if err != nil {
			return failure("inspect local gcode file", err, ""), nil, nil
		}

		front := &localGCodeFront{StateBlock: block, LocalGCodeFile: *result}
		body := localGCodeBody(front)
		if thumb := largestThumbnail(thumbs); thumb != nil {
			return imageResult(front, nil, body, thumb.Mime, thumb.Data), nil, nil
		}
		return successResult(front, nil, body), nil, nil
	}
}

// --- inspect_local_3mf ---

type inspectLocal3MFInput struct {
	Printer      *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Path         string  `json:"path" jsonschema:"local 3MF project file path, absolute or relative to this server's own working directory"`
	FullSettings bool    `json:"full_settings,omitempty" jsonschema:"if true, return every settings key from the project (capped in size) instead of the default filtered printer/filament/process summary"`
}

type local3MFFront struct {
	printerstate.StateBlock `yaml:",inline"`
	gcodeinfo.ThreeMFInfo   `yaml:",inline"`
}

func (f *local3MFFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func local3MFBody(front *local3MFFront) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d bytes, dialect %s, %d plate(s), %d object(s).",
		front.Path, front.SizeBytes, front.Dialect, len(front.Plates), len(front.Objects))
	if front.PrinterModel != "" {
		fmt.Fprintf(&b, " Printer model: %s.", front.PrinterModel)
	}
	if front.SettingsFull {
		fmt.Fprintf(&b, " Showing all %d settings keys", front.SettingsCount)
	} else {
		fmt.Fprintf(&b, " Showing %d of %d settings keys (filtered); pass full_settings: true for the rest", len(front.Settings), front.SettingsCount)
	}
	if front.SettingsTruncated {
		b.WriteString(", truncated at the size cap")
	}
	b.WriteString(".")
	if len(front.Warnings) > 0 {
		fmt.Fprintf(&b, " Warnings: %s.", strings.Join(front.Warnings, "; "))
	}
	if len(front.Thumbnails) == 0 {
		b.WriteString(" No plate or legacy thumbnail was found.")
	}
	return b.String()
}

func inspectLocal3MFHandler(s *Server) func(context.Context, *mcp.CallToolRequest, inspectLocal3MFInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in inspectLocal3MFInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		absPath, _, errRes2 := resolveLocalFile(in.Path)
		if errRes2 != nil {
			return errRes2, nil, nil
		}

		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		result, thumbs, err := gcodeinfo.InspectLocal3MF(absPath, gcodeinfo.ThreeMFOptions{FullSettings: in.FullSettings})
		if err != nil {
			return failure("inspect local 3mf file", err, ""), nil, nil
		}

		front := &local3MFFront{StateBlock: block, ThreeMFInfo: *result}
		body := local3MFBody(front)
		if thumb := firstPlateThumbnail(thumbs); thumb != nil {
			return imageResult(front, nil, body, thumb.Mime, thumb.Data), nil, nil
		}
		return successResult(front, nil, body), nil, nil
	}
}

// validateRemoteFilename rejects a gcodes-root filename that could not
// possibly name a plain file directly under the gcodes root: empty, a path
// separator, "..", a colon (Windows drive letter, and never valid in a
// filename either way), or a control character. This is upload's filename
// override and delete's target filename - both identify a file in the
// gcodes root, which this printer's Moonraker never nests into
// subdirectories in practice (files.go's gcodesRoot doc comment). Moonraker
// also enforces this server-side (it rejects any path escaping the gcodes
// root), but checking here first avoids a pointless network round trip and
// gives a clearer invalid_input error than Moonraker's own.
// remoteFilenameMaxBytes caps a remote gcodes-root filename at 255 bytes
// (review backlog item 25): the common ext4/most-filesystems NAME_MAX, and
// well past anything a real slicer output or a person would ever type, so
// this only ever catches a pathological or adversarial name.
const remoteFilenameMaxBytes = 255

// confusableSeparators are Unicode code points that visually resemble a
// path separator but are not ASCII, so strings.ContainsAny's "/\\" check
// above never sees them (review backlog item 25): a name built from one of
// these could still read as a directory component to a person, or to some
// downstream tool that is less strict than Moonraker's own gcodes-root
// check.
var confusableSeparators = []rune{
	0xFF0F, // fullwidth solidus
	0x2215, // division slash
	0x2044, // fraction slash
	0x29F8, // big solidus
	0xFF3C, // fullwidth reverse solidus
}

func validateRemoteFilename(name string) *mcp.CallToolResult {
	if strings.TrimSpace(name) == "" {
		return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: "filename must not be empty"})
	}
	if name != strings.TrimSpace(name) {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("filename %q must not have leading or trailing whitespace", name),
		})
	}
	if len(name) > remoteFilenameMaxBytes {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("filename is %d bytes, longer than the %d-byte cap", len(name), remoteFilenameMaxBytes),
		})
	}
	if strings.ContainsAny(name, "/\\") {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("filename %q must not contain a path separator", name),
			Hint:    "Pass a plain file name; this printer's gcodes root has no subdirectories.",
		})
	}
	for _, r := range confusableSeparators {
		if strings.ContainsRune(name, r) {
			return render.ErrorResult(render.Error{
				Code:    render.CodeInvalidInput,
				Message: fmt.Sprintf("filename %q must not contain a Unicode character that visually resembles a path separator (U+%04X)", name, r),
				Hint:    "Pass a plain file name; this printer's gcodes root has no subdirectories.",
			})
		}
	}
	if strings.Contains(name, "..") {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("filename %q must not contain \"..\"", name),
		})
	}
	if strings.Contains(name, ":") {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("filename %q must not contain a colon or drive letter", name),
		})
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return render.ErrorResult(render.Error{
				Code:    render.CodeInvalidInput,
				Message: fmt.Sprintf("filename %q contains a control character", name),
			})
		}
	}
	return nil
}

// --- upload_gcode_file ---

// Upload and delete reuse the shared control-tool plumbing tools_control.go
// already built for the same proposal_token flow (executeControl,
// controlFront/controlFrontFrom, controlBody, controlFailure, tokenArg):
// controlFront's fields (action, accepted, effect, proposed, confirm_token,
// before/after) are generic across every control action, so the only thing
// specific to these two tools is which filename/local path the body names.

type uploadGCodeFileInput struct {
	Printer      *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Path         string  `json:"path" jsonschema:"local gcode file path to upload, absolute or relative to this server's own working directory"`
	Filename     *string `json:"filename,omitempty" jsonschema:"remote filename in the printer's gcodes root; defaults to the local file's base name"`
	ConfirmToken *string `json:"confirm_token,omitempty" jsonschema:"the token from a prior call to this same tool, required to confirm overwriting a file that already exists; omit for a new filename or the first call"`
}

func uploadGCodeFileHandler(s *Server) func(context.Context, *mcp.CallToolRequest, uploadGCodeFileInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in uploadGCodeFileInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		absPath, _, errRes2 := resolveLocalFile(in.Path)
		if errRes2 != nil {
			return errRes2, nil, nil
		}
		remoteName := filepath.Base(absPath)
		if in.Filename != nil && strings.TrimSpace(*in.Filename) != "" {
			remoteName = *in.Filename
			if errRes3 := validateRemoteFilename(remoteName); errRes3 != nil {
				return errRes3, nil, nil
			}
		}

		params := policy.Params{Filename: remoteName, LocalPath: absPath}
		res, err := s.executeControl(ctx, printer, policy.ActionUploadGCodeFile, params, tokenArg(in.ConfirmToken))
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		body := fmt.Sprintf("Target file: %s (local: %s). This never starts a print.\n\n%s", remoteName, absPath, controlBody(res))
		return successResult(front, nil, body), nil, nil
	}
}

// --- delete_gcode_file ---

type deleteGCodeFileInput struct {
	Printer      *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Filename     string  `json:"filename" jsonschema:"file name in the printer's gcodes root to delete"`
	ConfirmToken *string `json:"confirm_token,omitempty" jsonschema:"the token from a prior call to this same tool, required to confirm the delete; omit for the first call"`
}

func deleteGCodeFileHandler(s *Server) func(context.Context, *mcp.CallToolRequest, deleteGCodeFileInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in deleteGCodeFileInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		if errRes2 := validateRemoteFilename(in.Filename); errRes2 != nil {
			return errRes2, nil, nil
		}

		params := policy.Params{Filename: in.Filename}
		res, err := s.executeControl(ctx, printer, policy.ActionDeleteGCodeFile, params, tokenArg(in.ConfirmToken))
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		body := fmt.Sprintf("Target file: %s. This cannot be undone.\n\n%s", in.Filename, controlBody(res))
		return successResult(front, nil, body), nil, nil
	}
}
