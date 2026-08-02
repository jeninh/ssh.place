package canvas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// snapshotVersion is the version this build writes. Version 1 predates solid
// blocks and is still readable: it simply has no fills.
const (
	snapshotVersion = 2
	minReadVersion  = 1
)

const hexDigits = "0123456789abcdef"

// noFillMarker stands in for NoFill in a fills row. It is deliberately not a hex
// digit so it cannot be confused with a palette index.
const noFillMarker = '-'

// snapshotFile is the JSON shape written to disk. Storing each row as a string
// keeps a 200x60 canvas around 25 KB and leaves the file readable by eye,
// which matters when debugging a restored canvas.
type snapshotFile struct {
	Version int      `json:"version"`
	Width   int      `json:"width"`
	Height  int      `json:"height"`
	Runes   []string `json:"runes"`
	Colors  []string `json:"colors"`
	// Fills is absent in version 1 snapshots.
	Fills []string `json:"fills,omitempty"`
}

// MarshalSnapshot encodes the canvas as JSON.
func (c *Canvas) MarshalSnapshot() ([]byte, error) {
	c.mu.RLock()
	f := snapshotFile{
		Version: snapshotVersion,
		Width:   c.w,
		Height:  c.h,
		Runes:   make([]string, c.h),
		Colors:  make([]string, c.h),
		Fills:   make([]string, c.h),
	}
	runes := make([]byte, c.w)
	cols := make([]byte, c.w)
	fills := make([]byte, c.w)
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			cell := c.cells[y*c.w+x]
			if !ValidRune(cell.Rune) {
				cell.Rune = Empty
			}
			runes[x] = byte(cell.Rune)
			cols[x] = hexDigits[cell.Color&0x0f]
			if cell.IsBlock() {
				fills[x] = hexDigits[cell.Fill&0x0f]
			} else {
				fills[x] = noFillMarker
			}
		}
		f.Runes[y] = string(runes)
		f.Colors[y] = string(cols)
		f.Fills[y] = string(fills)
	}
	c.mu.RUnlock()
	return json.Marshal(f)
}

// UnmarshalSnapshot replaces the canvas contents with data. A snapshot whose
// dimensions differ from the canvas is loaded into the overlapping top-left
// region rather than rejected, so the canvas can be resized between restarts
// without losing the artwork.
func (c *Canvas) UnmarshalSnapshot(data []byte) error {
	var f snapshotFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if f.Version < minReadVersion || f.Version > snapshotVersion {
		return fmt.Errorf("unsupported snapshot version %d", f.Version)
	}
	if f.Width <= 0 || f.Height <= 0 {
		return errors.New("snapshot has non-positive dimensions")
	}
	if len(f.Runes) < f.Height || len(f.Colors) < f.Height {
		return errors.New("snapshot row count does not match height")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.cells {
		c.cells[i] = blank()
	}
	rows := min(f.Height, c.h)
	for y := 0; y < rows; y++ {
		runeRow := f.Runes[y]
		colorRow := f.Colors[y]
		var fillRow string
		if y < len(f.Fills) {
			fillRow = f.Fills[y]
		}
		cols := min(min(f.Width, c.w), len(runeRow))
		for x := 0; x < cols; x++ {
			r := rune(runeRow[x])
			if !ValidRune(r) {
				r = Empty
			}
			col := DefaultColor
			if x < len(colorRow) {
				if v, ok := unhex(colorRow[x]); ok {
					col = v
				}
			}
			fill := NoFill
			if x < len(fillRow) {
				if v, ok := unhex(fillRow[x]); ok {
					fill = v
				}
			}
			c.cells[y*c.w+x] = Cell{Rune: r, Color: col, Fill: fill}
		}
	}
	c.version.Add(1)
	return nil
}

func unhex(b byte) (uint8, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

// Load reads a snapshot from path into a new w by h canvas. A missing file is
// not an error: the canvas comes back empty, which is exactly what a first
// boot needs.
func Load(path string, w, h int) (*Canvas, error) {
	c := New(w, h)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("read snapshot %s: %w", path, err)
	}
	if err := c.UnmarshalSnapshot(data); err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", path, err)
	}
	return c, nil
}

// Save atomically writes a snapshot to path. Writing to a sibling temp file
// and renaming means a crash mid-save leaves the previous snapshot intact
// rather than a truncated file.
func (c *Canvas) Save(path string) error {
	data, err := c.MarshalSnapshot()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create snapshot dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp snapshot: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp snapshot: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp snapshot: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return nil
}

// Text renders the canvas as plain rows of text, one string per row.
//
// Solid blocks have no character of their own, so they come out as U+2588. That
// is a rendering choice for this view only; the canvas itself stores no
// non-ASCII runes.
func (c *Canvas) Text() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, c.h)
	row := make([]rune, c.w)
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			cell := c.cells[y*c.w+x]
			switch {
			case cell.IsBlock():
				row[x] = '\u2588'
			case ValidRune(cell.Rune):
				row[x] = cell.Rune
			default:
				row[x] = Empty
			}
		}
		out[y] = string(row)
	}
	return out
}
