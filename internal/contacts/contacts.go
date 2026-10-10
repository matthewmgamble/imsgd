// Package contacts resolves phone numbers and emails to contact names by
// reading the macOS AddressBook databases directly (no Contacts framework,
// no CGO). Requires Full Disk Access.
package contacts

import (
	"context"
	"database/sql"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nyaruka/phonenumbers"

	_ "modernc.org/sqlite"
)

// DefaultDir is the AddressBook root.
func DefaultDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library", "Application Support", "AddressBook")
}

// DefaultRegion derives a phone region (e.g. "CA") from the macOS locale,
// falling back to "US".
func DefaultRegion() string {
	out, err := exec.Command("/usr/bin/defaults", "read", "-g", "AppleLocale").Output()
	if err == nil {
		loc := strings.TrimSpace(string(out)) // e.g. en_CA or en_CA@rg=usz…
		loc, _, _ = strings.Cut(loc, "@")
		if _, region, ok := strings.Cut(loc, "_"); ok && len(region) == 2 {
			return strings.ToUpper(region)
		}
	}
	return "US"
}

// Index maps normalized addresses to display names.
type Index struct {
	dir    string
	region string

	mu      sync.RWMutex
	names   map[string]string // normalized address → name
	sources map[string]time.Time
	stats   Stats
}

// Stats summarises the loaded index.
type Stats struct {
	Databases int
	Contacts  int
	Addresses int
	Conflicts int // addresses shared by contacts with different names
}

func New(dir, region string) *Index {
	if region == "" {
		region = DefaultRegion()
	}
	return &Index{dir: dir, region: region, names: map[string]string{}}
}

// Name returns the contact name for an address (phone or email), or nil.
func (x *Index) Name(address string) *string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, k := range x.keys(address) {
		if n, ok := x.names[k]; ok {
			return &n
		}
	}
	return nil
}

// Stats returns counts from the last load.
func (x *Index) Stats() Stats {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.stats
}

// keys returns lookup keys for an address, most specific first.
func (x *Index) keys(addr string) []string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if strings.Contains(addr, "@") {
		return []string{"e:" + strings.ToLower(addr)}
	}
	var keys []string
	if num, err := phonenumbers.Parse(addr, x.region); err == nil {
		keys = append(keys, "p:"+phonenumbers.Format(num, phonenumbers.E164))
	}
	// Fallback for numbers libphonenumber can't parse, or stored without a
	// country code in a different region: the last 10 digits.
	if d := digits(addr); len(d) >= 10 {
		keys = append(keys, "d:"+d[len(d)-10:])
	} else if d != "" {
		keys = append(keys, "d:"+d)
	}
	return keys
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// databases lists AddressBook-v22.abcddb files: the top-level one and one per
// account under Sources/.
func (x *Index) databases() []string {
	var out []string
	top := filepath.Join(x.dir, "AddressBook-v22.abcddb")
	if _, err := os.Stat(top); err == nil {
		out = append(out, top)
	}
	matches, _ := filepath.Glob(filepath.Join(x.dir, "Sources", "*", "AddressBook-v22.abcddb"))
	sort.Strings(matches)
	return append(out, matches...)
}

// modTimes returns each database's latest modification time (including WAL).
func modTimes(dbs []string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, p := range dbs {
		var latest time.Time
		for _, f := range []string{p, p + "-wal"} {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(latest) {
				latest = fi.ModTime()
			}
		}
		out[p] = latest
	}
	return out
}

// Load (re)reads every AddressBook database.
func (x *Index) Load(ctx context.Context) error {
	dbs := x.databases()
	names := map[string]string{}
	conflicts := map[string]bool{}
	st := Stats{Databases: len(dbs)}
	for _, p := range dbs {
		n, err := x.loadOne(ctx, p, names, conflicts)
		if err != nil {
			slog.Warn("read AddressBook database", "path", p, "err", err)
			continue
		}
		st.Contacts += n
	}
	st.Addresses = len(names)
	st.Conflicts = len(conflicts)
	x.mu.Lock()
	x.names, x.sources, x.stats = names, modTimes(dbs), st
	x.mu.Unlock()
	return nil
}

func (x *Index) loadOne(ctx context.Context, path string, names map[string]string, conflicts map[string]bool) (int, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return 0, err
	}
	defer db.Close()

	people := map[int64]string{}
	rows, err := db.QueryContext(ctx, `SELECT Z_PK, IFNULL(ZFIRSTNAME, ''), IFNULL(ZLASTNAME, ''),
		IFNULL(ZNICKNAME, ''), IFNULL(ZORGANIZATION, '') FROM ZABCDRECORD`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var pk int64
		var first, last, nick, org string
		if err := rows.Scan(&pk, &first, &last, &nick, &org); err != nil {
			rows.Close()
			return 0, err
		}
		if n := displayName(first, last, nick, org); n != "" {
			people[pk] = n
		}
	}
	rows.Close()

	add := func(owner int64, addr string) {
		name, ok := people[owner]
		if !ok {
			return
		}
		for _, k := range x.keys(addr) {
			if prev, ok := names[k]; ok && prev != name {
				conflicts[k] = true
				continue // first wins, deterministically (databases are sorted)
			}
			names[k] = name
		}
	}
	for _, q := range []string{
		`SELECT IFNULL(ZOWNER, 0), IFNULL(ZFULLNUMBER, '') FROM ZABCDPHONENUMBER`,
		`SELECT IFNULL(ZOWNER, 0), IFNULL(ZADDRESS, '') FROM ZABCDEMAILADDRESS`,
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var owner int64
			var addr string
			if err := rows.Scan(&owner, &addr); err == nil {
				add(owner, addr)
			}
		}
		rows.Close()
	}
	return len(people), nil
}

// displayName formats a contact like Messages: "First Last", else nickname,
// else organization.
func displayName(first, last, nick, org string) string {
	if n := strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last)); n != "" {
		return n
	}
	if n := strings.TrimSpace(nick); n != "" {
		return n
	}
	return strings.TrimSpace(org)
}

// Run reloads the index whenever an AddressBook database changes, checking
// every interval, until ctx is done.
func (x *Index) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			x.mu.RLock()
			prev := x.sources
			x.mu.RUnlock()
			cur := modTimes(x.databases())
			changed := len(cur) != len(prev)
			for p, m := range cur {
				if !prev[p].Equal(m) {
					changed = true
				}
			}
			if changed {
				if err := x.Load(ctx); err == nil {
					slog.Info("contacts reloaded", "contacts", x.Stats().Contacts)
				}
			}
		}
	}
}
