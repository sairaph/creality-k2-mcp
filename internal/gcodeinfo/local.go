package gcodeinfo

import (
	"fmt"
	"io"
	"os"
)

// DefaultMaxHeadBytes and DefaultMaxTailBytes bound how much of a local
// gcode file InspectLocalGCode reads, matching the same bounded-Range
// approach get_gcode_file uses against a printer over Moonraker
// (plan-v0.1.0.md decision 7): this package never reads a whole multi-tens-
// of-megabytes gcode file into memory just to report its header.
const (
	DefaultMaxHeadBytes int64 = 1 << 20 // 1 MiB
	DefaultMaxTailBytes int64 = 1 << 20 // 1 MiB
)

// ReadLimits bounds how much of a local file InspectLocalGCode reads. A
// zero value on either field falls back to that field's Default*Bytes
// constant.
type ReadLimits struct {
	MaxHeadBytes int64
	MaxTailBytes int64
}

func (r ReadLimits) headBytes() int64 {
	if r.MaxHeadBytes > 0 {
		return r.MaxHeadBytes
	}
	return DefaultMaxHeadBytes
}

func (r ReadLimits) tailBytes() int64 {
	if r.MaxTailBytes > 0 {
		return r.MaxTailBytes
	}
	return DefaultMaxTailBytes
}

// InspectLocalGCode reads a bounded head and tail of a local gcode file and
// parses them with ParseHeader. It never reads more than limits' head and
// tail bounds regardless of the file's actual size, and never touches the
// network. Thumbnail image bytes are returned separately from the
// yaml-safe LocalGCodeFile result.
func InspectLocalGCode(path string, limits ReadLimits) (*LocalGCodeFile, []Thumbnail, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("gcodeinfo: open %s: %w", path, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("gcodeinfo: stat %s: %w", path, err)
	}
	if stat.IsDir() {
		return nil, nil, fmt.Errorf("gcodeinfo: %s is a directory, not a file", path)
	}
	size := stat.Size()

	headWant := limits.headBytes()
	tailWant := limits.tailBytes()

	headEnd := size
	if headWant < headEnd {
		headEnd = headWant
	}
	head := make([]byte, headEnd)
	if headEnd > 0 {
		if _, err := io.ReadFull(f, head); err != nil {
			return nil, nil, fmt.Errorf("gcodeinfo: read head of %s: %w", path, err)
		}
	}

	tailStart := size - tailWant
	if tailStart < headEnd {
		// The tail window overlaps (or is entirely inside) what head
		// already covers; the whole file already fits in head, so there is
		// nothing distinct left to read as a tail.
		tailStart = size
	}
	var tail []byte
	if tailStart < size {
		tail = make([]byte, size-tailStart)
		if _, err := f.Seek(tailStart, io.SeekStart); err != nil {
			return nil, nil, fmt.Errorf("gcodeinfo: seek tail of %s: %w", path, err)
		}
		if _, err := io.ReadFull(f, tail); err != nil {
			return nil, nil, fmt.Errorf("gcodeinfo: read tail of %s: %w", path, err)
		}
	}

	info, thumbs, err := ParseHeader(head, tail)
	if err != nil {
		return nil, nil, err
	}

	result := &LocalGCodeFile{
		Path:      path,
		SizeBytes: size,
		GCodeInfo: *info,
	}
	return result, thumbs, nil
}
