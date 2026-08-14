# journal-cli

Interstitial journaling in the terminal. Open the app, the cursor is already
blinking next to the current time. Write a line, press enter, it's logged.
Page back for previous days. That's the whole app.

A TUI port of [journal](../journal). Local-first: everything lives on the
device in plain text files. Optionally, `journal login` connects to the same
Supabase backend as the web app, so one account sees one timeline everywhere.

## Install

**Homebrew (macOS / Linux):**

```sh
brew tap lasagnapizza/tap
brew install journal-cli
```

**Arch Linux (AUR):**

```sh
yay -S journal-cli-bin
```

**Debian / Ubuntu (.deb):**

```sh
curl -LO https://github.com/lasagnapizza/journal-cli/releases/latest/download/journal-cli_linux_amd64.deb
sudo dpkg -i journal-cli_linux_amd64.deb
```

**Fedora / RHEL (.rpm):**

```sh
sudo rpm -i https://github.com/lasagnapizza/journal-cli/releases/latest/download/journal-cli_linux_amd64.rpm
```

**Alpine (.apk):**

```sh
curl -LO https://github.com/lasagnapizza/journal-cli/releases/latest/download/journal-cli_linux_amd64.apk
apk add --allow-untrusted journal-cli_linux_amd64.apk
```

**Go:**

```sh
go install github.com/lasagnapizza/journal-cli@latest
```

Prebuilt archives for macOS, Linux, and Windows are on the
[releases page](https://github.com/lasagnapizza/journal-cli/releases). Packages
install the binary as `journal` (`go install` names it `journal-cli` after
the module).

## Run

```bash
go build -o journal .
./journal          # 12-hour clock
./journal -24h     # 24-hour clock (or JOURNAL_24H=1)
```

Entries live as plain text, one file per day, in
`$XDG_DATA_HOME/journal-cli/` (falls back to `~/.local/share/journal-cli/`;
override with `JOURNAL_DATA_DIR`):

```
2026-08-13.txt:
[07:02 AM] coffee, reading mail
[07:54 AM] standup went long
```

One `[time] text` line per entry — the storage format is the export format,
so the journal is readable and greppable without the app. A multiline entry
continues on the following lines until the next timestamped line. The parser
accepts both `[03:27 PM]` and `[15:27]`. Writes are atomic (temp file +
rename), so a crash never truncates a day, and an exclusive lock refuses a
second running instance. An `entries.json` from an earlier build is migrated
to day files on first load and parked as `entries.json.bak`.

## Sync (optional)

The CLI can share the web app's Supabase backend — same table, same merge
rules, so the web app, the phone, and the terminal all see the same entries.
Without ever running `journal login`, nothing here exists: no network, no
credentials, pure local.

```bash
journal login    # asks for the Supabase URL + anon key (the web app's
                 # VITE_SUPABASE_* values), then your email; paste the 6-digit
                 # code or the whole magic link from the email
journal sync     # manual reconcile, prints a summary
journal logout   # forgets the session; local entries stay
```

Release binaries have the hosted backend baked in, so `login` goes straight
to the email prompt. Self-hosting your own Supabase? Set
`JOURNAL_SUPABASE_URL` / `JOURNAL_SUPABASE_ANON_KEY`, or just answer the
prompts a plain `go build` binary shows. The anon key is the same public
value the web app ships to every browser — row-level security is the
boundary, not the key.

How it works: entries sync to the `journal_entries` table keyed by the same
client-generated epoch-ms id the web app uses. The TUI pulls once at startup
and pushes each log/edit/delete as it happens; failed pushes queue in
`syncstate.json` and flush on the next run. Deletes are soft (`deleted_at`)
with local tombstones, so a pull never resurrects an entry deleted elsewhere.
Day files keep minute resolution, so after a restart entries re-match their
cloud rows by (minute, text) — which also means editing the day files in
`$EDITOR` syncs like any other edit. The refresh token lives in
`session.json` (0600) in the data dir.

## Stack

Go + Bubble Tea v2 + Bubbles v2 + Lip Gloss v2 (the `charm.land` module
paths). Flat files, one per feature:

- `app.go` — app shell: model, day pager, timeline render, composer, entry
  menu, export menu, main
- `store.go` — persistence: parse/write the per-day text files, atomic
  writes, the instance lock, timestamp generation (pure I/O, no TUI). `at`
  (epoch ms) is the entry's identity; files carry minute resolution, so
  same-minute entries get bumped a millisecond apart on load, in file order
- `daytime.go` — clock formatting, day bucketing, day labels, gap math,
  export text shapes (pure)
- `sync.go` — optional Supabase sync: session, magic-link login, PostgREST
  push/pull, and the pure three-way merge (`mergeSync`) that reconciles day
  files ⟷ last-sync state ⟷ cloud

Rules: shared time helpers go in `daytime.go`, persistence in `store.go`; no
new folders; a new feature = one new top-level file.

## Interaction

- The composer is always focused; just type. **Enter** logs the entry. Blank
  input does nothing. **Ctrl+J** (or Shift+Enter where the terminal reports
  it) is a newline inside the draft.
- One day per screen. **←/→** page between days when the draft is empty
  (a draft owns its caret); **Ctrl+←/→** page always. Only days that have
  entries are pages, plus today, always, because that's where the composer
  writes. Logging anything snaps you back to today.
- Gaps of 20 minutes or more between entries on the same day render a
  marker — the point of interstitial journaling is seeing where the time
  went.
- With an empty draft, **↑** picks the newest entry; **↑/↓** move the pick,
  stepping past the newest returns to the composer. On a picked entry,
  **d** (or delete) asks **y/n**, then deletes; **e** lifts its text into
  the composer — the prompt freezes on the entry's own time, **Enter** saves
  it back under that same timestamp, **Esc** discards. An in-progress draft
  is stashed for the duration and comes back after. Copying is the
  terminal's job — select the text.
- **Ctrl+E** opens the export menu: copy (OSC 52) or save (.txt in the
  current directory) the shown day or the whole history. A day is one
  `[03:27 PM] text` line per entry; history gets a date header over each
  day, absolute dates — "Yesterday" is meaningless in a file read next
  month.
- **PgUp/PgDn** scroll a long day. A day arrives parked on its newest entry.
- The clock ticks every second, and the page list rolls over at midnight
  while the app stays open.
- Deleting the last entry of a non-today day drops its page and falls back
  to today rather than showing a blank pager.
- **Ctrl+C** / **Ctrl+Q** quit. Every change is already saved.

## The point of the app

It stays very simple. One screen, one input, one keystroke to log. Before
adding anything — tags, search, edit, folders, streaks — say out loud what it
costs in keystrokes on the write path, and default to not adding it.

## Tests

`go test ./...` covers the pure helpers (`daytime.go`: formatting, grouping,
gap math, export shapes) and the store (round trip, empty load, id
collisions). Run after any non-trivial change.
