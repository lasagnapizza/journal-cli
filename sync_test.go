package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helpers: a minute-resolution At the way a reload produces it, and a
// full-ms cloud at inside the same minute.
const (
	min1 = int64(1_700_000_040_000) // some exact minute
	min2 = min1 + 60_000
)

func ids(r mergeResult) map[int64]bool {
	out := map[int64]bool{}
	for _, row := range r.rows {
		out[row.ID] = true
	}
	return out
}

func TestMergeFirstSyncUploadsLocal(t *testing.T) {
	local := []Entry{{Text: "coffee", At: min1}}
	r := mergeSync(local, nil, nil, nil, min2)
	if len(r.upserts) != 1 || r.upserts[0].ID != min1 || r.upserts[0].Text != "coffee" {
		t.Fatalf("upserts = %+v", r.upserts)
	}
	if len(r.entries) != 1 || len(r.deletes) != 0 {
		t.Fatalf("entries = %+v deletes = %+v", r.entries, r.deletes)
	}
}

func TestMergeDownloadsRemoteOnly(t *testing.T) {
	remote := []remoteRow{{ID: min1 + 33_123, At: min1 + 33_123, Text: "from phone"}}
	r := mergeSync(nil, nil, nil, remote, min2)
	if len(r.entries) != 1 || r.entries[0].Text != "from phone" || r.entries[0].At != min1+33_123 {
		t.Fatalf("entries = %+v", r.entries)
	}
	if len(r.upserts) != 0 {
		t.Fatalf("nothing to upload, got %+v", r.upserts)
	}
	if r.idByAt[min1+33_123] != min1+33_123 {
		t.Fatalf("idByAt = %+v", r.idByAt)
	}
}

// A reloaded entry (minute resolution) must re-match its base row (full ms)
// and not upload a duplicate.
func TestMergeRematchAfterReload(t *testing.T) {
	cloudAt := min1 + 33_123
	base := []syncRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	remote := []remoteRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	local := []Entry{{Text: "coffee", At: min1}} // what the day file reloads as
	r := mergeSync(local, base, nil, remote, min2)
	if len(r.upserts) != 0 || len(r.deletes) != 0 {
		t.Fatalf("clean rematch should push nothing: up=%+v del=%+v", r.upserts, r.deletes)
	}
	if len(r.entries) != 1 || r.entries[0].At != cloudAt {
		t.Fatalf("should adopt the cloud at: %+v", r.entries)
	}
}

// Same minute, different text = a local edit (in the app or in $EDITOR):
// keep the id, push the new text.
func TestMergeLocalEditPushes(t *testing.T) {
	cloudAt := min1 + 33_123
	base := []syncRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	remote := []remoteRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	local := []Entry{{Text: "coffee and toast", At: min1}}
	r := mergeSync(local, base, nil, remote, min2)
	if len(r.upserts) != 1 || r.upserts[0].ID != cloudAt || r.upserts[0].Text != "coffee and toast" {
		t.Fatalf("upserts = %+v", r.upserts)
	}
	if len(r.entries) != 1 || r.entries[0].Text != "coffee and toast" {
		t.Fatalf("entries = %+v", r.entries)
	}
}

func TestMergeLocalDeletePushesTombstone(t *testing.T) {
	cloudAt := min1 + 33_123
	base := []syncRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	remote := []remoteRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	r := mergeSync(nil, base, nil, remote, min2) // the line is gone from the files
	if len(r.deletes) != 1 || r.deletes[0].ID != cloudAt {
		t.Fatalf("deletes = %+v", r.deletes)
	}
	if len(r.entries) != 0 {
		t.Fatalf("entries = %+v", r.entries)
	}
	if _, ok := r.tombs[cloudAt]; !ok {
		t.Fatal("expected a tombstone")
	}
}

// Deleted on another device: drop it here, don't re-upload it, tombstone it.
func TestMergeRemoteDeleteWins(t *testing.T) {
	cloudAt := min1 + 33_123
	del := min2
	base := []syncRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	remote := []remoteRow{{ID: cloudAt, At: cloudAt, Text: "coffee", DeletedAt: &del}}
	local := []Entry{{Text: "coffee", At: min1}}
	r := mergeSync(local, base, nil, remote, min2)
	if len(r.entries) != 0 || len(r.upserts) != 0 {
		t.Fatalf("entries=%+v upserts=%+v", r.entries, r.upserts)
	}
	if r.tombs[cloudAt] != del {
		t.Fatalf("tombs = %+v", r.tombs)
	}
}

// A tombstoned id must not come back on the next pull.
func TestMergeTombstoneBlocksResurrection(t *testing.T) {
	cloudAt := min1 + 33_123
	remote := []remoteRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	tombs := map[int64]int64{cloudAt: min1}
	r := mergeSync(nil, nil, tombs, remote, min2)
	if len(r.entries) != 0 {
		t.Fatalf("resurrected: %+v", r.entries)
	}
}

func TestMergeRemoteEditAdopted(t *testing.T) {
	cloudAt := min1 + 33_123
	base := []syncRow{{ID: cloudAt, At: cloudAt, Text: "coffee"}}
	remote := []remoteRow{{ID: cloudAt, At: cloudAt, Text: "espresso"}}
	local := []Entry{{Text: "coffee", At: min1}}
	r := mergeSync(local, base, nil, remote, min2)
	if len(r.entries) != 1 || r.entries[0].Text != "espresso" {
		t.Fatalf("entries = %+v", r.entries)
	}
	if len(r.upserts) != 0 {
		t.Fatalf("nothing to push: %+v", r.upserts)
	}
}

func TestMergeTombstoneTTLExpires(t *testing.T) {
	old := map[int64]int64{1: min1}
	r := mergeSync(nil, nil, old, nil, min1+tombTTLMs+1)
	if len(r.tombs) != 0 {
		t.Fatalf("tombs = %+v", r.tombs)
	}
}

func TestMergeSameMsIDCollisionBumps(t *testing.T) {
	// a remote row and a brand-new local entry land on the same ms
	remote := []remoteRow{{ID: min1, At: min1, Text: "remote"}}
	local := []Entry{{Text: "local", At: min1}}
	// no base: the local one is new; base matching must not eat the remote row
	r := mergeSync(local, nil, nil, remote, min2)
	if len(r.entries) != 2 {
		t.Fatalf("entries = %+v", r.entries)
	}
	if !ids(r)[min1] || len(ids(r)) != 2 {
		t.Fatalf("rows = %+v", r.rows)
	}
}

func TestMergeTwoSameMinuteEntriesRematch(t *testing.T) {
	// two same-minute cloud rows reload as two bumped local lines and must
	// pair off without pushing anything
	a, b := min1+10_000, min1+20_000
	base := []syncRow{{ID: a, At: a, Text: "one"}, {ID: b, At: b, Text: "two"}}
	remote := []remoteRow{{ID: a, At: a, Text: "one"}, {ID: b, At: b, Text: "two"}}
	local := []Entry{{Text: "one", At: min1}, {Text: "two", At: min1 + 1}}
	r := mergeSync(local, base, nil, remote, min2)
	if len(r.upserts) != 0 || len(r.deletes) != 0 || len(r.entries) != 2 {
		t.Fatalf("up=%+v del=%+v entries=%+v", r.upserts, r.deletes, r.entries)
	}
}

// End to end against a fake Supabase: a signed-in device with one local
// entry pulls a remote one, pushes its own, and rewrites the day files.
func TestSyncStartupRoundTrip(t *testing.T) {
	dir := withTempStore(t)
	sy.on = false
	sy.state = syncState{}
	sy.idByAt = nil
	sy.client = nil
	t.Cleanup(func() { sy.on = false; sy.state = syncState{}; sy.idByAt = nil; sy.client = nil; sy.sess = syncSession{} })

	remoteAt := time.Now().Add(-24 * time.Hour).UnixMilli()
	remoteAt -= remoteAt % 60000
	remoteAt += 33_123 // full-ms cloud timestamp, so the re-match is exercised
	// a stateful stand-in for PostgREST: upserts land in serverRows so the
	// next GET returns them, like the real table would
	serverRows := []remoteRow{{ID: remoteAt, At: remoteAt, Text: "from phone"}}
	var pushed [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/v1/"+syncTable {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(serverRows)
		case http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			pushed = append(pushed, b)
			var rows []remoteRow
			if err := json.Unmarshal(b, &rows); err != nil {
				t.Errorf("bad upsert body %s", b)
			}
			serverRows = append(serverRows, rows...)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	sy.sess = syncSession{URL: srv.URL, AnonKey: "k", UserID: "u",
		AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Unix() + 3600}
	if err := saveSession(); err != nil {
		t.Fatal(err)
	}

	localAt := time.Now().UnixMilli() - time.Now().UnixMilli()%60000
	local := []Entry{{Text: "typed here", At: localAt}}
	merged, err := syncStartup(local, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed = %d bodies", len(pushed))
	}
	var rows []map[string]any
	if err := json.Unmarshal(pushed[0], &rows); err != nil || len(rows) != 1 {
		t.Fatalf("push body %s", pushed[0])
	}
	if rows[0]["text"] != "typed here" || rows[0]["user_id"] != "u" {
		t.Fatalf("push body %s", pushed[0])
	}
	// both entries landed in the day files
	files, _ := filepath.Glob(filepath.Join(dir, "*.txt"))
	if len(files) != 2 {
		t.Fatalf("day files = %v", files)
	}
	// state remembers both rows for the next re-match
	data, _ := os.ReadFile(filepath.Join(dir, stateFile))
	var st syncState
	if err := json.Unmarshal(data, &st); err != nil || len(st.Rows) != 2 || len(st.Pending) != 0 {
		t.Fatalf("state = %s", data)
	}
	// a second run with the rewritten files pushes nothing new
	pushed = nil
	reloaded, err := loadEntries()
	if err != nil {
		t.Fatal(err)
	}
	sy.state = syncState{}
	merged2, err := syncStartup(reloaded, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged2) != 2 || len(pushed) != 0 {
		t.Fatalf("second run: merged=%d pushed=%d", len(merged2), len(pushed))
	}
}
