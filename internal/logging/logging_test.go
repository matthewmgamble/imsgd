package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	r, err := OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("a", 60) + "\n"
	for i := 0; i < 5; i++ {
		r.Write([]byte(line))
	}
	r.Close()
	for _, p := range []string{path, path + ".1", path + ".2"} {
		if fi, err := os.Stat(p); err != nil || fi.Size() > 100 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("kept too many files")
	}
}
