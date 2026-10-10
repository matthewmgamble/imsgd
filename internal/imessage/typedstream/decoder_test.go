package typedstream

import (
	"bytes"
	"testing"
)

// blob concatenates byte slices and strings into a test archive.
func blob(parts ...any) []byte {
	var buf bytes.Buffer
	for _, p := range parts {
		switch v := p.(type) {
		case byte:
			buf.WriteByte(v)
		case int:
			buf.WriteByte(byte(v))
		case string:
			buf.WriteString(v)
		case []byte:
			buf.Write(v)
		}
	}
	return buf.Bytes()
}

// lp returns a length-prefixed (single byte) string.
func lp(s string) []byte { return append([]byte{byte(len(s))}, s...) }

var header = blob(0x04, lp("streamtyped"), 0x81, 0xe8, 0x03)

// helloBody mirrors the layout Messages writes for "Hello" with one run
// carrying {__kIMMessagePartAttributeName: 0}.
//
// Shared strings: 0 "@", 1 NSAttributedString, 2 NSObject, 3 NSString,
// 4 "+", 5 "iI", 6 NSDictionary, 7 "i", 8 __kIMMessagePartAttributeName? no—
// that is an unshared '+' payload. 8 NSNumber, 9 NSValue.
// Objects: 0 root, 1 NSAttributedString, 2 NSObject, 3 str obj, 4 NSString,
// 5 dict obj, 6 NSDictionary, 7 key obj, 8 num obj, 9 NSNumber, 10 NSValue.
func helloBody(text string) []byte {
	return blob(header,
		0x84, lp("@"), // group encoding "@" (str 0)
		0x84,                                    // new object (obj 0)
		0x84, 0x84, lp("NSAttributedString"), 0, // new class (obj 1), name str 1, version 0
		0x84, 0x84, lp("NSObject"), 0, 0x85, // superclass (obj 2), str 2; its super nil
		0x92,                                // group "@" (ref str 0)
		0x84,                                // new object (obj 3)
		0x84, 0x84, lp("NSString"), 1, 0x94, // class (obj 4) str 3 v1, super -> obj 2
		0x84, lp("+"), lp(text), // group "+" (str 4) + payload
		0x86,                         // end NSString
		0x84, lp("iI"), 1, len(text), // run: dict #1, length
		0x92,                                    // group "@"
		0x84,                                    // new object (obj 5)
		0x84, 0x84, lp("NSDictionary"), 0, 0x94, // class (obj 6) str 6, super obj 2
		0x84, lp("i"), 1, // group "i" (str 7): count 1
		0x92, 0x84, 0x96, 0x96, lp("__kIMMessagePartAttributeName"), 0x86, // key: obj 7, class obj 4 (NSString), "+"
		0x92, 0x84, // value: new object (obj 8)
		0x84, 0x84, lp("NSNumber"), 0, 0x84, 0x84, lp("NSValue"), 0, 0x94, // classes obj 9, 10
		0x99, 0, // group "i" (ref str 7): 0
		0x86, // end NSNumber
		0x86, // end NSDictionary
		0x86, // end NSAttributedString
	)
}

func TestDecodeAttributedString(t *testing.T) {
	as, err := DecodeAttributedString(helloBody("Hello"))
	if err != nil {
		t.Fatal(err)
	}
	if as.Text != "Hello" {
		t.Fatalf("text = %q", as.Text)
	}
	if len(as.Runs) != 1 {
		t.Fatalf("runs = %+v", as.Runs)
	}
	r := as.Runs[0]
	if r.Start != 0 || r.Length != 5 {
		t.Fatalf("run range = %d,%d", r.Start, r.Length)
	}
	if v, ok := r.Attributes["__kIMMessagePartAttributeName"]; !ok || v != int64(0) {
		t.Fatalf("attributes = %#v", r.Attributes)
	}
}

func TestDecodeLongUTF8Text(t *testing.T) {
	// Lengths >= 128 use the 2-byte integer tag.
	long := bytes.Repeat([]byte("héllo 👋 "), 40)
	b := blob(header,
		0x84, lp("@"),
		0x84, 0x84, 0x84, lp("NSMutableAttributedString"), 0,
		0x84, 0x84, lp("NSAttributedString"), 0,
		0x84, 0x84, lp("NSObject"), 0, 0x85,
		0x92,
		0x84, 0x84, 0x84, lp("NSMutableString"), 1, 0x84, 0x84, lp("NSString"), 1, 0x95,
		0x84, lp("+"), 0x81, byte(len(long)), byte(len(long)>>8), long,
		0x86, 0x86,
	)
	as, err := DecodeAttributedString(b)
	if err != nil {
		t.Fatal(err)
	}
	if as.Text != string(long) {
		t.Fatalf("text mismatch: got %d bytes", len(as.Text))
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := map[string][]byte{
		"empty":       nil,
		"bad header":  blob(0x04, lp("nopenope")),
		"truncated":   helloBody("Hello")[:40],
		"bad ref":     blob(header, 0x84, lp("@"), 0x84, 0xA0),
		"no attr str": blob(header, 0x84, lp("i"), 5),
	}
	for name, b := range cases {
		if _, err := DecodeAttributedString(b); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseEncoding(t *testing.T) {
	types, err := parseEncoding("iI{_NSRange=QQ}[4c]@")
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 5 || types[2].kind != '{' || len(types[2].fields) != 2 || types[3].count != 4 {
		t.Fatalf("unexpected parse: %+v", types)
	}
}

func TestUTF16Slice(t *testing.T) {
	s := "a👋b"
	if got := UTF16Slice(s, 1, 2); got != "👋" {
		t.Fatalf("got %q", got)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(helloBody("Hello"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = DecodeAttributedString(b) // must not panic
	})
}

func TestEncodeRoundTrip(t *testing.T) {
	text := "Hi @Bob see ￼ ok" // 1 + 4 + 5 + 1 + 3 = mention at 3..7
	runs := []Run{
		{Length: 3, Attributes: map[string]any{"__kIMMessagePartAttributeName": 0}},
		{Length: 4, Attributes: map[string]any{"__kIMMessagePartAttributeName": 0, "__kIMMentionConfirmedMention": "bob@example.com"}},
		{Length: 5, Attributes: map[string]any{"__kIMMessagePartAttributeName": 0}},
		{Length: 1, Attributes: map[string]any{"__kIMMessagePartAttributeName": 1, "__kIMFileTransferGUIDAttributeName": "ATT-1"}},
		{Length: 3, Attributes: map[string]any{"__kIMMessagePartAttributeName": 2}},
	}
	as, err := DecodeAttributedString(EncodeAttributedString(text, runs))
	if err != nil {
		t.Fatal(err)
	}
	if as.Text != text || len(as.Runs) != len(runs) {
		t.Fatalf("got %q with %d runs", as.Text, len(as.Runs))
	}
	if r := as.Runs[1]; r.Start != 3 || r.Length != 4 || r.Attributes["__kIMMentionConfirmedMention"] != "bob@example.com" {
		t.Fatalf("mention run = %+v", r)
	}
	if r := as.Runs[3]; r.Attributes["__kIMFileTransferGUIDAttributeName"] != "ATT-1" || r.Attributes["__kIMMessagePartAttributeName"] != int64(1) {
		t.Fatalf("attachment run = %+v", r)
	}
	// Long text exercises multi-byte length encoding.
	long := string(bytes.Repeat([]byte("x"), 140))
	if as, err := DecodeAttributedString(EncodeAttributedString(long, nil)); err != nil || as.Text != long {
		t.Fatalf("long text: %v", err)
	}
}
