package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/matthewmgamble/imsgd/internal/config"
	"github.com/matthewmgamble/imsgd/internal/imessage/appledate"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/imessage/keyedarchive"
	"github.com/matthewmgamble/imsgd/internal/imessage/typedstream"
	"github.com/matthewmgamble/imsgd/internal/send"
)

const debugUsage = `Usage:
  imsgd debug chats      [-limit N]
  imsgd debug typedstream [-limit N] [-show]
  imsgd debug schema
  imsgd debug verify     (pages through the whole DB checking invariants)
  imsgd debug sql        QUERY   (read-only; prefer aggregate queries)
  imsgd debug plist      -table T -column C [-where SQL] [-n N]
  imsgd debug recent     -chat GUID [-n N]
  imsgd debug send       (-chat GUID | -to ADDRESS [-service SMS]) [-text TEXT] [-file PATH] [-stage=false]
`

func runDebug(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, debugUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "chats":
		return debugChats(args[1:])
	case "typedstream":
		return debugTypedstream(args[1:])
	case "send":
		return debugSend(args[1:])
	case "recent":
		return debugRecent(args[1:])
	case "schema":
		return debugSchema(args[1:])
	case "plist":
		return debugPlist(args[1:])
	case "sql":
		return debugSQL(args[1:])
	case "verify":
		return debugVerify(args[1:])
	case "media":
		return debugMedia(args[1:])
	case "contacts":
		return debugContacts(args[1:])
	}
	fmt.Fprint(os.Stderr, debugUsage)
	os.Exit(2)
	return nil
}

func openMessagesDB(cfgPath string) (*db.DB, config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, cfg, err
	}
	d, err := db.Open(cfg.MessagesDB)
	return d, cfg, err
}

// debugChats lists recent chats so a chat GUID can be picked for testing.
func debugChats(args []string) error {
	fs := flag.NewFlagSet("debug chats", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	limit := fs.Int("limit", 20, "number of chats")
	fs.Parse(args)

	d, _, err := openMessagesDB(*cfgPath)
	if err != nil {
		return err
	}
	defer d.Close()

	rows, err := d.SQL().Query(`
		SELECT c.guid, IFNULL(c.display_name, ''), c.style,
		       (SELECT COUNT(*) FROM chat_handle_join chj WHERE chj.chat_id = c.ROWID),
		       IFNULL((SELECT MAX(m.date) FROM chat_message_join cmj JOIN message m ON m.ROWID = cmj.message_id
		               WHERE cmj.chat_id = c.ROWID), 0) AS last
		FROM chat c ORDER BY last DESC LIMIT ?`, *limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var guid, name string
		var style, n, last int64
		if err := rows.Scan(&guid, &name, &style, &n, &last); err != nil {
			return err
		}
		lastStr := "-"
		if t, ok := appledate.ToTime(last); ok {
			lastStr = t.Local().Format(time.DateTime)
		}
		kind := "dm"
		if style == 43 {
			kind = "group"
		}
		fmt.Printf("%-19s %-5s %2d  %-45s %s\n", lastStr, kind, n, guid, name)
	}
	return rows.Err()
}

// debugTypedstream is Spike B: decode real attributedBody blobs and compare
// with the plain text column where both exist. By default only counts and
// lengths are printed, never message content.
func debugTypedstream(args []string) error {
	fs := flag.NewFlagSet("debug typedstream", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	limit := fs.Int("limit", 5000, "number of most recent messages with attributedBody to check (0 = all)")
	show := fs.Bool("show", false, "print message text for mismatches (private!)")
	fs.Parse(args)

	d, _, err := openMessagesDB(*cfgPath)
	if err != nil {
		return err
	}
	defer d.Close()

	q := `SELECT ROWID, text, attributedBody FROM message WHERE attributedBody IS NOT NULL ORDER BY ROWID DESC`
	if *limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", *limit)
	}
	rows, err := d.SQL().Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()

	var total, decodeErr, withText, match, matchAfterStrip, mismatch, textNull, textNullDecoded, emptyDecoded int
	errKinds := map[string]int{}
	attrKeys := map[string]int{}
	var mismatchIDs []int64
	start := time.Now()

	for rows.Next() {
		var rowid int64
		var text sql.NullString
		var body []byte
		if err := rows.Scan(&rowid, &text, &body); err != nil {
			return err
		}
		total++
		as, err := typedstream.DecodeAttributedString(body)
		if err != nil {
			decodeErr++
			errKinds[shortErr(err)]++
			if len(mismatchIDs) < 20 {
				mismatchIDs = append(mismatchIDs, rowid)
			}
			continue
		}
		for _, r := range as.Runs {
			for k := range r.Attributes {
				attrKeys[k]++
			}
		}
		if as.Text == "" {
			emptyDecoded++
		}
		if !text.Valid {
			textNull++
			if as.Text != "" {
				textNullDecoded++
			}
			continue
		}
		withText++
		switch {
		case as.Text == text.String:
			match++
		case strip(as.Text) == strip(text.String):
			matchAfterStrip++
		default:
			mismatch++
			if len(mismatchIDs) < 20 {
				mismatchIDs = append(mismatchIDs, rowid)
			}
			if *show {
				fmt.Printf("rowid %d mismatch:\n  text: %q\n  body: %q\n", rowid, text.String, as.Text)
			} else {
				fmt.Printf("rowid %d mismatch: text %d bytes, decoded %d bytes\n", rowid, len(text.String), len(as.Text))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	pct := func(n, of int) string {
		if of == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.2f%%", 100*float64(n)/float64(of))
	}
	fmt.Printf("\nchecked %d attributedBody blobs in %s\n", total, time.Since(start).Round(time.Millisecond))
	fmt.Printf("  decode errors:              %d (%s)\n", decodeErr, pct(decodeErr, total))
	for k, n := range errKinds {
		fmt.Printf("      %6d  %s\n", n, k)
	}
	fmt.Printf("  text column present:        %d\n", withText)
	fmt.Printf("    exact match:              %d (%s)\n", match, pct(match, withText))
	fmt.Printf("    match ignoring U+FFFC/ws: %d\n", matchAfterStrip)
	fmt.Printf("    mismatch:                 %d\n", mismatch)
	fmt.Printf("  text column NULL:           %d (recovered from body: %d, %s)\n", textNull, textNullDecoded, pct(textNullDecoded, textNull))
	fmt.Printf("  decoded to empty string:    %d\n", emptyDecoded)
	if len(mismatchIDs) > 0 {
		fmt.Printf("  sample problem rowids:      %v\n", mismatchIDs)
	}
	keys := make([]string, 0, len(attrKeys))
	for k := range attrKeys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return attrKeys[keys[i]] > attrKeys[keys[j]] })
	fmt.Println("  attribute keys seen in runs:")
	for _, k := range keys {
		fmt.Printf("      %6d  %s\n", attrKeys[k], k)
	}
	return nil
}

func strip(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "￼", ""))
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, " at "); i > 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// debugSend is Spike A: exercise each AppleScript send path.
func debugSend(args []string) error {
	fs := flag.NewFlagSet("debug send", flag.ExitOnError)
	chat := fs.String("chat", "", "target chat GUID")
	to := fs.String("to", "", "target phone number or email")
	service := fs.String("service", "iMessage", "iMessage or SMS (with -to)")
	text := fs.String("text", "", "text to send")
	file := fs.String("file", "", "file to send")
	stage := fs.Bool("stage", true, "copy the file under ~/Library/Messages/Attachments/imsgd before sending")
	fs.Parse(args)

	if (*chat == "") == (*to == "") {
		return errors.New("exactly one of -chat or -to is required")
	}
	if *text == "" && *file == "" {
		return errors.New("-text and/or -file is required")
	}
	svc := send.Service(*service)
	if svc != send.ServiceIMessage && svc != send.ServiceSMS {
		return fmt.Errorf("unknown service %q", *service)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if *text != "" {
		t0 := time.Now()
		var err error
		if *chat != "" {
			err = send.TextToChat(ctx, *chat, *text)
		} else {
			err = send.TextToAddress(ctx, *to, svc, *text)
		}
		fmt.Printf("text send: err=%v (%s)\n", err, time.Since(t0).Round(time.Millisecond))
		if err != nil {
			return err
		}
	}

	if *file != "" {
		path, err := filepath.Abs(*file)
		if err != nil {
			return err
		}
		if *stage {
			home, _ := os.UserHomeDir()
			dir := filepath.Join(home, "Library", "Messages", "Attachments", "imsgd")
			id := fmt.Sprintf("debug-%d", time.Now().UnixNano())
			if path, err = send.StageFile(dir, id, path); err != nil {
				return fmt.Errorf("stage: %w", err)
			}
			fmt.Println("staged at", path)
		}
		t0 := time.Now()
		if *chat != "" {
			err = send.FileToChat(ctx, *chat, path)
		} else {
			err = send.FileToAddress(ctx, *to, svc, path)
		}
		fmt.Printf("file send: err=%v (%s)\n", err, time.Since(t0).Round(time.Millisecond))
		if err != nil {
			return err
		}
	}
	fmt.Println("AppleScript returned without error. Confirm delivery in Messages, then check chat.db for is_sent/error on the new row.")
	return nil
}

// debugRecent shows the newest sent-by-me rows in a chat with their delivery
// state (incoming messages are never printed), to
// verify test sends.
func debugRecent(args []string) error {
	fs := flag.NewFlagSet("debug recent", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	chat := fs.String("chat", "", "chat GUID")
	n := fs.Int("n", 5, "number of messages")
	fs.Parse(args)
	if *chat == "" {
		return errors.New("-chat is required")
	}

	d, _, err := openMessagesDB(*cfgPath)
	if err != nil {
		return err
	}
	defer d.Close()

	rows, err := d.SQL().Query(`
		SELECT m.ROWID, m.guid, m.is_from_me, m.is_sent, m.is_delivered, m.error, IFNULL(m.service, ''),
		       m.date, m.attributedBody,
		       (SELECT COUNT(*) FROM message_attachment_join maj WHERE maj.message_id = m.ROWID),
		       IFNULL((SELECT a.transfer_name FROM message_attachment_join maj JOIN attachment a ON a.ROWID = maj.attachment_id
		               WHERE maj.message_id = m.ROWID LIMIT 1), '')
		FROM message m JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
		JOIN chat c ON c.ROWID = cmj.chat_id
		WHERE c.guid = ? AND m.is_from_me = 1 ORDER BY m.date DESC LIMIT ?`, *chat, *n)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowid, fromMe, sent, delivered, errCode, date, nAtt int64
		var guid, service, transfer string
		var body []byte
		if err := rows.Scan(&rowid, &guid, &fromMe, &sent, &delivered, &errCode, &service, &date, &body, &nAtt, &transfer); err != nil {
			return err
		}
		text := ""
		if as, err := typedstream.DecodeAttributedString(body); err == nil {
			text = as.Text
		}
		ts := "-"
		if t, ok := appledate.ToTime(date); ok {
			ts = t.Local().Format(time.TimeOnly)
		}
		fmt.Printf("%d %s %s from_me=%d sent=%d delivered=%d error=%d svc=%s att=%d %s\n    %q\n",
			rowid, ts, guid, fromMe, sent, delivered, errCode, service, nAtt, transfer, text)
	}
	return rows.Err()
}

// debugSchema prints chat.db's DDL (no row data) for building test fixtures.
func debugSchema(args []string) error {
	fs := flag.NewFlagSet("debug schema", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	fs.Parse(args)

	d, _, err := openMessagesDB(*cfgPath)
	if err != nil {
		return err
	}
	defer d.Close()

	rows, err := d.SQL().Query(`SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'view' THEN 2 ELSE 3 END, name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return err
		}
		fmt.Printf("%s;\n\n", ddl)
	}
	return rows.Err()
}

// debugPlist prints the *shape* of plist blobs in a column (keys, types,
// lengths and numbers only; strings and data are never printed).
func debugPlist(args []string) error {
	fs := flag.NewFlagSet("debug plist", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	table := fs.String("table", "message", "table")
	column := fs.String("column", "message_summary_info", "blob column")
	where := fs.String("where", "", "extra SQL condition (developer use)")
	n := fs.Int("n", 3, "number of rows")
	fs.Parse(args)

	d, _, err := openMessagesDB(*cfgPath)
	if err != nil {
		return err
	}
	defer d.Close()

	q := fmt.Sprintf(`SELECT ROWID, %s FROM %s WHERE %s IS NOT NULL AND length(%s) > 0`, *column, *table, *column, *column)
	if *where != "" {
		q += " AND (" + *where + ")"
	}
	q += fmt.Sprintf(" ORDER BY ROWID DESC LIMIT %d", *n)
	rows, err := d.SQL().Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowid int64
		var b []byte
		if err := rows.Scan(&rowid, &b); err != nil {
			return err
		}
		v, err := keyedarchive.Decode(b)
		fmt.Printf("rowid %d (%d bytes):\n", rowid, len(b))
		if err != nil {
			fmt.Println("  error:", err)
			continue
		}
		printShape(v, "  ")
	}
	return rows.Err()
}

func printShape(v any, indent string) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch t[k].(type) {
			case map[string]any, []any:
				fmt.Printf("%s%s:\n", indent, k)
				printShape(t[k], indent+"  ")
			default:
				fmt.Printf("%s%s: %s\n", indent, k, scalarShape(t[k]))
			}
		}
	case []any:
		fmt.Printf("%s[%d items]\n", indent, len(t))
		if len(t) > 0 {
			printShape(t[0], indent+"  ")
		}
	default:
		fmt.Printf("%s%s\n", indent, scalarShape(v))
	}
}

func scalarShape(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("string(%d)", len(t))
	case []byte:
		kind := "data"
		if len(t) > 13 && string(t[2:13]) == "streamtyped" {
			kind = "typedstream"
		} else if keyedarchive.IsBinaryPlist(t) {
			kind = "bplist"
		}
		return fmt.Sprintf("%s(%d)", kind, len(t))
	case nil:
		return "null"
	case time.Time:
		return "date"
	}
	return fmt.Sprintf("%T=%v", v, v)
}

// debugSQL runs an arbitrary read-only query (the DB is opened read-only and
// query_only). Intended for aggregate investigation during development.
func debugSQL(args []string) error {
	fs := flag.NewFlagSet("debug sql", flag.ExitOnError)
	dbPath := fs.String("db", "", "SQLite file to query read-only (default: chat.db)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: imsgd debug sql [-db FILE] QUERY")
	}
	var q interface {
		Query(string, ...any) (*sql.Rows, error)
	}
	if *dbPath != "" {
		other, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro")
		if err != nil {
			return err
		}
		defer other.Close()
		q = other
	} else {
		d, _, err := openMessagesDB("")
		if err != nil {
			return err
		}
		defer d.Close()
		q = d.SQL()
	}
	rows, err := q.Query(fs.Arg(0))
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	fmt.Println(strings.Join(cols, " | "))
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		out := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				out[i] = fmt.Sprintf("<%d bytes>", len(b))
			} else {
				out[i] = fmt.Sprint(v)
			}
		}
		fmt.Println(strings.Join(out, " | "))
	}
	return rows.Err()
}
