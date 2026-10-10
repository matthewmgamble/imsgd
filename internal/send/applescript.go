// Package send delivers outgoing messages through Messages.app.
package send

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/matthewmgamble/imsgd/internal/macos"
)

// Service selects the account used when sending to an address.
type Service string

const (
	ServiceIMessage Service = "iMessage"
	ServiceSMS      Service = "SMS"
)

// All scripts receive caller data via argv; nothing is interpolated into the
// script source.

const scriptTextToChat = `on run argv
	set theText to item 1 of argv
	set chatId to item 2 of argv
	tell application "Messages"
		set targetChat to a reference to chat id chatId
		send theText to targetChat
	end tell
end run`

const scriptFileToChat = `on run argv
	set theFile to POSIX file (item 1 of argv)
	set chatId to item 2 of argv
	tell application "Messages"
		set targetChat to a reference to chat id chatId
		send theFile to targetChat
	end tell
end run`

const scriptTextToAddress = `on run argv
	set theText to item 1 of argv
	set theAddress to item 2 of argv
	set useSMS to (item 3 of argv) is "SMS"
	tell application "Messages"
		if useSMS then
			set targetAccount to 1st account whose service type = SMS
		else
			set targetAccount to 1st account whose service type = iMessage
		end if
		set targetParticipant to participant theAddress of targetAccount
		send theText to targetParticipant
	end tell
end run`

const scriptFileToAddress = `on run argv
	set theFile to POSIX file (item 1 of argv)
	set theAddress to item 2 of argv
	set useSMS to (item 3 of argv) is "SMS"
	tell application "Messages"
		if useSMS then
			set targetAccount to 1st account whose service type = SMS
		else
			set targetAccount to 1st account whose service type = iMessage
		end if
		set targetParticipant to participant theAddress of targetAccount
		send theFile to targetParticipant
	end tell
end run`

// TextToChat sends text to an existing chat by GUID (e.g. "iMessage;-;+15551234567").
func TextToChat(ctx context.Context, chatGUID, text string) error {
	_, err := macos.RunAppleScript(ctx, scriptTextToChat, text, chatGUID)
	return err
}

// FileToChat sends the file at path to an existing chat.
func FileToChat(ctx context.Context, chatGUID, path string) error {
	_, err := macos.RunAppleScript(ctx, scriptFileToChat, path, chatGUID)
	return err
}

// TextToAddress sends text to a phone number or email, creating the 1:1
// conversation if needed.
func TextToAddress(ctx context.Context, address string, svc Service, text string) error {
	_, err := macos.RunAppleScript(ctx, scriptTextToAddress, text, address, string(svc))
	return err
}

// FileToAddress sends the file at path to a phone number or email.
func FileToAddress(ctx context.Context, address string, svc Service, path string) error {
	_, err := macos.RunAppleScript(ctx, scriptFileToAddress, path, address, string(svc))
	return err
}

// StageFile copies src into dir/<id>/<basename> so Messages can read it.
// Recent macOS versions refuse (or silently fail) to send files from
// arbitrary locations; files under ~/Library/Messages/Attachments work.
func StageFile(dir, id, src string) (string, error) {
	name := filepath.Base(src)
	if name == "." || name == "/" || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("invalid staging name")
	}
	destDir := filepath.Join(dir, id)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, name)
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}
