package macos

import "testing"

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion("26.5.2\n")
	if err != nil || v != (Version{26, 5, 2}) {
		t.Fatalf("got %v, %v", v, err)
	}
	if v, _ = ParseVersion("11"); v != (Version{11, 0, 0}) {
		t.Fatalf("got %v", v)
	}
	if !v.AtLeast(11, 0) || v.AtLeast(11, 1) || !(Version{26, 0, 0}).AtLeast(15, 4) {
		t.Fatal("AtLeast wrong")
	}
	if _, err := ParseVersion("x.y"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseAppleScriptError(t *testing.T) {
	e := parseAppleScriptError("-:120:180: execution error: Messages got an error: Not authorized to send Apple events to Messages. (-1743)\n")
	if e.Number != ErrNumNotAuthorized {
		t.Fatalf("number = %d", e.Number)
	}
	if e.Message != "Messages got an error: Not authorized to send Apple events to Messages." {
		t.Fatalf("message = %q", e.Message)
	}
}
