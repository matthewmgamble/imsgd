// Package keyedarchive decodes binary property lists, resolving
// NSKeyedArchiver object graphs into plain Go values.
//
// Several chat.db columns hold plists: message_summary_info (edit/unsend
// history), payload_data (app/link previews), attachment attribution_info
// and chat properties. Some are plain plists, others NSKeyedArchiver graphs;
// Decode handles both.
package keyedarchive

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"howett.net/plist"
)

// Epoch is the NSDate reference date.
var epoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

const maxDepth = 128

// Decode parses a plist. If it is an NSKeyedArchiver archive, the object
// graph is resolved and the root object returned; otherwise the plist value
// is returned as is (with nested UIDs left untouched).
//
// Resolved values: NSDictionary → map[string]any, NSArray/NSSet → []any,
// NSString → string, NSData → []byte, NSDate → time.Time, NSNumber →
// int64/uint64/float64/bool, NSUUID → []byte, NSURL → string. Other
// objects become map[string]any of their resolved fields plus "$class".
func Decode(b []byte) (any, error) {
	if len(b) == 0 {
		return nil, errors.New("keyedarchive: empty input")
	}
	var v any
	if _, err := plist.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("keyedarchive: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["$archiver"] != "NSKeyedArchiver" {
		return v, nil
	}
	objects, ok := m["$objects"].([]any)
	if !ok {
		return nil, errors.New("keyedarchive: missing $objects")
	}
	top, ok := m["$top"].(map[string]any)
	if !ok {
		return nil, errors.New("keyedarchive: missing $top")
	}
	r := &resolver{objects: objects}
	if root, ok := top["root"]; ok {
		return r.resolve(root, 0)
	}
	out := map[string]any{}
	for k, v := range top {
		rv, err := r.resolve(v, 0)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	return out, nil
}

// IsBinaryPlist reports whether b starts with the bplist magic.
func IsBinaryPlist(b []byte) bool { return bytes.HasPrefix(b, []byte("bplist")) }

type resolver struct {
	objects []any
	nodes   int
}

// maxNodes bounds total work: shared references can otherwise expand an
// archive exponentially.
const maxNodes = 100_000

func (r *resolver) resolve(v any, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("keyedarchive: object graph too deep (cycle?)")
	}
	if r.nodes++; r.nodes > maxNodes {
		return nil, errors.New("keyedarchive: archive too large")
	}
	uid, ok := v.(plist.UID)
	if !ok {
		// Inline values (rare in archives) and containers of UIDs.
		switch t := v.(type) {
		case []any:
			return r.resolveSlice(t, depth)
		case map[string]any:
			return r.resolveDict(t, depth)
		}
		return v, nil
	}
	if int(uid) >= len(r.objects) {
		return nil, fmt.Errorf("keyedarchive: UID %d out of range", uid)
	}
	obj := r.objects[uid]
	if s, ok := obj.(string); ok && s == "$null" {
		return nil, nil
	}
	m, ok := obj.(map[string]any)
	if !ok {
		return obj, nil // string, number, data, bool
	}
	return r.resolveObject(m, depth)
}

func (r *resolver) resolveSlice(in []any, depth int) ([]any, error) {
	out := make([]any, 0, len(in))
	for _, e := range in {
		v, err := r.resolve(e, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (r *resolver) resolveDict(in map[string]any, depth int) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, e := range in {
		v, err := r.resolve(e, depth+1)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func (r *resolver) className(m map[string]any) string {
	uid, ok := m["$class"].(plist.UID)
	if !ok || int(uid) >= len(r.objects) {
		return ""
	}
	c, _ := r.objects[uid].(map[string]any)
	name, _ := c["$classname"].(string)
	return name
}

func (r *resolver) resolveObject(m map[string]any, depth int) (any, error) {
	class := r.className(m)
	switch class {
	case "NSDictionary", "NSMutableDictionary":
		keys, _ := m["NS.keys"].([]any)
		vals, _ := m["NS.objects"].([]any)
		if len(keys) != len(vals) {
			return nil, errors.New("keyedarchive: dictionary key/value count mismatch")
		}
		out := make(map[string]any, len(keys))
		for i := range keys {
			k, err := r.resolve(keys[i], depth+1)
			if err != nil {
				return nil, err
			}
			v, err := r.resolve(vals[i], depth+1)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = v
		}
		return out, nil
	case "NSArray", "NSMutableArray", "NSSet", "NSMutableSet", "NSOrderedSet", "NSMutableOrderedSet":
		items, _ := m["NS.objects"].([]any)
		return r.resolveSlice(items, depth)
	case "NSString", "NSMutableString":
		return r.resolve(m["NS.string"], depth+1)
	case "NSData", "NSMutableData":
		return m["NS.data"], nil
	case "NSDate":
		secs, ok := toFloat(m["NS.time"])
		if !ok {
			return nil, nil
		}
		return epoch.Add(time.Duration(secs * float64(time.Second))).UTC(), nil
	case "NSUUID":
		return m["NS.uuidbytes"], nil
	case "NSURL":
		rel, err := r.resolve(m["NS.relative"], depth+1)
		if err != nil {
			return nil, err
		}
		base, err := r.resolve(m["NS.base"], depth+1)
		if err != nil {
			return nil, err
		}
		if bs, ok := base.(string); ok && bs != "" {
			return bs + fmt.Sprint(rel), nil
		}
		return rel, nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "$class" {
			continue
		}
		rv, err := r.resolve(v, depth+1)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	if class != "" {
		out["$class"] = class
	}
	return out, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// Int extracts an integer from a decoded plist number.
func Int(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case float64:
		return int64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
