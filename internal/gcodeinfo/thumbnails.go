package gcodeinfo

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register the JPEG format with image.DecodeConfig
	_ "image/png"  // register the PNG format with image.DecodeConfig
	"regexp"
	"strconv"
	"strings"
)

// Thumbnails are UI preview images, not full renders, so a legitimate one is
// always small; these caps reject a thumbnail whose decoded header declares
// an implausible canvas (whether a genuinely oversized image or a crafted
// header lying about its size) before any caller treats the bytes as safe.
const (
	maxThumbnailDimension = 4096
	maxThumbnailPixels    = maxThumbnailDimension * maxThumbnailDimension
)

// decodeThumbnailConfig reads just the header of data, via
// image.DecodeConfig (which never decodes the full pixel grid), to confirm
// it is a recognised image format - PNG or JPEG, registered above through
// blank imports - and to read its actual declared width and height. It
// returns an error for anything else, including data that is not a
// supported image at all.
func decodeThumbnailConfig(data []byte) (width, height int, mime string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, "", err
	}
	switch format {
	case "png":
		mime = "image/png"
	case "jpeg":
		mime = "image/jpeg"
	default:
		return 0, 0, "", fmt.Errorf("unsupported image format %q", format)
	}
	return cfg.Width, cfg.Height, mime, nil
}

// thumbnailSizeExceedsCap reports whether width x height is too large for a
// legitimate slicer thumbnail, either because a side is over
// maxThumbnailDimension or because the total pixel count is over
// maxThumbnailPixels.
func thumbnailSizeExceedsCap(width, height int) bool {
	if width > maxThumbnailDimension || height > maxThumbnailDimension {
		return true
	}
	return width*height > maxThumbnailPixels
}

var (
	thumbBeginPattern = regexp.MustCompile(`(?i)thumbnail begin\s+(\d+)x(\d+)\s+(\d+)`)
	thumbEndPattern   = regexp.MustCompile(`(?i)thumbnail end`)
)

// extractThumbnails scans a THUMBNAIL_BLOCK-style comment section (one or
// more "thumbnail begin WxH size" / base64 lines / "thumbnail end" groups,
// each line commented with a leading ";") for every embedded thumbnail. It
// never returns an error: a thumbnail whose data was cut off by a bounded
// head read is reported as Truncated, with whatever prefix could still be
// decoded, rather than failing the whole parse.
func extractThumbnails(head string) ([]Thumbnail, []ThumbnailMeta, []string) {
	var thumbs []Thumbnail
	var metas []ThumbnailMeta
	var warnings []string

	lines := strings.Split(head, "\n")
	i := 0
	for i < len(lines) {
		m := thumbBeginPattern.FindStringSubmatch(lines[i])
		if m == nil {
			i++
			continue
		}
		width, _ := strconv.Atoi(m[1])
		height, _ := strconv.Atoi(m[2])
		declared, _ := strconv.Atoi(m[3])
		i++

		var b64 strings.Builder
		closed := false
		for i < len(lines) {
			if thumbEndPattern.MatchString(lines[i]) {
				closed = true
				i++
				break
			}
			if thumbBeginPattern.MatchString(lines[i]) {
				// A new block started before this one closed: the
				// previous thumbnail's data was cut short.
				break
			}
			b64.WriteString(stripCommentPrefix(lines[i]))
			i++
		}

		encoded := b64.String()
		truncated := !closed
		if declared > 0 && len(encoded) < declared {
			truncated = true
		}

		data, err := decodeBase64Lenient(encoded)
		if err != nil {
			truncated = true
			data = nil
		}

		mime := "image/png"
		rejected := false
		if len(data) > 0 {
			actualWidth, actualHeight, actualMime, cfgErr := decodeThumbnailConfig(data)
			switch {
			case cfgErr != nil:
				// Not a complete, recognisable image stream. Drop the
				// bytes rather than pass on data no caller can safely
				// treat as a valid image.
				warnings = append(warnings, fmt.Sprintf("thumbnail %dx%d could not be decoded as a valid image and was rejected: %s", width, height, cfgErr))
				data = nil
				rejected = true
			case thumbnailSizeExceedsCap(actualWidth, actualHeight):
				warnings = append(warnings, fmt.Sprintf("thumbnail %dx%d decodes to an oversized %dx%d image and was rejected", width, height, actualWidth, actualHeight))
				data = nil
				rejected = true
			default:
				mime = actualMime
				if actualWidth != width || actualHeight != height {
					warnings = append(warnings, fmt.Sprintf("thumbnail declared as %dx%d but actually decodes to %dx%d; using the actual size", width, height, actualWidth, actualHeight))
					width = actualWidth
					height = actualHeight
				}
			}
		}
		if rejected {
			truncated = true
		}

		thumbs = append(thumbs, Thumbnail{Width: width, Height: height, Mime: mime, Data: data, Truncated: truncated})
		metas = append(metas, ThumbnailMeta{Width: width, Height: height, Mime: mime, Truncated: truncated})
		// A rejection above already carries its own specific warning; only
		// add the generic one for the other kinds of truncation (missing
		// end marker, declared/actual base64 length mismatch, undecodable
		// base64).
		if truncated && !rejected {
			warnings = append(warnings, fmt.Sprintf("thumbnail %dx%d is truncated or incomplete", width, height))
		}
	}

	return thumbs, metas, warnings
}

// stripCommentPrefix removes a leading gcode comment marker (";" plus at
// most one following space) from a thumbnail data line, leaving the base64
// content untouched otherwise.
func stripCommentPrefix(line string) string {
	line = strings.TrimRight(line, "\r")
	line = strings.TrimPrefix(line, ";")
	line = strings.TrimPrefix(line, " ")
	return line
}

// decodeBase64Lenient decodes s as standard base64, and if that fails
// (typically because s was cut mid-stream by a bounded read and is missing
// its final padding), retries against the largest 4-byte-aligned prefix of
// s. It only returns an error when no prefix at all could be decoded.
func decodeBase64Lenient(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no thumbnail data")
	}
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	for n := (len(s) / 4) * 4; n > 0; n -= 4 {
		if data, err := base64.StdEncoding.DecodeString(s[:n]); err == nil {
			return data, nil
		}
	}
	return nil, errors.New("could not decode any thumbnail data")
}
