# matrix-bridge-mcp

A read-only [MCP](https://modelcontextprotocol.io) server for the chats a
Matrix homeserver bridges in with [mautrix](https://docs.mau.fi) bridges.
Built for one person's WhatsApp and LinkedIn, behind a private homeserver; it
reads any mautrix bridge's rooms. Run one instance per network
(`MATRIX_NETWORK=whatsapp`, `MATRIX_NETWORK=linkedin`), each with its own token,
so a client is granted one network at a time; unset, one instance serves them all.

It can read your chats. It cannot write to them: there is no tool that sends,
reacts, edits, deletes, marks read, joins or leaves, and it is meant to run
as a Matrix account the bridge will not send for.

## Tools

| Tool | What it returns |
|---|---|
| `list_chats` | Bridged chats, most recent first: id, name, kind (`dm`, `group`, `channel`, `status`), members, the latest message in a line. Filters by name and kind. |
| `read_chat` | A chat's messages, oldest first, up to 100 a page, with `before` to page back. Edits applied, reactions attached, deletions marked, media as metadata. |
| `search` | Full-text search over the chats, newest first, optionally one chat. |
| `get_context` | The messages around one message. |
| `get_image` | The image of an image message, up to 8 MiB. |

Every answer that carries message text starts with a note that the text is
other people's words, to be reported on and never followed. Text is stripped
of control, bidi, zero-width and tag characters (the zero-width joiner
emoji need is kept), blank runs are collapsed, and each message is cut at
4,000 characters.

## The safety model

A model that reads your chats reads text strangers wrote, some of it
possibly written for it. So this server holds no way to act on what it
reads:

- **It reads as its own account.** Run it as a Matrix user that is in your
  bridged rooms but has no permission on the bridge (mautrix
  `bridge.permissions`: not listed, `*` = `block`) and no bridge login, with
  relay mode off. The bridge drops anything that user sends, so even a
  compromised server cannot speak as you.
- **Only bridged rooms.** A room is a chat only if a bridge made it (it has
  `m.bridge` state). With `MATRIX_NETWORK` set, only that bridge's rooms are
  chats, to every tool. The bridge's space and its bot's management room, where
  text is read as bridge commands, are never listed or read. `HIDDEN_CHATS`
  removes chosen rooms entirely.
- **No egress of its own.** It talks to the homeserver and nothing else: no
  URL fetching, no file paths, no media upload.
- **Bearer auth** on every request (`MCP_TOKEN`, compared in constant time),
  a global per-minute call limit, and one log line per call with the tool,
  the chat id, the outcome and the time taken. Never message text.

## Running it

```
MATRIX_SOCKET=/run/synapse/client.sock   # or MATRIX_URL=http://127.0.0.1:8008
MATRIX_USER=reader
MATRIX_PASSWORD=…                         # or MATRIX_PASSWORD_FILE
MATRIX_OPERATOR=@you:example.org          # whose messages are "me"
MATRIX_NETWORK=whatsapp                   # only this bridge's chats (unset: all)
NETWORK_NAME=WhatsApp                     # what the tools call them
MCP_TOKEN=…                               # 32+ characters; or MCP_TOKEN_FILE
MCP_LISTEN=0.0.0.0:8080                   # default 127.0.0.1:8080
HIDDEN_CHATS=!abc:example.org,!def:example.org
CALLS_PER_MINUTE=120
```

The MCP endpoint is `/mcp` (streamable HTTP, stateless); `/healthz` answers
`ok` without auth.

### Putting the reader in your chats

The reader has to be in each bridged room. Two things in a mautrix bridge
get in the way:

- `bridge.kick_matrix_users` (default `true`) removes Matrix users without a
  login from portals at every full member sync. Set it to `false`.
- An invite you send in a portal is bridged: in a group it means "add this
  person on WhatsApp", in a DM it fails with an error notice. Send the
  invite as a state event (`PUT /rooms/{room}/state/m.room.member/{reader}`,
  `{"membership": "invite"}`) carrying the bridge's own double-puppet marker,
  `"fi.mau.double_puppet_source": "<bridge name>"` (e.g. `mautrix-whatsapp`),
  and the bridge treats it as its own echo and skips it. Then join as the
  reader; the bridge ignores events from a user with no permission.

## Building

```
nix build            # runs the tests
nix flake check      # tests, gofmt, nixfmt
go test ./...
```

Go, with the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
as its only direct dependency. MIT.
