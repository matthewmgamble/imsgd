package contacts

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeAB(t *testing.T, path string, people [][5]string, phones, emails [][2]any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZNICKNAME TEXT, ZORGANIZATION TEXT)`,
		`CREATE TABLE ZABCDPHONENUMBER (Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZFULLNUMBER TEXT)`,
		`CREATE TABLE ZABCDEMAILADDRESS (Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZADDRESS TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range people {
		db.Exec(`INSERT INTO ZABCDRECORD VALUES (?, ?, ?, ?, ?)`, i+1, nullable(p[1]), nullable(p[2]), nullable(p[3]), nullable(p[4]))
	}
	for _, p := range phones {
		db.Exec(`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (?, ?)`, p[0], p[1])
	}
	for _, e := range emails {
		db.Exec(`INSERT INTO ZABCDEMAILADDRESS (ZOWNER, ZADDRESS) VALUES (?, ?)`, e[0], e[1])
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestIndex(t *testing.T) {
	dir := t.TempDir()
	makeAB(t, filepath.Join(dir, "AddressBook-v22.abcddb"), nil, nil, nil) // empty top-level DB
	makeAB(t, filepath.Join(dir, "Sources", "A", "AddressBook-v22.abcddb"),
		[][5]string{{"", "Jane", "Doe", "", ""}, {"", "", "", "", "Acme Plumbing"}, {"", "", "", "Bobby", ""}},
		[][2]any{{1, "(416) 555-0100"}, {2, "+1 905 555 0199"}, {3, "07700 900123"}},
		[][2]any{{1, "Jane.Doe@Example.com"}})
	makeAB(t, filepath.Join(dir, "Sources", "B", "AddressBook-v22.abcddb"),
		[][5]string{{"", "Janet", "", "", ""}},
		[][2]any{{1, "416-555-0100"}}, nil) // same number, different name → conflict

	x := New(dir, "CA")
	if err := x.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"+14165550100":         "Jane Doe", // first database wins
		"4165550100":           "Jane Doe",
		"jane.doe@example.com": "Jane Doe",
		"+19055550199":         "Acme Plumbing",
		// Stored in UK national format on a Canadian Mac: libphonenumber can't
		// place it, but the last-10-digits fallback still finds the contact.
		"+447700900123":      "Bobby",
		"07700 900123":       "Bobby",
		"nobody@example.com": "",
	}
	for addr, want := range cases {
		got := x.Name(addr)
		if (want == "" && got != nil) || (want != "" && (got == nil || *got != want)) {
			t.Errorf("Name(%q) = %v, want %q", addr, deref(got), want)
		}
	}
	st := x.Stats()
	if st.Databases != 3 || st.Contacts != 4 || st.Conflicts == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Sources", "A", "AddressBook-v22.abcddb")
	makeAB(t, path, [][5]string{{"", "Old", "Name", "", ""}}, [][2]any{{1, "+15550001111"}}, nil)
	x := New(dir, "US")
	x.Load(context.Background())

	db, _ := sql.Open("sqlite", path)
	db.Exec(`UPDATE ZABCDRECORD SET ZFIRSTNAME = 'New'`)
	db.Close()
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go x.Run(ctx, 20*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := x.Name("+15550001111"); n != nil && *n == "New Name" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not reloaded: %v", deref(x.Name("+15550001111")))
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
