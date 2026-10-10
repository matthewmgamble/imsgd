package keyedarchive

import (
	"reflect"
	"testing"
	"time"

	"howett.net/plist"
)

func archive(t *testing.T, objects []any) []byte {
	t.Helper()
	b, err := plist.Marshal(map[string]any{
		"$archiver": "NSKeyedArchiver",
		"$version":  100000,
		"$top":      map[string]any{"root": plist.UID(1)},
		"$objects":  objects,
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func class(name string, supers ...string) map[string]any {
	return map[string]any{"$classname": name, "$classes": append([]any{name}, toAny(supers)...)}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func TestKeyedArchive(t *testing.T) {
	objects := []any{
		"$null",
		// 1: root dictionary {name: "x", items: [1, "two"], when: date, url: ..., missing: nil, custom: obj}
		map[string]any{
			"$class":     plist.UID(2),
			"NS.keys":    []any{plist.UID(3), plist.UID(5), plist.UID(9), plist.UID(12), plist.UID(15), plist.UID(16)},
			"NS.objects": []any{plist.UID(4), plist.UID(6), plist.UID(10), plist.UID(13), plist.UID(0), plist.UID(17)},
		},
		class("NSMutableDictionary", "NSDictionary", "NSObject"),
		"name", "x",
		"items",
		map[string]any{"$class": plist.UID(7), "NS.objects": []any{plist.UID(8), plist.UID(18)}},
		class("NSArray", "NSObject"),
		int64(1),
		"when",
		map[string]any{"$class": plist.UID(11), "NS.time": 86400.0},
		class("NSDate", "NSObject"),
		"url",
		map[string]any{"$class": plist.UID(14), "NS.base": plist.UID(0), "NS.relative": plist.UID(19)},
		class("NSURL", "NSObject"),
		"missing",
		"custom",
		map[string]any{"$class": plist.UID(20), "flag": true, "nested": plist.UID(4)},
		map[string]any{"$class": plist.UID(21), "NS.string": "two"},
		"https://example.com/a",
		class("IMSomething", "NSObject"),
		class("NSMutableString", "NSString", "NSObject"),
	}
	v, err := Decode(archive(t, objects))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name":    "x",
		"items":   []any{uint64(1), "two"},
		"when":    time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC),
		"url":     "https://example.com/a",
		"missing": nil,
		"custom":  map[string]any{"$class": "IMSomething", "flag": true, "nested": "x"},
	}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got  %#v\nwant %#v", v, want)
	}
}

func TestPlainPlist(t *testing.T) {
	b, _ := plist.Marshal(map[string]any{"pgensw": float32(10), "ep": []any{0}}, plist.BinaryFormat)
	v, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["pgensw"] != float32(10) {
		t.Fatalf("got %#v", m)
	}
	if n, ok := Int(m["ep"].([]any)[0]); !ok || n != 0 {
		t.Fatal("Int failed")
	}
}

func TestCycleAndBadInput(t *testing.T) {
	objects := []any{
		"$null",
		map[string]any{"$class": plist.UID(2), "self": plist.UID(1)},
		class("Loop", "NSObject"),
	}
	if _, err := Decode(archive(t, objects)); err == nil {
		t.Fatal("expected cycle error")
	}
	if _, err := Decode([]byte("not a plist")); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := Decode(nil); err == nil {
		t.Fatal("expected empty error")
	}
}
