package main

// Clock formatting, day bucketing, gap math, export text shapes.
// Pure — no TUI, no storage.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Entry is one logged line. At is epoch ms at creation and doubles as the
// identity — local-only, so a separate id would always mirror it. Two entries
// in the same millisecond get bumped a millisecond apart (see newAt).
type Entry struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

// Only gaps worth noticing get a marker — below this the timeline stays quiet.
const gapMin = 20 * time.Minute

func fmtTime(ms int64, use24h bool) string {
	t := time.UnixMilli(ms)
	if use24h {
		return t.Format("15:04")
	}
	return t.Format("03:04 PM")
}

func dayKey(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02")
}

func dayLabel(ms, now int64) string {
	k := dayKey(ms)
	if k == dayKey(now) {
		return "Today"
	}
	// calendar yesterday, not now-24h: across a DST transition the day is
	// 23 or 25 hours long and fixed arithmetic lands on the wrong date
	if k == dayKey(time.UnixMilli(now).AddDate(0, 0, -1).UnixMilli()) {
		return "Yesterday"
	}
	t := time.UnixMilli(ms)
	if t.Year() == time.UnixMilli(now).Year() {
		return t.Format("Mon, Jan 2")
	}
	return t.Format("Mon, Jan 2, 2006")
}

// gapLabel returns "" for gaps under the threshold.
func gapLabel(d time.Duration) string {
	if d < gapMin {
		return ""
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	h, m := mins/60, mins%60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

type day struct {
	Key     string
	Entries []Entry
}

// groupByDay returns oldest-day-first, oldest-entry-first inside each day —
// the timeline renders top-down with the newest at the bottom.
func groupByDay(entries []Entry) []day {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At < sorted[j].At })
	var days []day
	idx := map[string]int{}
	for _, e := range sorted {
		k := dayKey(e.At)
		i, ok := idx[k]
		if !ok {
			i = len(days)
			idx[k] = i
			days = append(days, day{Key: k})
		}
		days[i].Entries = append(days[i].Entries, e)
	}
	return days
}

// dayPages: every day that has entries, plus today — today is always
// reachable because that's where the composer writes.
func dayPages(entries []Entry, now int64) []day {
	days := groupByDay(entries)
	today := dayKey(now)
	found := false
	for _, d := range days {
		if d.Key == today {
			found = true
			break
		}
	}
	if !found {
		days = append(days, day{Key: today})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Key < days[j].Key })
	return days
}

// dayText: one "[time] text" line per entry, so a pasted day reads like the
// timeline rather than a wall of undated text.
func dayText(entries []Entry, use24h bool) string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = fmt.Sprintf("[%s] %s", fmtTime(e.At, use24h), e.Text)
	}
	return strings.Join(lines, "\n")
}

// historyText: a date header over each day, oldest day first. Absolute dates,
// not relative labels — "Yesterday" is meaningless in a file read next month.
func historyText(entries []Entry, use24h bool) string {
	days := groupByDay(entries)
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = d.Key + "\n\n" + dayText(d.Entries, use24h)
	}
	return strings.Join(parts, "\n\n")
}
