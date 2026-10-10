package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/matthewmgamble/imsgd/internal/imessage/db"
)

// debugVerify pages through the whole database with the query layer and
// checks invariants. Only counts are printed, never content.
func debugVerify(args []string) error {
	fs := flag.NewFlagSet("debug verify", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	fs.Parse(args)
	ctx := context.Background()

	d, _, err := openMessagesDB(*cfgPath)
	if err != nil {
		return err
	}
	defer d.Close()
	stats, err := d.Stats(ctx)
	if err != nil {
		return err
	}
	var problems []string
	fail := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// 1. Full sync via MessagesSince.
	t0 := time.Now()
	var since int64
	var total int
	counts := map[string]int{}
	reactions := map[string]int{}
	groupEvents := map[string]int{}
	for {
		page, err := d.MessagesSince(ctx, since, "", 1000)
		if err != nil {
			return fmt.Errorf("MessagesSince(%d): %w", since, err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			if m.RowID <= since {
				fail("MessagesSince not ascending at %d", m.RowID)
			}
			since = m.RowID
			total++
			r, ge := m.Reaction(), m.GroupEvent()
			switch {
			case r != nil:
				k := string(r.Kind)
				if r.Removed {
					k = "-" + k
				}
				reactions[k]++
			case ge != nil:
				groupEvents[string(ge.Kind)]++
			}
			if m.Body == nil {
				counts["no body"]++
			}
			if m.LocationShare() != nil {
				counts["location sharing notices"]++
			}
			if m.Text() == "" && len(m.Attachments) == 0 && r == nil && ge == nil && !m.IsRetracted() && m.LocationShare() == nil {
				counts["empty (not attachment/reaction/group/unsent)"]++
				if m.BalloonBundleID != "" {
					counts["  …of which app/balloon messages"]++
				}
				if m.ItemType != 0 {
					counts[fmt.Sprintf("  …of which item_type %d", m.ItemType)]++
				}
			}
			if m.DateEdited != nil {
				counts["edited"]++
				if len(m.Edits) > 0 {
					counts["edited with history"]++
				}
			}
			if m.IsRetracted() {
				counts["fully unsent"]++
			}
			if m.IsPartiallyRetracted() {
				counts["partially unsent"]++
			}
			if m.Body != nil && len(m.Body.Mentions) > 0 {
				counts["with mentions"]++
			}
			if m.Body != nil && len(m.Body.Links) > 0 {
				counts["with links"]++
			}
			if m.Body != nil && len(m.Body.Parts) > 1 {
				counts["multi-part"]++
			}
			if m.ChatGUID == "" {
				counts["no chat"]++
			}
			if m.ThreadOriginatorGUID != "" {
				counts["thread replies"]++
			}
			for _, a := range m.Attachments {
				counts["attachments"]++
				if a.HideAttachment {
					counts["attachments hidden (link previews etc.)"]++
					continue
				}
				if a.PreviewWidth > 0 {
					counts["attachments with preview dimensions"]++
				}
				if a.MimeType == "application/octet-stream" {
					counts["attachments with unknown mime"]++
				}
				if a.Filename == "" {
					counts["attachments without path"]++
				} else if _, err := os.Stat(a.Filename); err != nil {
					counts["attachments missing on disk"]++
				}
			}
			// Attachment parts in the body should refer to loaded attachments.
			if m.Body != nil {
				for _, p := range m.Body.Parts {
					if p.AttachmentGUID == "" {
						continue
					}
					found := false
					for _, a := range m.Attachments {
						if a.GUID == p.AttachmentGUID {
							found = true
						}
					}
					if !found {
						counts["attachment parts without attachment row"]++
					}
				}
			}
		}
	}
	syncDur := time.Since(t0)
	if int64(total) != stats.Messages {
		fail("MessagesSince returned %d messages, table has %d", total, stats.Messages)
	}

	// 2. Chat list paging.
	t0 = time.Now()
	var chats []*db.Chat
	q := db.ChatsQuery{Limit: 100, IncludeParticipants: true}
	for {
		page, err := d.ListChats(ctx, q)
		if err != nil {
			return fmt.Errorf("ListChats: %w", err)
		}
		if len(page) == 0 {
			break
		}
		chats = append(chats, page...)
		c := db.ChatCursorFor(page[len(page)-1])
		q.Before = &c
	}
	chatDur := time.Since(t0)
	if int64(len(chats)) != stats.Chats {
		fail("ListChats returned %d chats, table has %d", len(chats), stats.Chats)
	}
	seen := map[string]bool{}
	noParticipants := 0
	for i, c := range chats {
		if seen[c.GUID] {
			fail("chat %d listed twice", c.RowID)
		}
		seen[c.GUID] = true
		if i > 0 && chats[i-1].LastMessageAt != nil && c.LastMessageAt != nil && c.LastMessageAt.After(*chats[i-1].LastMessageAt) {
			fail("chat list not ordered at %d", c.RowID)
		}
		if len(c.Participants) == 0 {
			noParticipants++
		}
	}

	// 3. Per-chat message paging.
	t0 = time.Now()
	var inChats int
	for _, c := range chats {
		cq := db.ChatMessagesQuery{ChatGUID: c.GUID, Limit: 500}
		var prev *db.Message
		for {
			page, err := d.ChatMessages(ctx, cq)
			if err != nil {
				return fmt.Errorf("ChatMessages(%d): %w", c.RowID, err)
			}
			if len(page) == 0 {
				break
			}
			for _, m := range page {
				if prev != nil && m.Date != nil && prev.Date != nil && m.Date.After(*prev.Date) {
					fail("chat %d messages not ordered", c.RowID)
				}
				prev = m
			}
			inChats += len(page)
			cur := db.CursorFor(page[len(page)-1])
			cq.Before = &cur
		}
	}
	chatMsgDur := time.Since(t0)
	if want := total - counts["no chat"]; inChats != want {
		fail("per-chat paging found %d messages, expected %d", inChats, want)
	}

	fmt.Printf("messages: %d synced in %s (MessagesSince, pages of 1000)\n", total, syncDur.Round(time.Millisecond))
	fmt.Printf("chats:    %d listed in %s (%d without participants)\n", len(chats), chatDur.Round(time.Millisecond), noParticipants)
	fmt.Printf("per-chat: %d messages paged across all chats in %s\n", inChats, chatMsgDur.Round(time.Millisecond))
	printCounts("message stats", counts)
	printCounts("reactions", reactions)
	printCounts("group events", groupEvents)
	if len(problems) > 0 {
		fmt.Println("\nPROBLEMS:")
		for _, p := range problems {
			fmt.Println("  -", p)
		}
		return errors.New("verification failed")
	}
	fmt.Println("\nall invariants hold")
	return nil
}

func printCounts(title string, m map[string]int) {
	fmt.Printf("%s:\n", title)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %7d  %s\n", m[k], k)
	}
}
