package main

// Persistence: one plain-text file per day under the XDG data dir —
// "2026-08-13.txt", one "[03:27 PM] text" line per entry, exactly the export
// shape. A multiline entry continues on the following lines until the next
// timestamped line. Pure I/O — no TUI. The files on disk are the source of
// truth, and they are the export.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The dir never changes while the app runs, and persist() runs on every
// log/edit/delete — resolve the env vars and create the directory once and
// cache the result. Not sync.Once, so tests can reset it per temp dir.
var (
	storeMu        sync.Mutex
	cachedStoreDir string
)

func storeDir() (string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if cachedStoreDir != "" {
		return cachedStoreDir, nil
	}
	base := os.Getenv("JOURNAL_DATA_DIR")
	if base == "" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			base = filepath.Join(xdg, "journal-cli")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".local", "share", "journal-cli")
		}
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	cachedStoreDir = base
	return base, nil
}

var errStoreLocked = errors.New("another journal instance is already running")

// lockStore takes an exclusive flock on a lock file for the life of the
// process. Two instances would race the whole-file day rewrites, each
// silently dropping the other's entries — so the second instance refuses to
// start instead. The fd is deliberately kept open; the kernel releases the
// lock on exit.
func lockStore() error {
	dir, err := storeDir()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return errStoreLocked
	}
	return nil
}

var (
	dayFileRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.txt$`)
	entryLineRe = regexp.MustCompile(`^\[(\d{1,2}:\d{2}(?: [AP]M)?)\] (.*)$`)
)

// parseEntryTime accepts both clock shapes the app can write.
func parseEntryTime(s string) (hour, minute int, ok bool) {
	for _, layout := range []string{"03:04 PM", "15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Hour(), t.Minute(), true
		}
	}
	return 0, 0, false
}

func loadEntries() ([]Entry, error) {
	dir, err := storeDir()
	if err != nil {
		return nil, err
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	sawDayFile := false
	for _, f := range names {
		if !dayFileRe.MatchString(f.Name()) {
			continue
		}
		sawDayFile = true
		day, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(f.Name(), ".txt"), time.Local)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, parseDayFile(day, string(data))...)
	}
	if !sawDayFile {
		return migrateLegacyJSON(dir)
	}
	// files carry minute resolution, so entries within one minute collide;
	// dedupeAt bumps later ones a millisecond apart, preserving file order
	entries = dedupeAt(entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].At < entries[j].At })
	return entries, nil
}

// parseDayFile: a "[time] text" line starts an entry; any other line
// continues the previous entry's text (that's how multiline entries and
// their blank lines round-trip).
func parseDayFile(day time.Time, data string) []Entry {
	var entries []Entry
	for _, line := range strings.Split(data, "\n") {
		m := entryLineRe.FindStringSubmatch(line)
		if m == nil {
			if len(entries) > 0 {
				entries[len(entries)-1].Text += "\n" + line
			}
			continue
		}
		h, min, ok := parseEntryTime(m[1])
		if !ok {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, time.Local)
		entries = append(entries, Entry{Text: m[2], At: at.UnixMilli()})
	}
	for i := range entries {
		entries[i].Text = strings.TrimRight(entries[i].Text, "\n")
	}
	return entries
}

// migrateLegacyJSON: earlier builds kept one entries.json. Read it once,
// hand the entries back to be re-saved as day files, and park the old file
// as .bak so it is never imported twice.
func migrateLegacyJSON(dir string) ([]Entry, error) {
	path := filepath.Join(dir, "entries.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	entries = dedupeAt(entries)
	if err := saveEntries(entries, false); err != nil {
		return nil, err
	}
	if err := os.Rename(path, path+".bak"); err != nil {
		return nil, err
	}
	return entries, nil
}

// dedupeAt enforces At uniqueness — At is the entry's identity, and both the
// minute-resolution files and hand-edited data produce duplicates. Later
// duplicates get bumped forward a millisecond at a time, mirroring newAt.
func dedupeAt(entries []Entry) []Entry {
	seen := make(map[int64]bool, len(entries))
	for i := range entries {
		for seen[entries[i].At] {
			entries[i].At++
		}
		seen[entries[i].At] = true
	}
	return entries
}

// saveEntries rewrites every day's file and removes files for days that no
// longer have entries. Each write is atomic: temp file in the same dir, then
// rename, so a crash mid-write never truncates a day.
func saveEntries(entries []Entry, use24h bool) error {
	dir, err := storeDir()
	if err != nil {
		return err
	}
	days := groupByDay(entries)
	keep := make(map[string]bool, len(days))
	for _, d := range days {
		name := d.Key + ".txt"
		keep[name] = true
		path := filepath.Join(dir, name)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(dayText(d.Entries, use24h)+"\n"), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range names {
		if dayFileRe.MatchString(f.Name()) && !keep[f.Name()] {
			if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// newAt: epoch ms now, bumped only past exact collisions so two entries
// logged within the same millisecond stay distinct — At is the identity.
// It must not leap past the file's max: one future-dated At (clock skew,
// hand edit) would otherwise mis-date every entry logged after it.
func newAt(entries []Entry) int64 {
	at := time.Now().UnixMilli()
	taken := make(map[int64]bool, len(entries))
	for _, e := range entries {
		taken[e.At] = true
	}
	for taken[at] {
		at++
	}
	return at
}
