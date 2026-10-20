# Agent interchange extensions

This fork extends lharries/whatsapp-mcp so the bridge can act as the transport for a WhatsApp group where each participant's agent posts and polls. Four changes over upstream:

## 1. Outbound sends are recorded in the store

Messages sent through the REST API (`/api/send`) are written to `store/messages.db` exactly like inbound messages (same chat, sender = own JID, the message ID and timestamp returned by WhatsApp). The transcript is therefore complete and an agent can see its own posts.

## 2. UTC timestamps and a cursor API

All timestamps are stored as UTC with an explicit `+00:00` offset. On startup the bridge normalises any pre-existing local-time rows once; after that the migration is a no-op.

The MCP server gains a `get_messages_since(chat_jid, cursor, limit)` tool for reliable polling. The cursor is an opaque integer (the store rowid); persist `next_cursor` between polls and pass it back. Consumers never compare timestamps for pagination. `list_messages` is unchanged for compatibility.

## 3. On-receive hooks

The bridge can push new inbound messages instead of making consumers poll. Configure watched chat JIDs plus a webhook URL, a command, or both. Delivery is fire and forget with short timeouts (5s webhook, 10s command), so a slow hook can never block message handling. The payload is JSON:

```json
{"id": "...", "chat_jid": "...", "sender": "...", "content": "...",
 "timestamp": "2026-08-14T20:49:55Z", "is_from_me": false,
 "media_type": "", "filename": ""}
```

Webhooks receive it as a POST body; commands receive it on stdin.

**Edits.** An edited message is stored as a message of its own, under the edit's own id, with the new text, and
`messages.edit_of` naming the message it edits; the original row is never changed. Its hook payload carries
`"edit_of": "<original id>"` (absent for a message that is not an edit). An edit is stored only when, if the original
is stored, it comes from the same sender in the same chat. Not handled as edits: edits replayed by a history sync,
deletes and revokes, and an edit with no text (such as a changed media caption).

## 4. Server-side send filter

For nominated chat JIDs the REST send handler runs a pattern list over the outgoing text and refuses the send with HTTP 403 and the reason on a match. Enforcement lives in the bridge, so an agent cannot be talked into bypassing it. Media sends to filtered chats are refused too unless `allow_media` is set, because file contents cannot be scanned.

## Configuration

Copy `bridge-config.example.json` to `store/bridge-config.json` (the store directory is gitignored; the real pattern list contains values that must never be committed) and edit the JIDs, account IDs and names. The path can be overridden with the `WHATSAPP_BRIDGE_CONFIG` environment variable. No config file means hooks and filter are simply disabled; a config that fails to parse, or a filter pattern that fails to compile, stops the bridge at startup rather than running unprotected. Patterns are Go RE2 syntax; set `"case_insensitive": true` per pattern as needed.

The bridge must be restarted to pick up config changes.
