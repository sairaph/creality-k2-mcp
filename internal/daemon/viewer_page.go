package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"text/template"
)

// printerJSON is one printer's shape in both the /api/printers response and
// the page's server-rendered PRINTERS array: id and display name only (the
// camera host is internal to the daemon and never sent to the browser).
type printerJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// streamPrinterJSON extends printerJSON with the printer's stream URL
// (already carrying the access token), embedded into the page itself
// (viewerPageData.PrintersJSON) so the page's inline JS never has to build
// that URL, and so the raw page response literally contains the stream URL.
type streamPrinterJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StreamURL string `json:"stream_url"`
}

// handlePrinters serves GET /api/printers: the JSON list of enabled
// printers the viewer may show.
func (v *viewerServer) handlePrinters(w http.ResponseWriter, r *http.Request) {
	printers, err := v.listPrintersSorted()
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]printerJSON, len(printers))
	for i, p := range printers {
		out[i] = printerJSON{ID: p.ID, Name: p.Name}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// listPrintersSorted returns PrinterLister.ListEnabled's result sorted by
// id, for a deterministic page and JSON listing. A nil PrinterLister (never
// the case in production; only an incompletely wired test) yields an empty
// list rather than a panic.
func (v *viewerServer) listPrintersSorted() ([]ViewerPrinter, error) {
	if v.printers == nil {
		return nil, nil
	}
	printers, err := v.printers.ListEnabled()
	if err != nil {
		return nil, err
	}
	sort.Slice(printers, func(i, j int) bool { return printers[i].ID < printers[j].ID })
	return printers, nil
}

// viewerPageData is what viewerPageTemplate renders.
type viewerPageData struct {
	// PrintersJSON is a JSON array of streamPrinterJSON, already
	// HTML/script-safe (encoding/json's default encoder escapes <, > and &),
	// embedded directly into the page's inline <script> as the PRINTERS
	// constant.
	PrintersJSON string
}

// handleIndex serves GET /: the self-contained MSE viewer page. With no
// "printer" query parameter it lists every enabled printer, each with its
// own video element; with one, only that printer is shown (viewer.url's
// optional printer id, daemon.go's handleViewerURL).
func (v *viewerServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	// checkToken has already confirmed this exact token matches the server's
	// own, so it is safe to reuse when building each stream URL below.
	token := r.URL.Query().Get("token")

	all, err := v.listPrintersSorted()
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	selected := all
	if want := r.URL.Query().Get("printer"); want != "" {
		selected = nil
		for _, p := range all {
			if p.ID == want {
				selected = append(selected, p)
			}
		}
	}

	videos := make([]streamPrinterJSON, len(selected))
	for i, p := range selected {
		videos[i] = streamPrinterJSON{
			ID:        p.ID,
			Name:      p.Name,
			StreamURL: fmt.Sprintf("/stream/%s.mp4?token=%s", url.PathEscape(p.ID), url.QueryEscape(token)),
		}
	}

	jsonBytes, err := json.Marshal(videos)
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := viewerPageTemplate.Execute(w, viewerPageData{PrintersJSON: string(jsonBytes)}); err != nil {
		if v.log != nil {
			v.log.Printf("viewer: render page: %v", err)
		}
	}
}

// viewerPageTemplate is the whole GET / response: a small, self-contained
// HTML page with inline CSS and JS (no external CDN, per T11b), one video
// element per printer in PrintersJSON, each decoded from its fMP4 stream via
// Media Source Extensions: fetch() the stream, read the codec string off the
// X-Video-Codec response header (never hardcoded), create a MediaSource
// SourceBuffer with it, and pump the response body's chunks into
// appendBuffer as they arrive. If a stream ends (the server closed it after
// an upstream parameter change, or a network hiccup), the page reconnects
// with a fresh MediaSource after a short delay.
var viewerPageTemplate = template.Must(template.New("viewer").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>creality_k2_mcp camera viewer</title>
<style>
  body { font-family: system-ui, sans-serif; background: #111; color: #eee; margin: 0; padding: 16px; }
  h1 { font-size: 1.2em; margin: 0 0 16px; }
  .printer { margin-bottom: 24px; max-width: 960px; }
  .printer h2 { font-size: 1em; margin: 0 0 8px; }
  .printer video { width: 100%; background: #000; display: block; }
  .printer .status { font-size: 0.85em; color: #aaa; margin-top: 4px; }
  .empty { color: #aaa; }
</style>
</head>
<body>
<h1>creality_k2_mcp camera viewer</h1>
<div id="printers"></div>
<script>
"use strict";
const PRINTERS = {{.PrintersJSON}};

// Reconnect backoff: capped exponential with jitter, starting at
// RECONNECT_BASE_MS and never exceeding RECONNECT_MAX_MS, reset back to the
// base once a stream actually plays (appends a chunk successfully), so a
// brief blip reconnects quickly but a camera that is genuinely down does not
// get hammered with requests.
const RECONNECT_BASE_MS = 1000;
const RECONNECT_MAX_MS = 30000;

// SOURCE_BUFFER_WINDOW_SECONDS is how much video behind the current play
// position is kept in the SourceBuffer; older ranges are trimmed on every
// updateend so a long-running view does not grow the buffer without bound
// (and so there is always somewhere to trim from if the browser reports
// QuotaExceededError on appendBuffer).
const SOURCE_BUFFER_WINDOW_SECONDS = 30;

function nextReconnectDelay(backoffMs) {
  const jitter = backoffMs * (0.5 + Math.random() * 0.5); // 50%-100% of backoffMs
  const grown = Math.min(backoffMs * 2, RECONNECT_MAX_MS);
  return { waitMs: jitter, nextBackoffMs: grown };
}

function trimSourceBuffer(sourceBuffer, video) {
  if (sourceBuffer.updating || sourceBuffer.buffered.length === 0) {
    return;
  }
  const start = sourceBuffer.buffered.start(0);
  const cutoff = video.currentTime - SOURCE_BUFFER_WINDOW_SECONDS;
  if (cutoff <= start) {
    return;
  }
  try {
    sourceBuffer.remove(start, cutoff);
  } catch (e) {
    // Mid-update or otherwise not removable right now; the next updateend
    // will try again.
  }
}

function startStream(printer, video, statusEl, backoffMs) {
  backoffMs = backoffMs || RECONNECT_BASE_MS;
  const mediaSource = new MediaSource();
  video.src = URL.createObjectURL(mediaSource);

  function scheduleReconnect() {
    const { waitMs, nextBackoffMs } = nextReconnectDelay(backoffMs);
    setTimeout(function () { startStream(printer, video, statusEl, nextBackoffMs); }, waitMs);
  }

  mediaSource.addEventListener("sourceopen", async function () {
    try {
      const resp = await fetch(printer.stream_url, { cache: "no-store" });
      if (!resp.ok) {
        statusEl.textContent = "stream error: HTTP " + resp.status;
        scheduleReconnect();
        return;
      }
      const codec = resp.headers.get("X-Video-Codec");
      if (!codec) {
        statusEl.textContent = "stream error: no codec reported by the server";
        scheduleReconnect();
        return;
      }
      const mime = 'video/mp4; codecs="' + codec + '"';
      if (!MediaSource.isTypeSupported(mime)) {
        statusEl.textContent = "this browser cannot decode " + mime;
        return;
      }
      const sourceBuffer = mediaSource.addSourceBuffer(mime);
      sourceBuffer.addEventListener("updateend", function () {
        trimSourceBuffer(sourceBuffer, video);
      });
      const reader = resp.body.getReader();

      function appendChunk(chunk) {
        return new Promise(function (resolve, reject) {
          function onUpdateEnd() {
            sourceBuffer.removeEventListener("updateend", onUpdateEnd);
            sourceBuffer.removeEventListener("error", onError);
            resolve();
          }
          function onError(e) {
            sourceBuffer.removeEventListener("updateend", onUpdateEnd);
            sourceBuffer.removeEventListener("error", onError);
            reject(e);
          }
          sourceBuffer.addEventListener("updateend", onUpdateEnd);
          sourceBuffer.addEventListener("error", onError);
          try {
            sourceBuffer.appendBuffer(chunk);
          } catch (e) {
            sourceBuffer.removeEventListener("updateend", onUpdateEnd);
            sourceBuffer.removeEventListener("error", onError);
            reject(e);
          }
        });
      }

      // appendChunkWithRetry handles the browser reporting the SourceBuffer
      // full (QuotaExceededError, thrown by appendBuffer itself or delivered
      // via the "error" event depending on the browser): trim everything
      // behind the current play position and retry exactly once, rather
      // than tearing down and reconnecting the whole stream over a
      // transient buffer-pressure condition.
      async function appendChunkWithRetry(chunk) {
        try {
          await appendChunk(chunk);
        } catch (e) {
          if (!(e && e.name === "QuotaExceededError")) {
            throw e;
          }
          if (sourceBuffer.buffered.length > 0) {
            const start = sourceBuffer.buffered.start(0);
            const end = Math.max(start, video.currentTime - SOURCE_BUFFER_WINDOW_SECONDS);
            if (end > start) {
              await new Promise(function (resolve, reject) {
                function onUpdateEnd() {
                  sourceBuffer.removeEventListener("updateend", onUpdateEnd);
                  resolve();
                }
                function onError(removeErr) {
                  sourceBuffer.removeEventListener("updateend", onUpdateEnd);
                  sourceBuffer.removeEventListener("error", onError);
                  reject(removeErr);
                }
                sourceBuffer.addEventListener("updateend", onUpdateEnd);
                sourceBuffer.addEventListener("error", onError);
                try {
                  sourceBuffer.remove(start, end);
                } catch (removeErr) {
                  sourceBuffer.removeEventListener("updateend", onUpdateEnd);
                  sourceBuffer.removeEventListener("error", onError);
                  reject(removeErr);
                }
              });
            }
          }
          await appendChunk(chunk); // one retry; a second failure propagates
        }
      }

      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          statusEl.textContent = "stream ended; reconnecting...";
          scheduleReconnect();
          return;
        }
        await appendChunkWithRetry(value);
        statusEl.textContent = "live";
        backoffMs = RECONNECT_BASE_MS; // the stream is playing: reset backoff
      }
    } catch (e) {
      statusEl.textContent = "stream error: " + e;
      scheduleReconnect();
    }
  });
}

const container = document.getElementById("printers");
if (PRINTERS.length === 0) {
  const p = document.createElement("p");
  p.className = "empty";
  p.textContent = "No enabled printer is available to view.";
  container.appendChild(p);
}
for (const printer of PRINTERS) {
  const div = document.createElement("div");
  div.className = "printer";
  const h2 = document.createElement("h2");
  h2.textContent = printer.name;
  const video = document.createElement("video");
  video.controls = true;
  video.autoplay = true;
  video.muted = true;
  video.playsInline = true;
  const statusEl = document.createElement("div");
  statusEl.className = "status";
  statusEl.textContent = "connecting...";
  div.appendChild(h2);
  div.appendChild(video);
  div.appendChild(statusEl);
  container.appendChild(div);
  startStream(printer, video, statusEl);
}
</script>
</body>
</html>
`))
