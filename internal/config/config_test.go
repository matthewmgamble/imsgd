package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyEditingPreservesComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	os.WriteFile(path, []byte("# my server\nport: 9000 # custom port\napi_keys:\n  - name: old\n    key: \"0123456789abcdef0\"\n"), 0o644)

	key, err := AddKey(path, "phone-app")
	if err != nil || !strings.HasPrefix(key, "imsgd_") || len(key) < 40 {
		t.Fatalf("AddKey = %q, %v", key, err)
	}
	if _, err := AddKey(path, "phone-app"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# my server") || !strings.Contains(string(b), "# custom port") {
		t.Fatalf("comments lost:\n%s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	cfg, err := Load(path)
	if err != nil || cfg.Port != 9000 || len(cfg.APIKeys) != 2 || cfg.APIKeys[1].Key != key {
		t.Fatalf("reloaded = %+v, %v", cfg, err)
	}
	if err := RemoveKey(path, "old"); err != nil {
		t.Fatal(err)
	}
	names, _ := KeyNames(path)
	if len(names) != 1 || names[0] != "phone-app" {
		t.Fatalf("names = %v", names)
	}
	if err := RemoveKey(path, "nope"); err == nil {
		t.Fatal("removing unknown key succeeded")
	}
}

func TestAddKeyCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.yml")
	if _, err := AddKey(path, "first"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.APIKeys) != 1 || cfg.ValidateForServe() != nil {
		t.Fatalf("cfg = %+v err=%v validate=%v", cfg, err, cfg.ValidateForServe())
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	if c.ValidateForServe() == nil {
		t.Fatal("no keys should fail")
	}
	c.APIKeys = []APIKey{{Name: "a", Key: "short"}}
	if c.ValidateForServe() == nil {
		t.Fatal("short key should fail")
	}
	c.APIKeys = []APIKey{{Name: "a", Key: "0123456789abcdef"}, {Name: "a", Key: "0123456789abcdef1"}}
	if c.ValidateForServe() == nil {
		t.Fatal("duplicate names should fail")
	}
}
