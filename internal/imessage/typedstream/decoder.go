// Package typedstream decodes NeXTSTEP/Apple "typedstream" archives, the
// legacy NSArchiver format Messages uses for message.attributedBody.
//
// The format is a sequence of typed value groups. Each group starts with a
// type-encoding string (Objective-C @encode syntax, e.g. "@", "iI", "+")
// followed by one value per type. Strings, classes and objects are stored
// once and later referred to by index.
//
// References: python-typedstream (dgelessus), imessage-exporter (ReagentX),
// node-typedstream (as used by BlueBubbles).
package typedstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Tag bytes, interpreted as signed int8.
const (
	tagInt2     = -127 // 0x81: followed by a 2-byte integer
	tagInt4     = -126 // 0x82: followed by a 4-byte integer
	tagFloat    = -125 // 0x83: followed by a float or double
	tagNew      = -124 // 0x84: a new string/class/object follows
	tagNil      = -123 // 0x85: nil
	tagEnd      = -122 // 0x86: end of object
	firstTag    = -128
	lastTag     = -111
	firstRefNum = lastTag + 1 // -110 (0x92): reference number 0
)

// Class is an archived Objective-C class with its superclass chain.
type Class struct {
	Name    string
	Version int64
	Super   *Class
}

// Is reports whether c is name or a subclass of name.
func (c *Class) Is(name string) bool {
	for ; c != nil; c = c.Super {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Object is an archived object instance: its class and the value groups its
// encodeWithCoder: wrote.
type Object struct {
	Class  *Class
	Groups []Group
}

// ClassName returns the object's class name or "" if unknown.
func (o *Object) ClassName() string {
	if o == nil || o.Class == nil {
		return ""
	}
	return o.Class.Name
}

// Group is one encoded group: the type encoding and its decoded values.
//
// Value types: int64 (all integer types), float32, float64, []byte
// (unshared strings '+' and char arrays), string (C strings, atoms,
// selectors), *Class, *Object (nil for a nil object), []any (other arrays
// and structs).
type Group struct {
	Encoding string
	Values   []any
}

var (
	ErrTruncated = errors.New("typedstream: unexpected end of data")
	ErrHeader    = errors.New("typedstream: invalid header")
)

type decoder struct {
	b       []byte
	pos     int
	order   binary.ByteOrder
	strings []string
	objects []any // *Class, *Object or string (C strings)
	depth   int
}

const maxDepth = 256

// Decode parses a typedstream and returns its top-level groups.
func Decode(b []byte) ([]Group, error) {
	d := &decoder{b: b}
	if err := d.readHeader(); err != nil {
		return nil, err
	}
	var groups []Group
	for d.pos < len(d.b) {
		head, err := d.readHead()
		if err != nil {
			return groups, err
		}
		g, err := d.readGroup(head)
		if err != nil {
			return groups, err
		}
		groups = append(groups, g)
	}
	return groups, nil
}

func (d *decoder) readHeader() error {
	if len(d.b) < 2 {
		return ErrHeader
	}
	version, sigLen := d.b[0], int(d.b[1])
	d.pos = 2
	sig, err := d.readExact(sigLen)
	if err != nil {
		return ErrHeader
	}
	switch {
	case version == 4 && string(sig) == "streamtyped":
		d.order = binary.LittleEndian
	case version == 3 && string(sig) == "typedstream":
		d.order = binary.BigEndian
	default:
		return fmt.Errorf("%w: version %d signature %q", ErrHeader, version, sig)
	}
	head, err := d.readHead()
	if err != nil {
		return err
	}
	_, err = d.readInt(head, true) // system version, e.g. 1000
	return err
}

func (d *decoder) readExact(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.b) {
		return nil, ErrTruncated
	}
	out := d.b[d.pos : d.pos+n]
	d.pos += n
	return out, nil
}

func (d *decoder) readHead() (int8, error) {
	if d.pos >= len(d.b) {
		return 0, ErrTruncated
	}
	h := int8(d.b[d.pos])
	d.pos++
	return h, nil
}

func isTag(h int8) bool { return h >= firstTag && h <= lastTag }

func (d *decoder) readInt(head int8, signed bool) (int64, error) {
	switch head {
	case tagInt2:
		b, err := d.readExact(2)
		if err != nil {
			return 0, err
		}
		v := d.order.Uint16(b)
		if signed {
			return int64(int16(v)), nil
		}
		return int64(v), nil
	case tagInt4:
		b, err := d.readExact(4)
		if err != nil {
			return 0, err
		}
		v := d.order.Uint32(b)
		if signed {
			return int64(int32(v)), nil
		}
		return int64(v), nil
	}
	if isTag(head) {
		return 0, fmt.Errorf("typedstream: unexpected tag 0x%02x reading integer at %d", uint8(head), d.pos-1)
	}
	if signed {
		return int64(head), nil
	}
	return int64(uint8(head)), nil
}

func (d *decoder) readFloat(head int8, double bool) (any, error) {
	if head != tagFloat {
		v, err := d.readInt(head, true)
		if double {
			return float64(v), err
		}
		return float32(v), err
	}
	if double {
		b, err := d.readExact(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(d.order.Uint64(b)), nil
	}
	b, err := d.readExact(4)
	if err != nil {
		return nil, err
	}
	return math.Float32frombits(d.order.Uint32(b)), nil
}

func refIndex(n int64) int { return int(n - firstRefNum) }

func (d *decoder) readUnsharedString(head int8) ([]byte, error) {
	if head == tagNil {
		return nil, nil
	}
	n, err := d.readInt(head, false)
	if err != nil {
		return nil, err
	}
	return d.readExact(int(n))
}

// readSharedString reads a string stored in (or referenced from) the shared
// string table. ok is false for nil.
func (d *decoder) readSharedString(head int8) (s string, ok bool, err error) {
	switch head {
	case tagNil:
		return "", false, nil
	case tagNew:
		h, err := d.readHead()
		if err != nil {
			return "", false, err
		}
		b, err := d.readUnsharedString(h)
		if err != nil {
			return "", false, err
		}
		s := string(b)
		d.strings = append(d.strings, s)
		return s, true, nil
	}
	n, err := d.readInt(head, true)
	if err != nil {
		return "", false, err
	}
	i := refIndex(n)
	if i < 0 || i >= len(d.strings) {
		return "", false, fmt.Errorf("typedstream: string reference %d out of range (%d strings)", i, len(d.strings))
	}
	return d.strings[i], true, nil
}

func (d *decoder) lookupObject(head int8) (any, error) {
	n, err := d.readInt(head, true)
	if err != nil {
		return nil, err
	}
	i := refIndex(n)
	if i < 0 || i >= len(d.objects) {
		return nil, fmt.Errorf("typedstream: object reference %d out of range (%d objects)", i, len(d.objects))
	}
	return d.objects[i], nil
}

func (d *decoder) readClass(head int8) (*Class, error) {
	switch head {
	case tagNil:
		return nil, nil
	case tagNew:
		h, err := d.readHead()
		if err != nil {
			return nil, err
		}
		name, _, err := d.readSharedString(h)
		if err != nil {
			return nil, err
		}
		h, err = d.readHead()
		if err != nil {
			return nil, err
		}
		version, err := d.readInt(h, true)
		if err != nil {
			return nil, err
		}
		c := &Class{Name: name, Version: version}
		d.objects = append(d.objects, c)
		h, err = d.readHead()
		if err != nil {
			return nil, err
		}
		if c.Super, err = d.readClass(h); err != nil {
			return nil, err
		}
		// A malformed archive can reference c as its own ancestor.
		for s, n := c.Super, 0; s != nil; s, n = s.Super, n+1 {
			if s == c || n > maxDepth {
				return nil, errors.New("typedstream: cyclic class hierarchy")
			}
		}
		return c, nil
	}
	ref, err := d.lookupObject(head)
	if err != nil {
		return nil, err
	}
	c, ok := ref.(*Class)
	if !ok {
		return nil, fmt.Errorf("typedstream: reference is %T, expected class", ref)
	}
	return c, nil
}

func (d *decoder) readObject(head int8) (*Object, error) {
	switch head {
	case tagNil:
		return nil, nil
	case tagNew:
		d.depth++
		defer func() { d.depth-- }()
		if d.depth > maxDepth {
			return nil, errors.New("typedstream: object nesting too deep")
		}
		// The object is registered before its class so later references
		// resolve to the correct table indexes.
		o := &Object{}
		d.objects = append(d.objects, o)
		h, err := d.readHead()
		if err != nil {
			return nil, err
		}
		if o.Class, err = d.readClass(h); err != nil {
			return nil, err
		}
		for {
			h, err := d.readHead()
			if err != nil {
				return nil, err
			}
			if h == tagEnd {
				return o, nil
			}
			g, err := d.readGroup(h)
			if err != nil {
				return nil, err
			}
			o.Groups = append(o.Groups, g)
		}
	}
	ref, err := d.lookupObject(head)
	if err != nil {
		return nil, err
	}
	o, ok := ref.(*Object)
	if !ok {
		return nil, fmt.Errorf("typedstream: reference is %T, expected object", ref)
	}
	return o, nil
}

func (d *decoder) readCString(head int8) (any, error) {
	switch head {
	case tagNil:
		return nil, nil
	case tagNew:
		h, err := d.readHead()
		if err != nil {
			return nil, err
		}
		s, _, err := d.readSharedString(h)
		if err != nil {
			return nil, err
		}
		d.objects = append(d.objects, s)
		return s, nil
	}
	return d.lookupObject(head)
}

// readGroup reads a type encoding (whose head byte has been consumed) and
// the values it describes.
func (d *decoder) readGroup(head int8) (Group, error) {
	enc, ok, err := d.readSharedString(head)
	if err != nil {
		return Group{}, err
	}
	if !ok {
		return Group{}, fmt.Errorf("typedstream: nil type encoding at %d", d.pos)
	}
	types, err := parseEncoding(enc)
	if err != nil {
		return Group{}, err
	}
	g := Group{Encoding: enc}
	for _, t := range types {
		v, skip, err := d.readValue(t)
		if err != nil {
			return g, err
		}
		if !skip {
			g.Values = append(g.Values, v)
		}
	}
	return g, nil
}

func (d *decoder) readValue(t typeNode) (v any, skip bool, err error) {
	switch t.kind {
	case '!':
		return nil, true, nil
	case '[':
		if t.elem.kind == 'c' || t.elem.kind == 'C' {
			b, err := d.readExact(t.count)
			return b, false, err
		}
		arr := make([]any, 0, min(t.count, 64))
		for i := 0; i < t.count; i++ {
			v, _, err := d.readValue(*t.elem)
			if err != nil {
				return nil, false, err
			}
			arr = append(arr, v)
		}
		return arr, false, nil
	case '{', '(':
		fields := make([]any, 0, len(t.fields))
		for _, f := range t.fields {
			v, skip, err := d.readValue(f)
			if err != nil {
				return nil, false, err
			}
			if !skip {
				fields = append(fields, v)
			}
		}
		return fields, false, nil
	}

	head, err := d.readHead()
	if err != nil {
		return nil, false, err
	}
	switch t.kind {
	case 'c', 's', 'i', 'l', 'q':
		v, err = d.readInt(head, true)
	case 'C', 'S', 'I', 'L', 'Q', 'B':
		v, err = d.readInt(head, false)
	case 'f':
		v, err = d.readFloat(head, false)
	case 'd':
		v, err = d.readFloat(head, true)
	case '+':
		v, err = d.readUnsharedString(head)
	case '*':
		v, err = d.readCString(head)
	case '%', ':':
		var s string
		s, _, err = d.readSharedString(head)
		v = s
	case '#':
		v, err = d.readClass(head)
	case '@':
		v, err = d.readObject(head)
	default:
		err = fmt.Errorf("typedstream: unsupported type %q", t.kind)
	}
	return v, false, err
}

// typeNode is a parsed Objective-C type encoding element.
type typeNode struct {
	kind   byte
	count  int        // arrays
	elem   *typeNode  // arrays
	fields []typeNode // structs and unions
}

func parseEncoding(enc string) ([]typeNode, error) {
	p := &encParser{s: enc}
	var out []typeNode
	for p.i < len(p.s) {
		t, err := p.parse()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

type encParser struct {
	s string
	i int
}

func (p *encParser) parse() (typeNode, error) {
	if p.i >= len(p.s) {
		return typeNode{}, fmt.Errorf("typedstream: truncated type encoding %q", p.s)
	}
	c := p.s[p.i]
	p.i++
	switch c {
	case '[':
		start := p.i
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		n, err := strconv.Atoi(p.s[start:p.i])
		if err != nil {
			return typeNode{}, fmt.Errorf("typedstream: bad array encoding %q", p.s)
		}
		elem, err := p.parse()
		if err != nil {
			return typeNode{}, err
		}
		if p.i >= len(p.s) || p.s[p.i] != ']' {
			return typeNode{}, fmt.Errorf("typedstream: unterminated array encoding %q", p.s)
		}
		p.i++
		return typeNode{kind: '[', count: n, elem: &elem}, nil
	case '{', '(':
		closer := byte('}')
		if c == '(' {
			closer = ')'
		}
		// Skip the optional "Name=" prefix.
		for j := p.i; j < len(p.s); j++ {
			if p.s[j] == '=' {
				p.i = j + 1
				break
			}
			if p.s[j] == closer || p.s[j] == '{' || p.s[j] == '[' || p.s[j] == '(' {
				break
			}
		}
		t := typeNode{kind: c}
		for p.i < len(p.s) && p.s[p.i] != closer {
			f, err := p.parse()
			if err != nil {
				return typeNode{}, err
			}
			t.fields = append(t.fields, f)
		}
		if p.i >= len(p.s) {
			return typeNode{}, fmt.Errorf("typedstream: unterminated struct encoding %q", p.s)
		}
		p.i++
		return t, nil
	case 'c', 'C', 's', 'S', 'i', 'I', 'l', 'L', 'q', 'Q', 'B', 'f', 'd',
		'+', '*', '%', ':', '#', '@', '!':
		return typeNode{kind: c}, nil
	}
	return typeNode{}, fmt.Errorf("typedstream: unsupported type %q in encoding %q", c, p.s)
}
