package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func withTempStore(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "journal-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("JOURNAL_DATA_DIR", dir)
	// the dir is cached per process; each test gets its own
	cachedStoreDir = ""
	t.Cleanup(func() { cachedStoreDir = "" })
	return dir
}

func TestLoadEmptyStore(t *testing.T) {
	withTempStore(t)
	entries, err := loadEntries()
	if err != nil || entries != nil {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := withTempStore(t)
	in := []Entry{
		{Text: "coffee\nwith milk\n\nand a biscuit", At: ms(2026, 8, 12, 9, 5)},
		{Text: "standup", At: ms(2026, 8, 13, 10, 16)},
	}
	if err := saveEntries(in, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-08-13.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[10:16 AM] standup\n" {
		t.Errorf("day file = %q", data)
	}
	out, err := loadEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Text != "coffee\nwith milk\n\nand a biscuit" || out[1].Text != "standup" {
		t.Fatalf("round trip = %+v", out)
	}
	if out[1].At != ms(2026, 8, 13, 10, 16) {
		t.Errorf("at = %d", out[1].At)
	}
}

func TestLoadParses24hFormat(t *testing.T) {
	dir := withTempStore(t)
	body := "[07:02] early\n[15:27] afternoon\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-08-13.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := loadEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].At != ms(2026, 8, 13, 7, 2) || out[1].At != ms(2026, 8, 13, 15, 27) {
		t.Fatalf("out = %+v", out)
	}
}

func TestSameMinuteEntriesKeepFileOrder(t *testing.T) {
	dir := withTempStore(t)
	body := "[03:19 PM] first\n[03:19 PM] second\n[03:19 PM] third\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-08-13.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := loadEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Text != "first" || out[1].Text != "second" || out[2].Text != "third" {
		t.Fatalf("order lost: %+v", out)
	}
	if out[0].At == out[1].At || out[1].At == out[2].At {
		t.Errorf("identities collide: %+v", out)
	}
}

func TestSaveRemovesEmptiedDayFile(t *testing.T) {
	dir := withTempStore(t)
	if err := saveEntries([]Entry{{Text: "x", At: ms(2026, 8, 12, 9, 0)}}, false); err != nil {
		t.Fatal(err)
	}
	if err := saveEntries(nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08-12.txt")); !os.IsNotExist(err) {
		t.Errorf("emptied day file survived: %v", err)
	}
}

func TestMigrateLegacyJSON(t *testing.T) {
	dir := withTempStore(t)
	legacy := `[{"text":"old entry","at":` + itoa(ms(2026, 8, 12, 9, 5)) + `}]`
	if err := os.WriteFile(filepath.Join(dir, "entries.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := loadEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Text != "old entry" {
		t.Fatalf("migrated = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08-12.txt")); err != nil {
		t.Errorf("day file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "entries.json.bak")); err != nil {
		t.Errorf("legacy file not parked: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestNewAtIgnoresFutureEntries(t *testing.T) {
	// one future-dated At in the file must not drag every new entry after it
	future := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	at := newAt([]Entry{{At: future}})
	if at >= future {
		t.Errorf("at = %d warped past future entry %d", at, future)
	}
	if drift := at - time.Now().UnixMilli(); drift < -1000 || drift > 1000 {
		t.Errorf("at = %d not near now", at)
	}
}

func TestNewAtBumpsExactCollision(t *testing.T) {
	now := time.Now().UnixMilli()
	entries := []Entry{{At: now}, {At: now + 1}, {At: now + 2}}
	at := newAt(entries)
	for _, e := range entries {
		if e.At == at {
			t.Fatalf("at = %d collides", at)
		}
	}
}

func TestDedupeAt(t *testing.T) {
	entries := dedupeAt([]Entry{{Text: "a", At: 100}, {Text: "b", At: 100}, {Text: "c", At: 101}})
	seen := map[int64]bool{}
	for _, e := range entries {
		if seen[e.At] {
			t.Fatalf("duplicate At survived: %+v", entries)
		}
		seen[e.At] = true
	}
	if entries[0].At != 100 || entries[0].Text != "a" {
		t.Errorf("first occurrence must keep its At: %+v", entries[0])
	}
}

func TestLockStoreCreatesLockFile(t *testing.T) {
	dir := withTempStore(t)
	if err := lockStore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journal.lock")); err != nil {
		t.Errorf("lock file missing: %v", err)
	}
}
