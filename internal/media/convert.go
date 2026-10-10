package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Format is a conversion target.
type Format string

const (
	FormatOriginal Format = "original"
	FormatJPEG     Format = "jpeg"
	FormatM4A      Format = "m4a"
)

// ErrNotConvertible means the source can't be converted to the format.
var ErrNotConvertible = errors.New("media: cannot convert this attachment to the requested format")

// CanConvert reports whether a file of mimeType can be converted to f.
func CanConvert(mimeType, uti string, f Format) bool {
	switch f {
	case FormatOriginal:
		return true
	case FormatJPEG:
		return strings.HasPrefix(mimeType, "image/") || uti == "public.heic" || uti == "public.heif"
	case FormatM4A:
		return strings.HasPrefix(mimeType, "audio/") || uti == "com.apple.coreaudio-format"
	}
	return false
}

// ContentType returns the MIME type of converted output.
func (f Format) ContentType() string {
	switch f {
	case FormatJPEG:
		return "image/jpeg"
	case FormatM4A:
		return "audio/mp4"
	}
	return ""
}

func (f Format) ext() string {
	switch f {
	case FormatJPEG:
		return ".jpg"
	case FormatM4A:
		return ".m4a"
	}
	return ""
}

// cacheVersion is part of every cache key; bump it when conversion output
// changes so stale entries are ignored (and later pruned).
// v2: JPEG output is checked against the source orientation (fixRotation).
const cacheVersion = "v2"

// Converter produces converted copies of attachments in a size-limited
// cache directory. Identical concurrent requests share one conversion.
type Converter struct {
	Dir      string
	MaxBytes int64

	mu       sync.Mutex
	inflight map[string]*call
	sem      chan struct{} // bounds concurrent sips/afconvert processes
	written  atomic.Int64  // bytes written since the last prune
	pruning  sync.Mutex
}

// MaxPixels rejects images larger than this before handing them to sips, so
// a "decompression bomb" from a stranger can't exhaust memory.
const MaxPixels = 100_000_000

type call struct {
	done chan struct{}
	path string
	err  error
}

func NewConverter(dir string, maxBytes int64) *Converter {
	return &Converter{Dir: dir, MaxBytes: maxBytes, inflight: map[string]*call{},
		sem: make(chan struct{}, max(2, runtime.NumCPU()/2))}
}

// Convert returns the path of src converted to f, resized so its longest edge
// is at most maxSize when maxSize > 0 (images only). Results are cached by
// source identity, size and modification time.
func (c *Converter) Convert(ctx context.Context, src string, f Format, maxSize int) (string, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d|%s|%d", cacheVersion, src, fi.Size(), fi.ModTime().UnixNano(), f, maxSize)))
	key := hex.EncodeToString(sum[:16])
	out := filepath.Join(c.Dir, key[:2], key+f.ext())

	if _, err := os.Stat(out); err == nil {
		now := time.Now()
		os.Chtimes(out, now, now) // LRU: mark as recently used
		return out, nil
	}

	c.mu.Lock()
	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-cl.done:
			return cl.path, cl.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[key] = cl
	c.mu.Unlock()

	// The conversion is not tied to one request; a cancelled client should
	// not waste the work for the next one.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	select {
	case c.sem <- struct{}{}:
		cl.path, cl.err = c.convert(cctx, src, out, f, maxSize)
		<-c.sem
	case <-cctx.Done():
		cl.err = cctx.Err()
	}
	cancel()
	if cl.err == nil {
		if fi, err := os.Stat(cl.path); err == nil && c.written.Add(fi.Size()) > c.MaxBytes/10 {
			go c.pruneOnce() // keep the cache near its budget between hourly prunes
		}
	}
	close(cl.done)

	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
	return cl.path, cl.err
}

func (c *Converter) convert(ctx context.Context, src, out string, f Format, maxSize int) (string, error) {
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", err
	}
	tmp := out + ".tmp" + f.ext()
	defer os.Remove(tmp)
	var cmd *exec.Cmd
	switch f {
	case FormatJPEG:
		if w, h, err := Dimensions(src); err == nil && int64(w)*int64(h) > MaxPixels {
			return "", fmt.Errorf("%w: image is %dx%d (over %d megapixels)", ErrNotConvertible, w, h, MaxPixels/1_000_000)
		}
		args := []string{"-s", "format", "jpeg", "-s", "formatOptions", "85"}
		if maxSize > 0 {
			if w, h, err := Dimensions(src); err != nil || max(w, h) > maxSize {
				args = append(args, "-Z", fmt.Sprint(maxSize)) // downscale only
			}
		}
		cmd = exec.CommandContext(ctx, "/usr/bin/sips", append(args, src, "--out", tmp)...)
	case FormatM4A:
		cmd = exec.CommandContext(ctx, "/usr/bin/afconvert", "-f", "m4af", "-d", "aac", src, tmp)
	default:
		return "", ErrNotConvertible
	}
	if outb, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s failed: %v: %s", filepath.Base(cmd.Path), err, strings.TrimSpace(string(outb)))
	}
	// sips can exit 0 without producing output for unreadable input.
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("%w (conversion produced no output)", ErrNotConvertible)
	}
	if f == FormatJPEG {
		if err := fixRotation(ctx, src, tmp); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return out, nil
}

func (c *Converter) pruneOnce() {
	if !c.pruning.TryLock() {
		return
	}
	defer c.pruning.Unlock()
	c.Prune()
}

// Prune deletes least-recently-used cache files until the cache fits MaxBytes.
func (c *Converter) Prune() {
	c.written.Store(0)
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var entries []entry
	var total int64
	filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			entries = append(entries, entry{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if total <= c.MaxBytes {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
	removed := 0
	for _, e := range entries {
		if total <= c.MaxBytes {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
			removed++
		}
	}
	slog.Info("pruned conversion cache", "files", removed, "bytes_now", total)
}

// RunPruner prunes the cache hourly until ctx is done.
func (c *Converter) RunPruner(ctx context.Context) {
	c.Prune()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Prune()
		}
	}
}

// LivePhotoVideo returns the Live Photo companion video for an image
// attachment: a .mov with the same name stem, or else the only .mov in the
// attachment's own directory (Messages stores each attachment in its own
// directory). Returns "" if there is none.
func LivePhotoVideo(imagePath string) string {
	dir := filepath.Dir(imagePath)
	stem := strings.TrimSuffix(filepath.Base(imagePath), filepath.Ext(imagePath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var movs []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".mov") {
			continue
		}
		if strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())) == stem {
			return filepath.Join(dir, e.Name())
		}
		movs = append(movs, e.Name())
	}
	if len(movs) == 1 {
		return filepath.Join(dir, movs[0])
	}
	return ""
}

// DimensionCache memoizes Dimensions by path, size and mtime.
type DimensionCache struct {
	mu    sync.Mutex
	items map[string]dimEntry
	max   int
}

type dimEntry struct {
	size int64
	mod  time.Time
	w, h int
	ok   bool
}

func NewDimensionCache(max int) *DimensionCache {
	return &DimensionCache{items: map[string]dimEntry{}, max: max}
}

// Get returns display dimensions for an image file (ok=false if unknown).
func (c *DimensionCache) Get(path string, fi os.FileInfo) (w, h int, ok bool) {
	c.mu.Lock()
	e, hit := c.items[path]
	c.mu.Unlock()
	if hit && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		return e.w, e.h, e.ok
	}
	w, h, err := Dimensions(path)
	e = dimEntry{size: fi.Size(), mod: fi.ModTime(), w: w, h: h, ok: err == nil}
	c.mu.Lock()
	if len(c.items) >= c.max {
		c.items = map[string]dimEntry{} // simple reset; entries are cheap to recompute
	}
	c.items[path] = e
	c.mu.Unlock()
	return e.w, e.h, e.ok
}

// fixRotation makes sure the converted image displays like the source. On
// macOS 26, sips carries HEIF irot into the JPEG as an EXIF orientation tag
// (verified with ImageIO; note `sips -g orientation` does not report it), so
// this is normally a no-op. It is a safety net for sources or sips versions
// that drop the rotation: if the output has no orientation but the source
// does, the output's pixels are rotated by the difference.
func fixRotation(ctx context.Context, src, out string) error {
	want, err := Rotation(src)
	if err != nil {
		return nil // unknown source orientation: leave as converted
	}
	have, err := Rotation(out)
	if err != nil {
		return nil
	}
	delta := (want - have + 360) % 360
	if delta == 0 {
		return nil
	}
	if have != 0 {
		slog.Warn("converted image has an unexpected orientation tag; not rotating", "src", src, "want", want, "have", have)
		return nil
	}
	if outb, err := exec.CommandContext(ctx, "/usr/bin/sips", "-r", fmt.Sprint(delta), out).CombinedOutput(); err != nil {
		return fmt.Errorf("sips rotate failed: %v: %s", err, strings.TrimSpace(string(outb)))
	}
	return nil
}
