package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pngBytes(w, h int) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)))
	return buf.Bytes()
}

// jpegWithOrientation encodes a JPEG and inserts an EXIF APP1 segment.
func jpegWithOrientation(w, h, orientation int) []byte {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	jpeg.Encode(&buf, img, nil)
	j := buf.Bytes()

	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08") // big-endian, IFD at 8
	ifd := make([]byte, 2+12+4)
	binary.BigEndian.PutUint16(ifd[0:], 1)      // one entry
	binary.BigEndian.PutUint16(ifd[2:], 0x0112) // Orientation
	binary.BigEndian.PutUint16(ifd[4:], 3)      // SHORT
	binary.BigEndian.PutUint32(ifd[6:], 1)
	binary.BigEndian.PutUint16(ifd[10:], uint16(orientation))
	payload := append([]byte("Exif\x00\x00"), append(tiff, ifd...)...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)
	return append(append([]byte{0xFF, 0xD8}, seg...), j[2:]...)
}

func mkbox(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }

// heif builds a minimal HEIF: primary item 2 with ispe (and optional
// irot/clap); item 1 is a thumbnail with a different ispe.
func heif(w, h uint32, rot byte, clap bool) []byte {
	fullbox := []byte{0, 0, 0, 0}
	props := [][]byte{
		mkbox("ispe", fullbox, u32(160), u32(120)), // 1: thumbnail
		mkbox("ispe", fullbox, u32(w), u32(h)),     // 2: primary
		mkbox("irot", []byte{rot}),                 // 3
	}
	primaryProps := []byte{2, 2, 0x80 | 3} // count, ispe#2, irot#3 (essential)
	if clap {
		props = append(props, mkbox("clap", u32(w-1), u32(1), u32(h), u32(1), u32(0), u32(1), u32(0), u32(1))) // 4
		primaryProps = []byte{3, 2, 3, 4}
	}
	ipco := mkbox("ipco", props...)
	ipma := mkbox("ipma", fullbox, u32(2),
		u16(1), []byte{1, 1},
		u16(2), primaryProps)
	meta := mkbox("meta", fullbox,
		mkbox("hdlr", fullbox, u32(0), []byte("pict"), make([]byte, 13)),
		mkbox("pitm", fullbox, u16(2)),
		mkbox("iprp", ipco, ipma))
	return append(mkbox("ftyp", []byte("heic"), u32(0), []byte("mif1heic")), meta...)
}

func TestDimensions(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		w, h int
	}{
		{"a.png", pngBytes(30, 20), 30, 20},
		{"plain.jpg", jpegWithOrientation(40, 10, 1), 40, 10},
		{"rotated.jpg", jpegWithOrientation(40, 10, 6), 10, 40},
		{"mirrored.jpg", jpegWithOrientation(40, 10, 2), 40, 10},
		{"a.heic", heif(4032, 3024, 0, false), 4032, 3024},
		{"rot90.heic", heif(4032, 3024, 1, false), 3024, 4032},
		{"rot180.heic", heif(4032, 3024, 2, false), 4032, 3024},
		{"clap.heic", heif(602, 670, 0, true), 601, 670},
	}
	for _, c := range cases {
		w, h, err := Dimensions(writeFile(t, c.name, c.data))
		if err != nil || w != c.w || h != c.h {
			t.Errorf("%s: got %dx%d, %v; want %dx%d", c.name, w, h, err, c.w, c.h)
		}
	}
	for _, bad := range [][]byte{[]byte("not an image at all"), heif(1, 1, 0, false)[:40]} {
		if _, _, err := Dimensions(writeFile(t, "bad", bad)); err == nil {
			t.Error("expected error")
		}
	}
}

func TestRotation(t *testing.T) {
	cases := map[string]struct {
		data []byte
		rot  int
	}{
		"plain.jpg":   {jpegWithOrientation(4, 2, 1), 0},
		"cw90.jpg":    {jpegWithOrientation(4, 2, 6), 90},
		"cw270.jpg":   {jpegWithOrientation(4, 2, 8), 270},
		"irot1.heic":  {heif(40, 20, 1, false), 270}, // 90° anticlockwise
		"irot3.heic":  {heif(40, 20, 3, false), 90},
		"irot2.heic":  {heif(40, 20, 2, false), 180},
		"noirot.heic": {heif(40, 20, 0, false), 0},
	}
	for name, c := range cases {
		if got, err := Rotation(writeFile(t, name, c.data)); err != nil || got != c.rot {
			t.Errorf("%s: rotation %d, %v; want %d", name, got, err, c.rot)
		}
	}
}

func TestConvertAppliesRotation(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/sips"); err != nil {
		t.Skip("sips not available")
	}
	// A JPEG stored 40x10 with EXIF orientation 6 (display 10x40). Whether sips
	// keeps the tag or we rotate pixels, the output must display as 10x40.
	src := writeFile(t, "rot.jpg", jpegWithOrientation(40, 10, 6))
	c := NewConverter(t.TempDir(), 1<<30)
	out, err := c.Convert(context.Background(), src, FormatJPEG, 0)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := Dimensions(out); w != 10 || h != 40 {
		t.Fatalf("converted display dims %dx%d, want 10x40", w, h)
	}
}

// TestHostileHEIF covers box sizes that previously made the parser seek
// backwards and loop forever (a 64-bit largesize that wraps negative).
func TestHostileHEIF(t *testing.T) {
	ftyp := mkbox("ftyp", []byte("heic"), u32(0))
	wrap := append(append([]byte{}, ftyp...), u32(1)...)
	wrap = append(wrap, []byte("free")...)
	big := make([]byte, 8)
	binary.BigEndian.PutUint64(big, ^uint64(0)-15) // 2^64-16
	wrap = append(wrap, big...)
	huge := append(append([]byte{}, ftyp...), u32(0xFFFFFFF0)...)
	huge = append(huge, []byte("mdat")...)
	many := append([]byte{}, ftyp...)
	for i := 0; i < 200; i++ {
		many = append(many, mkbox("free")...)
	}
	for name, b := range map[string][]byte{"wrap": wrap, "huge": huge, "many": many} {
		done := make(chan struct{})
		go func() {
			Dimensions(writeFile(t, name, b))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: parser hung", name)
		}
	}
}

func FuzzDimensions(f *testing.F) {
	f.Add(heif(10, 20, 1, true))
	f.Add(jpegWithOrientation(4, 4, 6))
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, b []byte) {
		p := filepath.Join(dir, "f")
		os.WriteFile(p, b, 0o600)
		Dimensions(p) // must not panic or hang
	})
}

func TestLivePhotoVideo(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "IMG_1.HEIC")
	os.WriteFile(img, nil, 0o600)
	if LivePhotoVideo(img) != "" {
		t.Fatal("no companion expected")
	}
	os.WriteFile(filepath.Join(dir, "other.MOV"), nil, 0o600)
	if got := LivePhotoVideo(img); filepath.Base(got) != "other.MOV" {
		t.Fatalf("single mov: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "IMG_1.mov"), nil, 0o600)
	if got := LivePhotoVideo(img); filepath.Base(got) != "IMG_1.mov" {
		t.Fatalf("same stem preferred: %q", got)
	}
}

func TestConvertAndCache(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/sips"); err != nil {
		t.Skip("sips not available")
	}
	src := writeFile(t, "big.png", pngBytes(800, 400))
	c := NewConverter(t.TempDir(), 1<<30)
	ctx := context.Background()

	out, err := c.Convert(ctx, src, FormatJPEG, 200)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := Dimensions(out); w != 200 || h != 100 {
		t.Fatalf("resized to %dx%d", w, h)
	}
	again, _ := c.Convert(ctx, src, FormatJPEG, 200)
	if again != out {
		t.Fatal("not served from cache")
	}
	// No upscaling.
	full, _ := c.Convert(ctx, src, FormatJPEG, 4000)
	if w, _, _ := Dimensions(full); w != 800 {
		t.Fatalf("upscaled to %d", w)
	}
	if _, err := c.Convert(ctx, writeFile(t, "x.txt", []byte("hello")), FormatJPEG, 0); err == nil {
		t.Fatal("expected conversion error")
	}

	// Prune to zero removes everything.
	c.MaxBytes = 0
	c.Prune()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("cache not pruned")
	}
}

func TestCanConvert(t *testing.T) {
	if !CanConvert("image/heic", "public.heic", FormatJPEG) || !CanConvert("", "com.apple.coreaudio-format", FormatM4A) {
		t.Fatal("expected convertible")
	}
	if CanConvert("video/quicktime", "", FormatJPEG) || CanConvert("image/png", "", FormatM4A) {
		t.Fatal("unexpected convertible")
	}
}
