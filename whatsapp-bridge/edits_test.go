package main

// KAN-2066: message edits. Every test opens a throwaway store under t.TempDir(), never store/messages.db.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	editChat   = "120363000000000001@g.us"
	otherChat  = "120363000000000002@g.us"
	author     = "447700900001"
	stranger   = "447700900002"
	originalID = "3BF796B2630B4AA7A03B"
)

var (
	sentAt   = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	editedAt = time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC)
)

func ptr[T any](v T) *T { return &v }

func testStore(t *testing.T) *MessageStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	store, err := openMessageStore("file:" + path + "?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	for _, jid := range []string{editChat, otherChat} {
		if err := store.StoreChat(jid, "Team", sentAt); err != nil {
			t.Fatalf("store chat: %v", err)
		}
	}
	return store
}

func storeOriginal(t *testing.T, store *MessageStore, chat, sender string) {
	t.Helper()
	if err := store.StoreMessage(originalID, chat, sender, "I have one question about the 25 cases I'm reviewing.", sentAt,
		false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("store original: %v", err)
	}
}

// editEvent is an edit as the phone sends it, wrapped or bare, put through whatsmeow's own UnwrapRaw as the client
// does before dispatching it (message.go: dispatchEvent(evt.UnwrapRaw())).
func editEvent(editID, chat, sender, text string, wrapped bool, fromMe bool) *events.Message {
	inner := &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type:          ptr(waProto.ProtocolMessage_MESSAGE_EDIT),
		Key:           &waProto.MessageKey{ID: ptr(originalID)},
		EditedMessage: &waProto.Message{Conversation: ptr(text)},
	}}
	raw := inner
	if wrapped {
		raw = &waProto.Message{EditedMessage: &waProto.FutureProofMessage{Message: inner}}
	}
	chatJID, _ := types.ParseJID(chat)
	return (&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chatJID, Sender: types.NewJID(sender, types.DefaultUserServer), IsFromMe: fromMe},
			ID:            editID,
			Timestamp:     editedAt,
		},
		RawMessage: raw,
	}).UnwrapRaw()
}

type row struct {
	rowid           int64
	content, sender string
	fromMe          bool
	editOf          sql.NullString
	timestamp       string
}

func readRow(t *testing.T, store *MessageStore, id, chat string) (row, bool) {
	t.Helper()
	var r row
	err := store.db.QueryRow("SELECT rowid, content, sender, is_from_me, edit_of, timestamp FROM messages WHERE id = ? AND chat_jid = ?",
		id, chat).Scan(&r.rowid, &r.content, &r.sender, &r.fromMe, &r.editOf, &r.timestamp)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	return r, true
}

func countRows(t *testing.T, store *MessageStore) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

const fullText = "I have one question about the 25 cases I'm reviewing.\nCase 14 has two owners: which one signs off?"

func TestEditIsItsOwnRowLinkedToTheOriginalWhichIsKept(t *testing.T) {
	for _, wrapped := range []bool{true, false} {
		store := testStore(t)
		storeOriginal(t, store, editChat, author)
		evt := editEvent("EDIT1", editChat, author, fullText, wrapped, false)
		if wrapped && !evt.IsEdit {
			t.Fatalf("CONTROL: whatsmeow did not mark the wrapped edit IsEdit")
		}
		if !recordEdit(store, nil, evt, waLog.Noop) {
			t.Fatalf("wrapped=%v: the edit was not recognised", wrapped)
		}
		orig, ok := readRow(t, store, originalID, editChat)
		if !ok || orig.content != "I have one question about the 25 cases I'm reviewing." || orig.editOf.Valid {
			t.Fatalf("wrapped=%v: the original changed: %+v", wrapped, orig)
		}
		edit, ok := readRow(t, store, "EDIT1", editChat)
		if !ok {
			t.Fatalf("wrapped=%v: no edit row", wrapped)
		}
		if edit.content != fullText || edit.editOf.String != originalID || edit.sender != author || edit.fromMe {
			t.Fatalf("wrapped=%v: wrong edit row: %+v", wrapped, edit)
		}
		if !strings.HasPrefix(edit.timestamp, "2026-10-20") || edit.rowid <= orig.rowid {
			t.Fatalf("wrapped=%v: the edit must be stored after the original, at the edit time: %+v", wrapped, edit)
		}
	}
}

func TestARedeliveredEditIsStoredOnceAndKeepsItsRow(t *testing.T) {
	store := testStore(t)
	storeOriginal(t, store, editChat, author)
	recordEdit(store, nil, editEvent("EDIT1", editChat, author, fullText, true, false), waLog.Noop)
	first, _ := readRow(t, store, "EDIT1", editChat)
	recordEdit(store, nil, editEvent("EDIT1", editChat, author, fullText, true, false), waLog.Noop)
	again, _ := readRow(t, store, "EDIT1", editChat)
	if countRows(t, store) != 2 || again.rowid != first.rowid {
		t.Fatalf("a redelivery must not add or move the row: rows=%d first=%d again=%d", countRows(t, store), first.rowid, again.rowid)
	}
}

func TestAnEditIsRefusedFromAnotherSenderOrForAMessageInAnotherChat(t *testing.T) {
	store := testStore(t)
	storeOriginal(t, store, editChat, author)
	recordEdit(store, nil, editEvent("EDIT-STRANGER", editChat, stranger, "forged", true, false), waLog.Noop)
	if _, ok := readRow(t, store, "EDIT-STRANGER", editChat); ok {
		t.Fatalf("an edit by someone other than the author was stored")
	}
	recordEdit(store, nil, editEvent("EDIT-ELSEWHERE", otherChat, author, "moved", true, false), waLog.Noop)
	if _, ok := readRow(t, store, "EDIT-ELSEWHERE", otherChat); ok {
		t.Fatalf("an edit naming a message of another chat was stored")
	}
	// CONTROL: the author's own edit in the original's chat is stored.
	recordEdit(store, nil, editEvent("EDIT-OK", editChat, author, fullText, true, false), waLog.Noop)
	if _, ok := readRow(t, store, "EDIT-OK", editChat); !ok {
		t.Fatalf("CONTROL: the author's edit was refused")
	}
}

func TestAnEditOfAMessageNotStoredIsKeptWithItsLink(t *testing.T) {
	store := testStore(t)
	recordEdit(store, nil, editEvent("EDIT1", editChat, author, fullText, true, false), waLog.Noop)
	edit, ok := readRow(t, store, "EDIT1", editChat)
	if !ok || edit.editOf.String != originalID {
		t.Fatalf("an edit of a message sent before the bridge ran must be kept, linked: %+v ok=%v", edit, ok)
	}
}

func TestTheUsersOwnEditIsStoredAsFromMe(t *testing.T) {
	store := testStore(t)
	storeOriginal(t, store, editChat, author)
	recordEdit(store, nil, editEvent("EDIT-MINE", editChat, author, fullText, true, true), waLog.Noop)
	edit, ok := readRow(t, store, "EDIT-MINE", editChat)
	if !ok || !edit.fromMe {
		t.Fatalf("the user's own edit must be stored with is_from_me, as their own messages are: %+v", edit)
	}
}

func TestAnEditWithNoTextIsSkippedAndOtherProtocolMessagesAreNotEdits(t *testing.T) {
	store := testStore(t)
	storeOriginal(t, store, editChat, author)
	if !recordEdit(store, nil, editEvent("EDIT-EMPTY", editChat, author, "", true, false), waLog.Noop) {
		t.Fatalf("an empty edit is still an edit")
	}
	if _, ok := readRow(t, store, "EDIT-EMPTY", editChat); ok {
		t.Fatalf("an edit with no text was stored")
	}
	revoke := editEvent("REVOKE1", editChat, author, "", false, false)
	revoke.Message.ProtocolMessage.Type = ptr(waProto.ProtocolMessage_REVOKE)
	if recordEdit(store, nil, revoke, waLog.Noop) {
		t.Fatalf("a revoke was handled as an edit")
	}
	if countRows(t, store) != 1 {
		t.Fatalf("only the original should be stored")
	}
}

func TestTheEditColumnIsAddedOnceToAStoreCreatedBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
			PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid))`,
		`INSERT INTO chats VALUES ('` + editChat + `', 'Team', '2026-10-20 09:00:00+00:00')`,
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('` + originalID + `', '` + editChat + `', '` + author + `', 'kept', '2026-10-20 09:00:00+00:00', 0)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("old schema: %v", err)
		}
	}
	old.Close()
	for i := 0; i < 2; i++ {
		store, err := openMessageStore("file:" + path + "?_foreign_keys=on")
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var n int
		store.db.QueryRow("SELECT count(*) FROM pragma_table_info('messages') WHERE name = 'edit_of'").Scan(&n)
		r, ok := readRow(t, store, originalID, editChat)
		store.Close()
		if n != 1 || !ok || r.content != "kept" || r.editOf.Valid {
			t.Fatalf("open %d: edit_of columns=%d, original kept=%v %+v", i, n, ok, r)
		}
	}
}

func TestTheHookPayloadNamesTheMessageAnEditEdits(t *testing.T) {
	body, _ := json.Marshal(HookPayload{ID: "EDIT1", EditOf: originalID})
	if !strings.Contains(string(body), `"edit_of":"`+originalID+`"`) {
		t.Fatalf("hook payload: %s", body)
	}
	plain, _ := json.Marshal(HookPayload{ID: "M1"})
	if strings.Contains(string(plain), "edit_of") {
		t.Fatalf("a message that is not an edit must not carry edit_of: %s", plain)
	}
}

// ---- Mira's KAN-2066 review M1: one person, two addresses (phone number and @lid).

const authorLID = "246000000000001"

// editEventAs is editEvent with the edit's Sender and SenderAlt given as JIDs, as whatsmeow fills them
// (types.MessageSource: Sender, and SenderAlt, the same user in the other addressing form).
func editEventAs(editID, chat string, sender, senderAlt types.JID, text string) *events.Message {
	evt := editEvent(editID, chat, sender.User, text, true, false)
	evt.Info.Sender = sender
	evt.Info.SenderAlt = senderAlt
	return evt
}

func storeOriginalAs(t *testing.T, store *MessageStore, sender string) {
	t.Helper()
	if err := store.StoreMessage(originalID, editChat, sender, "first text", sentAt, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("store original: %v", err)
	}
}

func TestAnEditUnderTheOtherAddressFormIsStoredUnderTheOriginalsSender(t *testing.T) {
	phone := types.NewJID(author, types.DefaultUserServer)
	lid := types.NewJID(authorLID, types.HiddenUserServer)
	for _, c := range []struct {
		name              string
		stored            string
		sender, senderAlt types.JID
	}{
		{"original under the phone form, edit under @lid", author, lid, phone},
		{"original under @lid, edit under the phone form", authorLID, phone, lid},
	} {
		store := testStore(t)
		storeOriginalAs(t, store, c.stored)
		recordEdit(store, nil, editEventAs("EDIT-ALT", editChat, c.sender, c.senderAlt, fullText), waLog.Noop)
		edit, ok := readRow(t, store, "EDIT-ALT", editChat)
		if !ok {
			t.Fatalf("%s: a genuine edit under the other address form was refused", c.name)
		}
		if edit.sender != c.stored {
			t.Fatalf("%s: the edit must be stored under the original's sender %q, got %q", c.name, c.stored, edit.sender)
		}
	}
}

func TestCONTROL_anUnrelatedSenderAltDoesNotMakeAnEditTheAuthors(t *testing.T) {
	store := testStore(t)
	storeOriginalAs(t, store, author)
	lid := types.NewJID(authorLID, types.HiddenUserServer)
	recordEdit(store, nil, editEventAs("EDIT-UNRELATED", editChat, lid, types.NewJID(stranger, types.DefaultUserServer), "forged"), waLog.Noop)
	if _, ok := readRow(t, store, "EDIT-UNRELATED", editChat); ok {
		t.Fatalf("an edit whose Sender and SenderAlt are both someone else was stored")
	}
}

func TestAnEmptySenderAltMatchesNoStoredSender(t *testing.T) {
	store := testStore(t)
	storeOriginalAs(t, store, "") // a row stored with no sender
	recordEdit(store, nil, editEventAs("EDIT-NOALT", editChat, types.NewJID(stranger, types.DefaultUserServer), types.JID{}, "forged"), waLog.Noop)
	if _, ok := readRow(t, store, "EDIT-NOALT", editChat); ok {
		t.Fatalf("an empty SenderAlt matched an original stored with no sender")
	}
}
