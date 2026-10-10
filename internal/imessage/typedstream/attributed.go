package typedstream

import (
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// AttributedString is the decoded content of an NSAttributedString.
type AttributedString struct {
	Text string
	Runs []Run
}

// Run is a span of Text sharing one attribute dictionary. Start and Length
// are in UTF-16 code units, as in Foundation.
type Run struct {
	Start      int
	Length     int
	Attributes map[string]any
}

// ErrNoAttributedString is returned when the archive has no NSAttributedString.
var ErrNoAttributedString = errors.New("typedstream: no NSAttributedString found")

// DecodeAttributedString decodes a message.attributedBody blob.
//
// Text extraction is the priority; attribute runs are decoded on a
// best-effort basis and are omitted if their layout is unrecognized.
func DecodeAttributedString(b []byte) (*AttributedString, error) {
	groups, err := Decode(b)
	root := findAttributedString(groups)
	if root == nil {
		if err != nil {
			return nil, err
		}
		return nil, ErrNoAttributedString
	}
	// A decode error after the string itself was read still leaves us with
	// usable text, so only fail if we got nothing.
	out := &AttributedString{}
	if len(root.Groups) == 0 || len(root.Groups[0].Values) == 0 {
		return nil, ErrNoAttributedString
	}
	str, ok := root.Groups[0].Values[0].(*Object)
	if !ok || str == nil {
		return nil, ErrNoAttributedString
	}
	text, ok := nsStringValue(str)
	if !ok {
		return nil, ErrNoAttributedString
	}
	out.Text = text
	out.Runs = decodeRuns(root.Groups[1:])
	return out, nil
}

func findAttributedString(groups []Group) *Object {
	for _, g := range groups {
		for _, v := range g.Values {
			if o, ok := v.(*Object); ok && o != nil && o.Class.Is("NSAttributedString") {
				return o
			}
		}
	}
	return nil
}

// nsStringValue extracts the contents of an NSString/NSMutableString, which
// archives its UTF-8 bytes as a single '+' group.
func nsStringValue(o *Object) (string, bool) {
	if o == nil || !o.Class.Is("NSString") {
		return "", false
	}
	for _, g := range o.Groups {
		if g.Encoding == "+" && len(g.Values) == 1 {
			if b, ok := g.Values[0].([]byte); ok {
				if !utf8.Valid(b) {
					return string([]rune(string(b))), true // replaces invalid bytes
				}
				return string(b), true
			}
		}
	}
	return "", false
}

// decodeRuns interprets the groups following the string. NSAttributedString
// writes, per run, an "iI" group (dictionary number, length) and, the first
// time a dictionary number is used, an "@" group with the NSDictionary.
func decodeRuns(groups []Group) []Run {
	var runs []Run
	dicts := map[int64]map[string]any{}
	pos := 0
	for i := 0; i < len(groups); i++ {
		g := groups[i]
		if g.Encoding != "iI" || len(g.Values) != 2 {
			continue
		}
		num, _ := g.Values[0].(int64)
		length, _ := g.Values[1].(int64)
		attrs, seen := dicts[num]
		if !seen && i+1 < len(groups) && groups[i+1].Encoding == "@" && len(groups[i+1].Values) == 1 {
			if o, ok := groups[i+1].Values[0].(*Object); ok {
				if m, ok := ToGo(o).(map[string]any); ok {
					attrs = m
				}
			}
			dicts[num] = attrs
			i++
		}
		runs = append(runs, Run{Start: pos, Length: int(length), Attributes: attrs})
		pos += int(length)
	}
	return runs
}

// UTF16Slice returns the substring of s covering UTF-16 range [start, start+length).
func UTF16Slice(s string, start, length int) string {
	u := utf16.Encode([]rune(s))
	if start < 0 || start > len(u) {
		return ""
	}
	end := start + length
	if end > len(u) {
		end = len(u)
	}
	return string(utf16.Decode(u[start:end]))
}

// ToGo converts common archived Foundation objects into plain Go values:
// NSString → string, NSNumber → int64/float64, NSDictionary → map[string]any,
// NSArray → []any, NSData → []byte, NSURL → string. Anything else becomes
// map{"$class": name}.
func ToGo(v any) any {
	return toGo(v, 0)
}

func toGo(v any, depth int) any {
	if depth > 64 {
		return nil
	}
	o, ok := v.(*Object)
	if !ok {
		if c, ok := v.(*Class); ok && c != nil {
			return map[string]any{"$class": c.Name}
		}
		return v
	}
	if o == nil {
		return nil
	}
	c := o.Class
	switch {
	case c.Is("NSString"):
		s, _ := nsStringValue(o)
		return s
	case c.Is("NSNumber") || c.Is("NSValue"):
		if len(o.Groups) > 0 && len(o.Groups[0].Values) == 1 {
			return o.Groups[0].Values[0]
		}
	case c.Is("NSDictionary"):
		// Groups: "i" count, then alternating "@" key / "@" value.
		if len(o.Groups) > 0 && o.Groups[0].Encoding == "i" {
			m := map[string]any{}
			rest := o.Groups[1:]
			for i := 0; i+1 < len(rest); i += 2 {
				key, _ := toGo(firstValue(rest[i]), depth+1).(string)
				m[key] = toGo(firstValue(rest[i+1]), depth+1)
			}
			return m
		}
	case c.Is("NSArray") || c.Is("NSSet"):
		if len(o.Groups) > 0 && o.Groups[0].Encoding == "i" {
			arr := make([]any, 0, len(o.Groups)-1)
			for _, g := range o.Groups[1:] {
				arr = append(arr, toGo(firstValue(g), depth+1))
			}
			return arr
		}
	case c.Is("NSData"):
		for _, g := range o.Groups {
			if len(g.Values) == 1 {
				if b, ok := g.Values[0].([]byte); ok {
					return b
				}
			}
		}
	case c.Is("NSURL"):
		for _, g := range o.Groups {
			for _, v := range g.Values {
				if s, ok := toGo(v, depth+1).(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return map[string]any{"$class": c.Name}
}

func firstValue(g Group) any {
	if len(g.Values) == 0 {
		return nil
	}
	return g.Values[0]
}
