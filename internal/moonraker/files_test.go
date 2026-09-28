package moonraker

import (
	"context"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDecodesRealCaptureAndRequestsGcodesRoot(t *testing.T) {
	var gotRoot string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRoot = r.URL.Query().Get("root")
		w.Write(readFixture(t, "server_files_list_root_gcodes.json"))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	files, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotRoot != "gcodes" {
		t.Fatalf("root query param = %q, want %q", gotRoot, "gcodes")
	}
	if len(files) != 13 {
		t.Fatalf("List returned %d files, want 13", len(files))
	}
}

func TestMetadataDecodesRealCapture(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "server_files_metadata_base_obj.json"))
	c := New(ts.URL, "")
	md, err := c.Metadata(context.Background(), "base.obj_PLA_47m5s.gcode")
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if md.Slicer != "Unknown" {
		t.Fatalf("Slicer = %q, want %q (this fork's own scanner does not recognise Creality's header)", md.Slicer, "Unknown")
	}
	if md.UUID != "cae7dac1-c9ed-4614-8ce7-574cfe432dd4" {
		t.Fatalf("UUID = %q, want the fixture's UUID", md.UUID)
	}
}

func TestDirectoryDecodesRealCaptureExtended(t *testing.T) {
	var gotExtended, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotExtended = r.URL.Query().Get("extended")
		gotPath = r.URL.Query().Get("path")
		w.Write(readFixture(t, "server_files_directory_gcodes_extended.json"))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	dir, err := c.Directory(context.Background(), "gcodes")
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if gotExtended != "true" {
		t.Fatalf("extended query param = %q, want %q", gotExtended, "true")
	}
	if gotPath != "gcodes" {
		t.Fatalf("path query param = %q, want %q", gotPath, "gcodes")
	}
	if len(dir.Files) != 13 {
		t.Fatalf("Directory returned %d files, want 13", len(dir.Files))
	}
	if dir.DiskUsage.Total != 6516404224 {
		t.Fatalf("DiskUsage.Total = %d, want 6516404224", dir.DiskUsage.Total)
	}
	if dir.RootInfo.Name != "gcodes" {
		t.Fatalf("RootInfo.Name = %q, want %q", dir.RootInfo.Name, "gcodes")
	}
}

func TestHeadRangeHonoursPartialContent(t *testing.T) {
	full := strings.Repeat("A", 100) + strings.Repeat("B", 900)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "bytes=0-9" {
			t.Errorf("Range header = %q, want %q", rangeHeader, "bytes=0-9")
		}
		w.Header().Set("Content-Range", "bytes 0-9/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(full[:10]))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	data, err := c.HeadRange(context.Background(), "big.gcode", 10)
	if err != nil {
		t.Fatalf("HeadRange: %v", err)
	}
	if string(data) != strings.Repeat("A", 10) {
		t.Fatalf("data = %q, want 10 A's", data)
	}
}

func TestHeadRangeFallsBackWhenServerIgnoresRange(t *testing.T) {
	full := strings.Repeat("A", 10) + strings.Repeat("B", 1<<20) // 1 MiB+ of B
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore the Range header entirely and answer 200 with the whole
		// body, the way a server without Range support would.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(full))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	data, err := c.HeadRange(context.Background(), "big.gcode", 10)
	if err != nil {
		t.Fatalf("HeadRange: %v", err)
	}
	if len(data) != 10 {
		t.Fatalf("len(data) = %d, want 10 (bounded read despite the server ignoring Range)", len(data))
	}
	if string(data) != strings.Repeat("A", 10) {
		t.Fatalf("data = %q, want 10 A's", data)
	}
}

func TestHeadRangeRejectsNonPositiveN(t *testing.T) {
	c := New("http://example.invalid", "")
	if _, err := c.HeadRange(context.Background(), "f.gcode", 0); err == nil {
		t.Fatal("expected an error for n=0")
	}
}

func TestTailRangeHonoursSuffixRange(t *testing.T) {
	full := strings.Repeat("A", 990) + strings.Repeat("B", 10)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "bytes=-10" {
			t.Errorf("Range header = %q, want %q", rangeHeader, "bytes=-10")
		}
		w.Header().Set("Content-Range", "bytes 990-999/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(full[990:]))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	data, err := c.TailRange(context.Background(), "big.gcode", 10)
	if err != nil {
		t.Fatalf("TailRange: %v", err)
	}
	if string(data) != strings.Repeat("B", 10) {
		t.Fatalf("data = %q, want 10 B's", data)
	}
}

func TestTailRangeFallsBackWhenServerIgnoresRange(t *testing.T) {
	full := strings.Repeat("A", 1<<20) + strings.Repeat("B", 10) // 1 MiB+ of A, then B's
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore the Range header entirely and answer 200 with the whole
		// body, the way a server without Range support would.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(full))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	data, err := c.TailRange(context.Background(), "big.gcode", 10)
	if err != nil {
		t.Fatalf("TailRange: %v", err)
	}
	if len(data) != 10 {
		t.Fatalf("len(data) = %d, want 10 (bounded fallback still ends on the true tail)", len(data))
	}
	if string(data) != strings.Repeat("B", 10) {
		t.Fatalf("data = %q, want 10 B's", data)
	}
}

func TestTailRangeSmallFileOverlapsHead(t *testing.T) {
	// A file smaller than the requested tail length: a suffix range longer
	// than the resource is answered with the whole thing, still as 206
	// (this exercises head and tail overlapping entirely for a small file).
	full := "G28\nM104 S220\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=-1048576" {
			t.Errorf("Range header = %q, want %q", got, "bytes=-1048576")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(full)-1, len(full)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(full))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	data, err := c.TailRange(context.Background(), "small.gcode", 1<<20)
	if err != nil {
		t.Fatalf("TailRange: %v", err)
	}
	if string(data) != full {
		t.Fatalf("data = %q, want the whole small file %q", data, full)
	}
}

func TestTailRangeRejectsNonPositiveN(t *testing.T) {
	c := New("http://example.invalid", "")
	if _, err := c.TailRange(context.Background(), "f.gcode", 0); err == nil {
		t.Fatal("expected an error for n=0")
	}
}

func TestUploadSendsExpectedMultipartShapeAndNeverSetsPrint(t *testing.T) {
	var gotRoot, gotFilename, gotContent string
	var sawPrintField bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("parse content type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			switch part.FormName() {
			case "root":
				buf := make([]byte, 64)
				n, _ := part.Read(buf)
				gotRoot = string(buf[:n])
			case "print":
				sawPrintField = true
			case "file":
				gotFilename = part.FileName()
				buf := make([]byte, 4096)
				n, _ := part.Read(buf)
				gotContent = string(buf[:n])
			}
		}
		w.Write([]byte(`{"item": {"root": "gcodes", "path": "remote.gcode", "modified": 1.0, "size": 5, "permissions": "rw"}, "print_started": false, "print_queued": false, "action": "create_file"}`))
	}))
	defer ts.Close()

	tmp := t.TempDir()
	local := filepath.Join(tmp, "local.gcode")
	if err := os.WriteFile(local, []byte("G28\n;x"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	c := New(ts.URL, "")
	result, err := c.Upload(context.Background(), local, "remote.gcode")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotRoot != "gcodes" {
		t.Fatalf("root field = %q, want %q", gotRoot, "gcodes")
	}
	if gotFilename != "remote.gcode" {
		t.Fatalf("uploaded filename = %q, want %q", gotFilename, "remote.gcode")
	}
	if !strings.HasPrefix(gotContent, "G28") {
		t.Fatalf("uploaded content = %q, want it to start with G28", gotContent)
	}
	if sawPrintField {
		t.Fatal("Upload must never send a \"print\" form field")
	}
	if result.Action != "create_file" {
		t.Fatalf("Action = %q, want %q", result.Action, "create_file")
	}
	if result.PrintStarted {
		t.Fatal("PrintStarted decoded true, want false")
	}
}

func TestUploadDecodesResponseWithoutResultWrapper(t *testing.T) {
	// control_test_20260928.md: the upload reply has no "result" wrapper,
	// unlike every other write endpoint.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.Write([]byte(`{"item": {"root": "gcodes", "path": "x.gcode", "modified": 1.0, "size": 1, "permissions": "rw"}, "print_started": false, "print_queued": false, "action": "create_file"}`))
	}))
	defer ts.Close()

	tmp := t.TempDir()
	local := filepath.Join(tmp, "x.gcode")
	os.WriteFile(local, []byte("G1"), 0o644)

	c := New(ts.URL, "")
	result, err := c.Upload(context.Background(), local, "x.gcode")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if result.Item.Path != "x.gcode" {
		t.Fatalf("Item.Path = %q, want %q", result.Item.Path, "x.gcode")
	}
}

func TestUploadRejectsMissingLocalFile(t *testing.T) {
	c := New("http://example.invalid", "")
	_, err := c.Upload(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.gcode"), "x.gcode")
	if err == nil {
		t.Fatal("expected an error for a missing local file")
	}
	merr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if merr.Code != CodeInvalidInput {
		t.Fatalf("Code = %q, want %q", merr.Code, CodeInvalidInput)
	}
}

func TestDeleteSucceeds(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.Write([]byte(`{"result": {"item": {"root": "gcodes", "path": "old.gcode"}, "action": "delete_file"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	result, err := c.Delete(context.Background(), "old.gcode")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if want := "/server/files/gcodes/old.gcode"; gotPath != want {
		t.Fatalf("request path = %q, want %q", gotPath, want)
	}
	if result.Action != "delete_file" {
		t.Fatalf("Action = %q, want %q", result.Action, "delete_file")
	}
}

func TestDeleteMissingFileIsNotFound(t *testing.T) {
	// control_test_20260928.md: this is a 400 "Invalid file path", not a
	// 404, and must still classify as not_found.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": {"code": 400, "message": "Invalid file path: gcodes/missing.gcode"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	_, err := c.Delete(context.Background(), "missing.gcode")
	if err == nil {
		t.Fatal("expected an error")
	}
	merr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if merr.Code != CodeNotFound {
		t.Fatalf("Code = %q, want %q", merr.Code, CodeNotFound)
	}
}

func TestDeleteFileInUseIsConflict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error": {"code": 403, "message": "File is loaded, DELETE not permitted"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	_, err := c.Delete(context.Background(), "current.gcode")
	merr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if merr.Code != CodeConflict {
		t.Fatalf("Code = %q, want %q", merr.Code, CodeConflict)
	}
}

func TestDeleteEscapesFilenameInPath(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"result": {"item": {}, "action": "delete_file"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	if _, err := c.Delete(context.Background(), "Part 1_PLA_3h5m4s.gcode"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	decoded, err := url.PathUnescape(gotPath)
	if err != nil {
		t.Fatalf("unescape path: %v", err)
	}
	if want := "/server/files/gcodes/Part 1_PLA_3h5m4s.gcode"; decoded != want {
		t.Fatalf("decoded path = %q, want %q", decoded, want)
	}
}
