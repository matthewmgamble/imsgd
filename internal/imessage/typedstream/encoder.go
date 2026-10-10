package typedstream

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// Encoder writes typedstream archives. It produces the subset Messages uses
// for attributedBody and exists to build test fixtures.
type Encoder struct {
	buf     bytes.Buffer
	strings map[string]int
	classes map[string]int // class name → object table index
	objects int
}

// NewEncoder starts a little-endian ("streamtyped") archive.
func NewEncoder() *Encoder {
	e := &Encoder{strings: map[string]int{}, classes: map[string]int{}}
	e.buf.WriteByte(4)
	e.buf.WriteByte(byte(len("streamtyped")))
	e.buf.WriteString("streamtyped")
	e.writeInt(1000)
	return e
}

// Bytes returns the archive.
func (e *Encoder) Bytes() []byte { return e.buf.Bytes() }

func (e *Encoder) writeInt(v int64) {
	switch {
	case v >= firstRefNum && v <= 127:
		e.buf.WriteByte(byte(int8(v)))
	case v >= -1<<15 && v < 1<<15:
		e.buf.WriteByte(0x81)
		binary.Write(&e.buf, binary.LittleEndian, int16(v))
	default:
		e.buf.WriteByte(0x82)
		binary.Write(&e.buf, binary.LittleEndian, int32(v))
	}
}

// writeLen writes an unsigned length; values the reader would mistake for a
// tag (128–145) use the 2-byte form.
func (e *Encoder) writeLen(n int) {
	if n < 128 || (n >= 146 && n <= 255) {
		e.buf.WriteByte(byte(n))
		return
	}
	if n < 1<<16 {
		e.buf.WriteByte(0x81)
		binary.Write(&e.buf, binary.LittleEndian, uint16(n))
		return
	}
	e.buf.WriteByte(0x82)
	binary.Write(&e.buf, binary.LittleEndian, uint32(n))
}

func (e *Encoder) sharedString(s string) {
	if i, ok := e.strings[s]; ok {
		e.writeInt(int64(i) + firstRefNum)
		return
	}
	e.buf.WriteByte(0x84)
	e.writeLen(len(s))
	e.buf.WriteString(s)
	e.strings[s] = len(e.strings)
}

// class writes a class and its superclass chain (names in order, most
// derived first).
func (e *Encoder) class(chain ...string) {
	if len(chain) == 0 {
		e.buf.WriteByte(0x85)
		return
	}
	if i, ok := e.classes[chain[0]]; ok {
		e.writeInt(int64(i) + firstRefNum)
		return
	}
	e.buf.WriteByte(0x84)
	e.sharedString(chain[0])
	e.writeInt(0)
	e.classes[chain[0]] = e.objects
	e.objects++
	e.class(chain[1:]...)
}

func (e *Encoder) beginObject(chain ...string) {
	e.buf.WriteByte(0x84)
	e.objects++ // registered before its class, as in the reader
	e.class(chain...)
}

func (e *Encoder) endObject() { e.buf.WriteByte(0x86) }

func (e *Encoder) nsString(s string) {
	e.beginObject("NSString", "NSObject")
	e.sharedString("+")
	e.writeLen(len(s))
	e.buf.WriteString(s)
	e.endObject()
}

// value writes a Foundation object for v: string → NSString, int/int64 →
// NSNumber, map[string]any → NSDictionary.
func (e *Encoder) value(v any) {
	switch t := v.(type) {
	case string:
		e.nsString(t)
	case int:
		e.nsNumber(int64(t))
	case int64:
		e.nsNumber(t)
	case map[string]any:
		e.nsDictionary(t)
	default:
		panic(fmt.Sprintf("typedstream encoder: unsupported value %T", v))
	}
}

func (e *Encoder) nsNumber(n int64) {
	e.beginObject("NSNumber", "NSValue", "NSObject")
	e.sharedString("q")
	e.writeInt(n)
	e.endObject()
}

func (e *Encoder) nsDictionary(m map[string]any) {
	e.beginObject("NSDictionary", "NSObject")
	e.sharedString("i")
	e.writeInt(int64(len(m)))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e.sharedString("@")
		e.nsString(k)
		e.sharedString("@")
		e.value(m[k])
	}
	e.endObject()
}

// EncodeAttributedString archives text with attribute runs the way Messages
// does. Run Start fields are ignored; runs are laid out consecutively and
// their Lengths (UTF-16 units) should sum to the text length.
func EncodeAttributedString(text string, runs []Run) []byte {
	e := NewEncoder()
	e.sharedString("@")
	e.beginObject("NSMutableAttributedString", "NSAttributedString", "NSObject")
	e.sharedString("@")
	e.nsString(text)
	dictNum := int64(0)
	for _, r := range runs {
		dictNum++
		e.sharedString("iI")
		e.writeInt(dictNum)
		e.writeInt(int64(r.Length))
		e.sharedString("@")
		e.nsDictionary(r.Attributes)
	}
	e.endObject()
	return e.Bytes()
}
