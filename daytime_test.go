package main

import (
	"strings"
	"testing"
	"time"
)

func ms(y int, mo time.Month, d, h, m int) int64 {
	return time.Date(y, mo, d, h, m, 0, 0, time.Local).UnixMilli()
}

func TestFmtTime(t *testing.T) {
	at := ms(2026, 8, 13, 15, 27)
	if got := fmtTime(at, false); got != "03:27 PM" {
		t.Errorf("12h = %q", got)
	}
	if got := fmtTime(at, true); got != "15:27" {
		t.Errorf("24h = %q", got)
	}
}

func TestDayLabel(t *testing.T) {
	now := ms(2026, 8, 13, 12, 0)
	if got := dayLabel(now, now); got != "Today" {
		t.Errorf("today = %q", got)
	}
	if got := dayLabel(ms(2026, 8, 12, 23, 59), now); got != "Yesterday" {
		t.Errorf("yesterday = %q", got)
	}
	if got := dayLabel(ms(2026, 8, 10, 9, 0), now); got != "Mon, Aug 10" {
		t.Errorf("same year = %q", got)
	}
	if got := dayLabel(ms(2025, 12, 31, 9, 0), now); got != "Wed, Dec 31, 2025" {
		t.Errorf("other year = %q", got)
	}
}

func TestGapLabel(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{19 * time.Minute, ""},
		{20 * time.Minute, "20m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h"},
		{84 * time.Minute, "1h 24m"},
		{120 * time.Minute, "2h"},
	}
	for _, c := range cases {
		if got := gapLabel(c.d); got != c.want {
			t.Errorf("gapLabel(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestGroupByDaySortsBothLevels(t *testing.T) {
	entries := []Entry{
		{Text: "late", At: ms(2026, 8, 13, 18, 0)},
		{Text: "old day", At: ms(2026, 8, 11, 9, 0)},
		{Text: "early", At: ms(2026, 8, 13, 8, 0)},
	}
	days := groupByDay(entries)
	if len(days) != 2 || days[0].Key != "2026-08-11" || days[1].Key != "2026-08-13" {
		t.Fatalf("days = %+v", days)
	}
	if days[1].Entries[0].Text != "early" || days[1].Entries[1].Text != "late" {
		t.Errorf("entries not oldest-first: %+v", days[1].Entries)
	}
}

func TestDayPagesAlwaysIncludesToday(t *testing.T) {
	now := ms(2026, 8, 13, 12, 0)
	entries := []Entry{{Text: "x", At: ms(2026, 8, 10, 9, 0)}}
	pages := dayPages(entries, now)
	if len(pages) != 2 || pages[1].Key != "2026-08-13" {
		t.Fatalf("pages = %+v", pages)
	}
	if len(pages[1].Entries) != 0 {
		t.Errorf("today should be empty")
	}
}

func TestExportShapes(t *testing.T) {
	entries := []Entry{
		{Text: "coffee", At: ms(2026, 8, 12, 9, 5)},
		{Text: "standup", At: ms(2026, 8, 13, 10, 16)},
	}
	d := dayText(entries[1:], false)
	if d != "[10:16 AM] standup" {
		t.Errorf("dayText = %q", d)
	}
	h := historyText(entries, false)
	want := "2026-08-12\n\n[09:05 AM] coffee\n\n2026-08-13\n\n[10:16 AM] standup"
	if h != want {
		t.Errorf("historyText = %q", h)
	}
	if strings.Contains(h, "Yesterday") {
		t.Errorf("history must use absolute dates")
	}
}
