# whatsapp-fakeit

A self-contained Go tool that generates a fake WhatsApp Android
`msgstore.db` with the exact same schema as a real one and synthetic,
internally-consistent data.

The schema is embedded into the binary via `go:embed`, so no source
database is required at runtime. Reference row-count distributions
(jids, chats, messages, side tables, message types, statuses, FK
fan-out, ...) are baked in as constants and were derived from a real
device's `msgstore.db`.

## Why

For testing, demos, fixtures, and reverse-engineering work where you
want a believable `msgstore.db` without using anyone's actual chats.

## Build

```sh
go build -o whatsapp-fakeit .
```

Requires the cgo `mattn/go-sqlite3` driver (already pinned in
`go.mod`). Standard `CGO_ENABLED=1` and a working C toolchain.

## Usage

```
usage: ./whatsapp-fakeit [-scale=N] [-seed=N] [-phone=+CC...] <output.db>
  -phone string
        primary phone number for the 'me' jid in E.164 format with
        leading '+' (e.g. +12025550123); empty = random
  -scale float
        scale factor for row counts (default 1)
        1.0 ~= a real ~24 MB msgstore.db
        0.1 ~= 2.4 MB
        2.0 ~= 50 MB
  -seed int
        PRNG seed; 0 = use current time (non-reproducible)
```

### Examples

Default 1.0x scale, random seed, random "me" phone:

```sh
./whatsapp-fakeit msgstore.db
```

Reproducible small fixture:

```sh
./whatsapp-fakeit -scale=0.1 -seed=42 fixture.db
```

Pin the "me" jid to a specific phone number:

```sh
./whatsapp-fakeit -phone=+12025550123 msgstore.db
```

When `-phone` is supplied, `jid._id=1` is reserved for the "me" jid
with `user=<digits>`, `server=s.whatsapp.net`, `type=0`, and
`raw_string=<digits>@s.whatsapp.net`. All outgoing messages
(`from_me=1`) reference this jid as their sender. When `-phone` is
empty, the first generated phone-type jid is used as "me" with a
random number.

## Output

The generated database is a valid SQLite 3 file with:

- The complete WhatsApp `msgstore.db` schema (tables, indexes,
  triggers, FTS5 virtual tables, including the `sqlite_schema`
  shadow-table entries needed for FTS to work).
- Referentially-consistent data: every `chat.jid_row_id`,
  `message.chat_row_id`, `message.sender_jid_row_id`, and side-table
  FK points at a row that actually exists.
- `chat.jid_row_id` UNIQUE constraint honored via per-type
  without-replacement sampling.
- Realistic distributions for jid types, chat types, message types,
  message statuses, and the proportion of messages that have text,
  media, quotes, ephemeral settings, or mentions.

Verify integrity:

```sh
sqlite3 msgstore.db 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

## Project layout

```
main.go              CLI, schema loader, generators dispatch, row inserts
schema.sql           Embedded WhatsApp msgstore schema (sqlite3 .dump format)
generators/
  primitives.go      Generic weighted picks, hex/byte/base64 helpers
  whatsapp.go        Domain generators: jids, key IDs, chat subjects,
                     message text, link previews, media info
go.mod / go.sum      Module deps (gofakeit v7, go-sqlite3)
```

## Notes

- The `me` jid is never excluded from `chat.jid_row_id` candidates —
  Notes-to-Self is a real WhatsApp feature and may legitimately appear
  as a 1:1 chat with yourself.
- Schema was captured via `sqlite3 .dump` (not just
  `.schema`), because FTS5 virtual tables need explicit
  `INSERT INTO sqlite_schema` rows with
  `PRAGMA writable_schema = ON` to round-trip cleanly.
