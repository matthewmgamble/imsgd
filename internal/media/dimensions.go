// Package media inspects and converts attachment files: image dimensions,
// HEIC→JPEG and audio→M4A conversion (via sips/afconvert), and Live Photo
// companions.
package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"

	_ "golang.org/x/image/webp"
)

// ErrUnsupported is returned for formats whose dimensions we can't read.
var ErrUnsupported = errors.New("media: unsupported format")

// Dimensions returns an image's display size: stored pixel dimensions with
// EXIF orientation (JPEG) or irot (HEIF/AVIF) applied, so a portrait photo
// stored landscape reports portrait dimensions. Only file headers are read.
func Dimensions(path string) (w, h int, err error) {
	w, h, rot, err := info(path)
	if err != nil {
		return 0, 0, err
	}
	if rot == 90 || rot == 270 {
		w, h = h, w
	}
	return w, h, nil
}

// Rotation returns the clockwise rotation, in degrees (0, 90, 180 or 270),
// that a viewer applies to the stored pixels: from EXIF orientation for
// JPEG, or the irot property for HEIF/AVIF. Mirroring is ignored.
func Rotation(path string) (int, error) {
	_, _, rot, err := info(path)
	return rot, err
}

// info returns stored pixel dimensions and the clockwise display rotation.
func info(path string) (w, h, rot int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, 0, 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, 0, err
	}
	if string(head[4:8]) == "ftyp" {
		return heifInfo(f)
	}
	r := bufio.NewReader(f)
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, 0, ErrUnsupported
	}
	if format == "jpeg" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			rot = exifRotation(jpegOrientation(io.LimitReader(f, 256<<10)))
		}
	}
	return cfg.Width, cfg.Height, rot, nil
}

// exifRotation maps EXIF orientation to clockwise degrees; the mirrored
// variants (2, 4, 5, 7) map to their rotation component.
func exifRotation(o int) int {
	switch o {
	case 3, 4:
		return 180
	case 5, 6:
		return 90
	case 7, 8:
		return 270
	}
	return 0
}

var _ = jpeg.Decode // keep the decoder registered explicitly

// jpegOrientation returns the EXIF orientation tag (1–8), or 0 if absent.
func jpegOrientation(r io.Reader) int {
	br := bufio.NewReader(r)
	var soi [2]byte
	if _, err := io.ReadFull(br, soi[:]); err != nil || soi != [2]byte{0xFF, 0xD8} {
		return 0
	}
	for {
		var m [4]byte
		if _, err := io.ReadFull(br, m[:]); err != nil || m[0] != 0xFF {
			return 0
		}
		marker := m[1]
		length := int(binary.BigEndian.Uint16(m[2:])) - 2
		if length < 0 {
			return 0
		}
		if marker == 0xDA || marker == 0xD9 { // start of scan / end: no EXIF
			return 0
		}
		seg := make([]byte, length)
		if _, err := io.ReadFull(br, seg); err != nil {
			return 0
		}
		if marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return exifOrientation(seg[6:])
		}
	}
}

func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(tiff[4:8]))
	if off+2 > len(tiff) {
		return 0
	}
	n := int(bo.Uint16(tiff[off:]))
	for i := 0; i < n; i++ {
		e := off + 2 + i*12
		if e+12 > len(tiff) {
			return 0
		}
		if bo.Uint16(tiff[e:]) == 0x0112 {
			return int(bo.Uint16(tiff[e+8:]))
		}
	}
	return 0
}

// ---- HEIF / AVIF (ISO base media file format) -----------------------------

type box struct {
	typ  string
	data []byte // payload (after the header)
}

// readBoxes splits b into boxes.
func readBoxes(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := uint64(8)
		switch size {
		case 1:
			if len(b) < 16 {
				return out
			}
			size, hdr = binary.BigEndian.Uint64(b[8:]), 16
		case 0:
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return out
		}
		out = append(out, box{typ: typ, data: b[hdr:size]})
		b = b[size:]
	}
	return out
}

func find(bs []box, typ string) (box, bool) {
	for _, b := range bs {
		if b.typ == typ {
			return b, true
		}
	}
	return box{}, false
}

// heifInfo reads the primary item's ispe (and irot/clap) from the meta box.
// Box sizes come from untrusted files, so every seek is bounds-checked
// against the file size and must move forward; the walk is also capped.
func heifInfo(f io.ReadSeeker) (int, int, int, error) {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, 0, ErrUnsupported
	}
	pos := int64(0)
	for boxes := 0; boxes < 64 && pos+8 <= end; boxes++ {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return 0, 0, 0, ErrUnsupported
		}
		var hdr [16]byte
		if _, err := io.ReadFull(f, hdr[:8]); err != nil {
			return 0, 0, 0, ErrUnsupported
		}
		size := uint64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hdrLen := uint64(8)
		switch size {
		case 1:
			if _, err := io.ReadFull(f, hdr[8:16]); err != nil {
				return 0, 0, 0, ErrUnsupported
			}
			size, hdrLen = binary.BigEndian.Uint64(hdr[8:16]), 16
		case 0: // box extends to end of file
			size = uint64(end - pos)
		}
		if size < hdrLen || size > uint64(end-pos) {
			return 0, 0, 0, ErrUnsupported
		}
		if typ == "meta" {
			if size-hdrLen > 16<<20 {
				return 0, 0, 0, ErrUnsupported
			}
			meta := make([]byte, size-hdrLen)
			if _, err := io.ReadFull(f, meta); err != nil || len(meta) < 4 {
				return 0, 0, 0, ErrUnsupported
			}
			return parseMeta(meta[4:]) // skip FullBox version/flags
		}
		pos += int64(size) // size >= 8, so always forward
	}
	return 0, 0, 0, ErrUnsupported
}

func parseMeta(b []byte) (int, int, int, error) {
	children := readBoxes(b)
	primary := uint32(0)
	if p, ok := find(children, "pitm"); ok && len(p.data) >= 6 {
		if p.data[0] == 0 {
			primary = uint32(binary.BigEndian.Uint16(p.data[4:]))
		} else if len(p.data) >= 8 {
			primary = binary.BigEndian.Uint32(p.data[4:])
		}
	}
	iprp, ok := find(children, "iprp")
	if !ok {
		return 0, 0, 0, ErrUnsupported
	}
	props := readBoxes(iprp.data)
	ipco, ok := find(props, "ipco")
	if !ok {
		return 0, 0, 0, ErrUnsupported
	}
	properties := readBoxes(ipco.data) // 1-based indexes in ipma
	ipma, ok := find(props, "ipma")
	if !ok {
		return 0, 0, 0, ErrUnsupported
	}

	var w, h, rot int
	var clapW, clapH int
	found := false
	for _, idx := range associations(ipma.data, primary) {
		if idx < 1 || idx > len(properties) {
			continue
		}
		p := properties[idx-1]
		switch p.typ {
		case "ispe":
			if len(p.data) >= 12 {
				w = int(binary.BigEndian.Uint32(p.data[4:]))
				h = int(binary.BigEndian.Uint32(p.data[8:]))
				found = true
			}
		case "irot":
			if len(p.data) >= 1 {
				rot = int(p.data[0] & 3)
			}
		case "clap":
			// Clean aperture: the displayed crop (widthN/widthD, heightN/heightD).
			if len(p.data) >= 16 {
				wn, wd := binary.BigEndian.Uint32(p.data[0:]), binary.BigEndian.Uint32(p.data[4:])
				hn, hd := binary.BigEndian.Uint32(p.data[8:]), binary.BigEndian.Uint32(p.data[12:])
				if wd != 0 && hd != 0 {
					clapW, clapH = int(wn/wd), int(hn/hd)
				}
			}
		}
	}
	if !found {
		return 0, 0, 0, ErrUnsupported
	}
	if clapW > 0 && clapH > 0 {
		w, h = clapW, clapH
	}
	// irot is in anticlockwise quarter turns; report clockwise degrees.
	return w, h, (360 - rot*90) % 360, nil
}

// associations returns the property indexes associated with item in ipma.
func associations(b []byte, item uint32) []int {
	if len(b) < 8 {
		return nil
	}
	version, flags := b[0], b[3]
	n := binary.BigEndian.Uint32(b[4:])
	p := 8
	for i := uint32(0); i < n; i++ {
		var id uint32
		if version < 1 {
			if p+2 > len(b) {
				return nil
			}
			id = uint32(binary.BigEndian.Uint16(b[p:]))
			p += 2
		} else {
			if p+4 > len(b) {
				return nil
			}
			id = binary.BigEndian.Uint32(b[p:])
			p += 4
		}
		if p >= len(b) {
			return nil
		}
		count := int(b[p])
		p++
		var idxs []int
		for j := 0; j < count; j++ {
			if flags&1 != 0 {
				if p+2 > len(b) {
					return nil
				}
				idxs = append(idxs, int(binary.BigEndian.Uint16(b[p:])&0x7FFF))
				p += 2
			} else {
				if p+1 > len(b) {
					return nil
				}
				idxs = append(idxs, int(b[p]&0x7F))
				p++
			}
		}
		if id == item {
			return idxs
		}
	}
	return nil
}
