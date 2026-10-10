package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/matthewmgamble/imsgd/internal/contacts"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/media"
)

// debugMedia checks media.Dimensions against sips for on-disk image
// attachments and surveys files stored next to them (Live Photo layout).
// Prints counts and attachment ROWIDs only.
func debugMedia(args []string) error {
	fs := flag.NewFlagSet("debug media", flag.ExitOnError)
	limit := fs.Int("limit", 500, "max images to check")
	guid := fs.String("guid", "", "inspect one attachment: rotation, dimensions, sips view")
	fs.Parse(args)
	if *guid != "" {
		return inspectAttachment(*guid)
	}
	d, _, err := openMessagesDB("")
	if err != nil {
		return err
	}
	defer d.Close()
	rows, err := d.SQL().Query(`SELECT ROWID, filename, IFNULL(mime_type, '') FROM attachment
		WHERE transfer_state = 5 AND mime_type LIKE 'image/%' AND IFNULL(hide_attachment, 0) = 0
		ORDER BY ROWID DESC LIMIT ?`, *limit)
	if err != nil {
		return err
	}
	type att struct {
		id         int64
		path, mime string
	}
	var atts []att
	for rows.Next() {
		var a att
		rows.Scan(&a.id, &a.path, &a.mime)
		a.path = strings.Replace(a.path, "~", os.Getenv("HOME"), 1)
		atts = append(atts, a)
	}
	rows.Close()

	stats := map[string]int{}
	siblings := map[string]int{}
	var mismatches []string
	var ours, sipsDur time.Duration
	for _, a := range atts {
		if _, err := os.Stat(a.path); err != nil {
			stats["missing on disk"]++
			continue
		}
		t0 := time.Now()
		w, h, err := media.Dimensions(a.path)
		ours += time.Since(t0)
		if err != nil {
			stats["parse error "+a.mime]++
			continue
		}
		t0 = time.Now()
		sw, sh, err := sipsDims(a.path)
		sipsDur += time.Since(t0)
		if err != nil {
			stats["sips error"]++
			continue
		}
		switch {
		case w == sw && h == sh:
			stats["match "+a.mime]++
		case w == sh && h == sw:
			stats["rotated (display dims swap sips raw) "+a.mime]++
		default:
			stats["MISMATCH "+a.mime]++
			mismatches = append(mismatches, fmt.Sprintf("%d %s ours=%dx%d sips=%dx%d", a.id, a.mime, w, h, sw, sh))
		}
		if a.mime == "image/heic" {
			entries, _ := os.ReadDir(filepath.Dir(a.path))
			base := filepath.Base(a.path)
			stem := strings.TrimSuffix(base, filepath.Ext(base))
			for _, e := range entries {
				if e.Name() == base {
					continue
				}
				ext := strings.ToLower(filepath.Ext(e.Name()))
				same := "different name"
				if strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())) == stem {
					same = "same stem"
				}
				kind := "file"
				if e.IsDir() {
					kind = "dir"
				}
				siblings[ext+" "+kind+", "+same]++
			}
			if strings.Contains(a.path, ".pvt/") {
				stats["heic inside a .pvt directory"]++
			}
		}
	}
	fmt.Printf("checked %d images: ours %s total, sips %s total\n", len(atts), ours.Round(time.Millisecond), sipsDur.Round(time.Millisecond))
	printCounts("results", stats)
	printCounts("files beside HEIC attachments (by extension)", siblings)
	sort.Strings(mismatches)
	for i, m := range mismatches {
		if i == 20 {
			break
		}
		fmt.Println("  ", m)
	}
	return nil
}

func sipsDims(path string) (int, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/sips", "-g", "pixelWidth", "-g", "pixelHeight", path).Output()
	if err != nil {
		return 0, 0, err
	}
	var w, h int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, _ := strconv.Atoi(f[1])
			switch f[0] {
			case "pixelWidth:":
				w = n
			case "pixelHeight:":
				h = n
			}
		}
	}
	return w, h, nil
}

// debugContacts reports how many Messages handles resolve to a contact name
// (counts only; names are never printed).
func debugContacts(args []string) error {
	ctx := context.Background()
	x := contacts.New(contacts.DefaultDir(), "")
	t0 := time.Now()
	if err := x.Load(ctx); err != nil {
		return err
	}
	// Per-database counts and any other AddressBook-like files.
	root := contacts.DefaultDir()
	filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.HasSuffix(p, ".abcddb") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fi, _ := de.Info()
		dbh, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
		if err != nil {
			return nil
		}
		defer dbh.Close()
		var recs, phones, emails int
		dbh.QueryRow(`SELECT (SELECT COUNT(*) FROM ZABCDRECORD), (SELECT COUNT(*) FROM ZABCDPHONENUMBER), (SELECT COUNT(*) FROM ZABCDEMAILADDRESS)`).Scan(&recs, &phones, &emails)
		fmt.Printf("  %-70s records=%-5d phones=%-5d emails=%-5d modified=%s\n", rel, recs, phones, emails, fi.ModTime().Format("2006-01-02"))
		return nil
	})
	st := x.Stats()
	fmt.Printf("loaded %d databases, %d contacts, %d address keys (%d conflicting) in %s\n",
		st.Databases, st.Contacts, st.Addresses, st.Conflicts, time.Since(t0).Round(time.Millisecond))
	d, _, err := openMessagesDB("")
	if err != nil {
		return err
	}
	defer d.Close()
	hs, err := d.ListHandles(ctx, db.HandlesQuery{Limit: 5000})
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, h := range hs {
		kind := "phone"
		if strings.Contains(h.Address, "@") {
			kind = "email"
		} else if !strings.HasPrefix(h.Address, "+") {
			kind = "other (short code etc.)"
		}
		if x.Name(h.Address) != nil {
			counts[kind+" resolved"]++
		} else {
			counts[kind+" unresolved"]++
		}
	}
	printCounts(fmt.Sprintf("handles (%d)", len(hs)), counts)
	// Chats with recent activity matter most.
	chats, _ := d.ListChats(ctx, db.ChatsQuery{Limit: 50, IncludeParticipants: true})
	var people, named int
	for _, c := range chats {
		for _, p := range c.Participants {
			people++
			if x.Name(p.Address) != nil {
				named++
			}
		}
	}
	fmt.Printf("participants of the 50 most recent chats: %d/%d named\n", named, people)
	return nil
}

func inspectAttachment(guid string) error {
	d, _, err := openMessagesDB("")
	if err != nil {
		return err
	}
	defer d.Close()
	a, err := d.GetAttachment(context.Background(), guid)
	if err != nil {
		return err
	}
	rot, rerr := media.Rotation(a.Filename)
	w, h, derr := media.Dimensions(a.Filename)
	fmt.Printf("mime=%s rotation=%d (%v) display=%dx%d (%v)\n", a.MimeType, rot, rerr, w, h, derr)
	out, _ := exec.Command("/usr/bin/sips", "-g", "pixelWidth", "-g", "pixelHeight", "-g", "orientation", a.Filename).Output()
	fmt.Printf("sips: %s\n", strings.Join(strings.Fields(string(out))[1:], " "))
	tmp, _ := os.MkdirTemp("", "imsgd-inspect")
	defer os.RemoveAll(tmp)
	conv, err := media.NewConverter(tmp, 1<<30).Convert(context.Background(), a.Filename, media.FormatJPEG, 0)
	if err != nil {
		return err
	}
	cr, _ := media.Rotation(conv)
	cw, ch, _ := media.Dimensions(conv)
	out, _ = exec.Command("/usr/bin/sips", "-g", "pixelWidth", "-g", "pixelHeight", conv).Output()
	fmt.Printf("converted: rotation=%d display=%dx%d sips: %s\n", cr, cw, ch, strings.Join(strings.Fields(string(out))[1:], " "))
	return nil
}
