package moonraker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
)

// gcodesRoot is the only file_manager root this package ever reads or
// writes: uploads, deletes and listings are scoped to gcodes, never config
// (safety-architecture.md 3.2 point 3: no config-root file writes).
const gcodesRoot = "gcodes"

// GCodeFile is one entry from the flat gcodes root listing.
type GCodeFile struct {
	Path        string  `json:"path"`
	Modified    float64 `json:"modified"`
	Size        int64   `json:"size"`
	Permissions string  `json:"permissions"`
}

// List returns the flat file list of the gcodes root (server/files/list).
// This printer's gcodes root has no subdirectories in practice
// (02-moonraker-api.md section 3), and this method never asks for another
// root.
func (c *Client) List(ctx context.Context) ([]GCodeFile, error) {
	query := url.Values{"root": {gcodesRoot}}
	var out []GCodeFile
	err := c.get(ctx, "List", "/server/files/list", query, timeoutFiles, &out)
	return out, err
}

// FileMetadata is a gcode file's metadata, as reported by both
// server/files/metadata and, embedded per file, server/files/directory.
// On this fork Moonraker's own slicer-format scanner virtually never
// recognises Creality's gcode header, so Slicer is almost always "Unknown"
// and EstimatedTime/LayerHeight/thumbnails are absent
// (02-moonraker-api.md section 3): prefer
// VirtualSDCard.CurPrintData.Metadata (CrealityPrintMetadata) for a file
// that has actually been printed, and fall back to this for everything
// else.
type FileMetadata struct {
	Filename         string   `json:"filename"`
	Size             int64    `json:"size"`
	Modified         float64  `json:"modified"`
	UUID             string   `json:"uuid"`
	Slicer           string   `json:"slicer"`
	SlicerVersion    string   `json:"slicer_version"`
	GCodeStartByte   int64    `json:"gcode_start_byte"`
	GCodeEndByte     int64    `json:"gcode_end_byte"`
	ObjectHeight     float64  `json:"object_height"`
	FirstLayerHeight float64  `json:"first_layer_height"`
	LayerHeight      *float64 `json:"layer_height"`
	EstimatedTime    *float64 `json:"estimated_time"`
	PrintStartTime   *float64 `json:"print_start_time"`
	JobID            *string  `json:"job_id"`
	Permissions      string   `json:"permissions"`
}

// Metadata fetches one gcode file's metadata by name, relative to the
// gcodes root.
func (c *Client) Metadata(ctx context.Context, filename string) (FileMetadata, error) {
	query := url.Values{"filename": {filename}}
	var out FileMetadata
	err := c.get(ctx, "Metadata", "/server/files/metadata", query, timeoutFiles, &out)
	return out, err
}

// DiskUsage is the gcodes root's filesystem usage, embedded in an extended
// directory listing.
type DiskUsage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
}

// RootInfo names the root a directory listing describes.
type RootInfo struct {
	Name        string `json:"name"`
	Permissions string `json:"permissions"`
}

// DirEntry is one subdirectory in a directory listing.
type DirEntry struct {
	DirName     string  `json:"dirname"`
	Modified    float64 `json:"modified"`
	Size        int64   `json:"size"`
	Permissions string  `json:"permissions"`
}

// Directory is an extended gcodes-root directory listing: subdirectories,
// files with embedded metadata, and disk usage.
type Directory struct {
	Dirs      []DirEntry     `json:"dirs"`
	Files     []FileMetadata `json:"files"`
	DiskUsage DiskUsage      `json:"disk_usage"`
	RootInfo  RootInfo       `json:"root_info"`
}

// Directory fetches an extended listing (metadata embedded per file) of a
// path under the gcodes root. path is passed through as Moonraker expects
// it: "gcodes" for the root itself, or "gcodes/<subdir>".
func (c *Client) Directory(ctx context.Context, path string) (Directory, error) {
	query := url.Values{"path": {path}, "extended": {"true"}}
	var out Directory
	err := c.get(ctx, "Directory", "/server/files/directory", query, timeoutFiles, &out)
	return out, err
}

// HeadRange reads the first n bytes of a gcodes-root file using an HTTP
// Range request, which this printer honours with a 206 response
// (02-moonraker-api.md section 3, F14 "verified live"). If the server
// answers 200 instead (ignoring Range), the read is still bounded to n
// bytes and the connection is closed rather than downloading the whole
// file, per plan-v0.1.0.md decision 7's documented fallback.
func (c *Client) HeadRange(ctx context.Context, filename string, n int64) ([]byte, error) {
	const op = "HeadRange"
	if n <= 0 {
		return nil, &Error{Op: op, Code: CodeInvalidInput, Body: "n must be positive"}
	}

	path := "/server/files/" + gcodesRoot + "/" + url.PathEscape(filename)
	ctx, cancel := context.WithTimeout(ctx, timeoutFiles)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, &Error{Op: op, Code: CodeInternal, Body: "build request: " + err.Error()}
	}
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Code: CodeUnavailable, Body: err.Error()}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent, http.StatusOK:
		// Either the server honoured Range (206, body is already at most n
		// bytes) or it ignored it and is sending the whole file (200), in
		// which case the read below is still capped at n bytes and closing
		// the response body (via the deferred Close above) after that stops
		// the transfer rather than pulling the rest of a large file over
		// the network.
		data, err := io.ReadAll(io.LimitReader(resp.Body, n))
		if err != nil {
			return nil, &Error{Op: op, Status: resp.StatusCode, Code: CodeInternal, Body: "read response: " + err.Error()}
		}
		return data, nil
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, &Error{Op: op, Status: resp.StatusCode, Code: classify(resp.StatusCode, data), Body: errorMessage(data)}
	}
}

// TailRange reads the last n bytes of a gcodes-root file using an HTTP
// suffix Range request ("Range: bytes=-N"), which this printer's Moonraker
// honours with a 206 response: its file handler inherits Tornado's
// StaticFileHandler suffix-range support, the same basis HeadRange already
// relies on (02-moonraker-api.md section 3). If the server answers 200
// instead (ignoring Range and sending the whole file from the start), the
// read is still bounded - to maxResponseBytes rather than n, since the
// whole file has to be read before the tail is known - and only the last n
// bytes of what was read are kept, per plan-v0.1.0.md decision 7's
// documented fallback; a file bigger than maxResponseBytes will not have
// its true tail recovered this way, which this printer's gcodes root never
// approaches in practice (see uploadTimeout's own size note).
func (c *Client) TailRange(ctx context.Context, filename string, n int64) ([]byte, error) {
	const op = "TailRange"
	if n <= 0 {
		return nil, &Error{Op: op, Code: CodeInvalidInput, Body: "n must be positive"}
	}

	path := "/server/files/" + gcodesRoot + "/" + url.PathEscape(filename)
	ctx, cancel := context.WithTimeout(ctx, timeoutFiles)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, &Error{Op: op, Code: CodeInternal, Body: "build request: " + err.Error()}
	}
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=-%d", n))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Code: CodeUnavailable, Body: err.Error()}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// The server honoured the suffix range: the body is already the
		// last n bytes of the file (or the whole file, if it is smaller
		// than n - a suffix range longer than the resource is answered with
		// the whole thing, still as 206). Bound the read as a backstop
		// against a misbehaving server.
		data, err := io.ReadAll(io.LimitReader(resp.Body, n))
		if err != nil {
			return nil, &Error{Op: op, Status: resp.StatusCode, Code: CodeInternal, Body: "read response: " + err.Error()}
		}
		return data, nil
	case http.StatusOK:
		// The server ignored Range and is sending the whole file from the
		// start: read at most maxResponseBytes rather than downloading an
		// arbitrarily large file, then keep only the trailing n bytes of
		// whatever was actually read.
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err != nil {
			return nil, &Error{Op: op, Status: resp.StatusCode, Code: CodeInternal, Body: "read response: " + err.Error()}
		}
		if int64(len(data)) > n {
			data = data[int64(len(data))-n:]
		}
		return data, nil
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return nil, &Error{Op: op, Status: resp.StatusCode, Code: classify(resp.StatusCode, data), Body: errorMessage(data)}
	}
}

// UploadResult is the response to a gcodes upload, which this fork sends
// without the usual "result" wrapper (control_test_20260928.md), unlike
// every other write endpoint in this package.
type UploadResult struct {
	Item struct {
		Root        string  `json:"root"`
		Path        string  `json:"path"`
		Modified    float64 `json:"modified"`
		Size        int64   `json:"size"`
		Permissions string  `json:"permissions"`
	} `json:"item"`
	PrintStarted bool   `json:"print_started"`
	PrintQueued  bool   `json:"print_queued"`
	Action       string `json:"action"`
}

// Upload sends a local file to the printer's gcodes root under remoteName.
// It never asks Moonraker to start the print: the print=true form field is
// never sent, matching plan-v0.1.0.md decision 7 ("Upload... never
// print=true"). Starting a print is always a separate, explicit
// PrintStart call.
func (c *Client) Upload(ctx context.Context, localPath, remoteName string) (UploadResult, error) {
	const op = "Upload"
	f, err := os.Open(localPath)
	if err != nil {
		return UploadResult{}, &Error{Op: op, Code: CodeInvalidInput, Body: err.Error()}
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return UploadResult{}, &Error{Op: op, Code: CodeInvalidInput, Body: err.Error()}
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go func() {
		defer f.Close()
		if err := mw.WriteField("root", gcodesRoot); err != nil {
			pw.CloseWithError(err)
			return
		}
		fw, err := mw.CreateFormFile("file", remoteName)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(fw, f); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(mw.Close())
	}()

	status, data, err := c.do(ctx, op, http.MethodPost, "/server/files/upload", nil, pr, contentType, uploadTimeout(stat.Size()))
	if err != nil {
		return UploadResult{}, err
	}
	if status < 200 || status >= 300 {
		return UploadResult{}, &Error{Op: op, Status: status, Code: classify(status, data), Body: errorMessage(data)}
	}
	var out UploadResult
	if err := json.Unmarshal(data, &out); err != nil {
		return UploadResult{}, &Error{Op: op, Status: status, Code: CodeInternal, Body: "decode response: " + err.Error()}
	}
	return out, nil
}

// DeleteResult is the response to deleting a gcodes-root file.
type DeleteResult struct {
	Item struct {
		Root string `json:"root"`
		Path string `json:"path"`
	} `json:"item"`
	Action string `json:"action"`
}

// Delete removes one file from the gcodes root. Deleting a file that does
// not exist returns a not_found error: Moonraker answers that case with
// HTTP 400 "Invalid file path: gcodes/<name>" rather than a 404
// (control_test_20260928.md), which classify() maps to CodeNotFound.
func (c *Client) Delete(ctx context.Context, filename string) (DeleteResult, error) {
	path := "/server/files/" + gcodesRoot + "/" + url.PathEscape(filename)
	var out DeleteResult
	err := c.deleteEnvelope(ctx, "Delete", path, timeoutFiles, &out)
	return out, err
}
