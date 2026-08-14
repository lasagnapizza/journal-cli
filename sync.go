package main

// Optional Supabase sync — the same backend, table, and merge rules as the
// journal web app, so one account sees one timeline everywhere. Pure REST
// over net/http (PostgREST + GoTrue); no SDK. Never touched unless the user
// runs `journal login`: with no session on disk every function here is a
// no-op and the app stays fully local.
//
// Identity: the cloud row id is a client-generated epoch-ms value, exactly
// the shape of Entry.At — a fresh local entry uploads with id = At. But day
// files only keep minute resolution, so after a restart a synced entry's At
// no longer equals its cloud id. syncstate.json remembers the cloud rows as
// of the last sync (id, full-ms at, text); on the next sync those rows are
// re-matched to the reloaded entries by (minute, text) — a text mismatch in
// the same minute is a local edit, a vanished line is a local delete. That
// also makes hand-editing the day files in $EDITOR sync correctly.
//
// Deletes are soft (deleted_at) and remembered locally as tombstones with a
// TTL, mirroring the web app — a pull can never resurrect an entry either
// side deleted. Pushes that fail (offline) queue in the state file and flush
// on the next attempt.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	syncTable   = "journal_entries"
	tombTTLMs   = 90 * 24 * 60 * 60 * 1000
	sessionFile = "session.json"
	stateFile   = "syncstate.json"
)

type syncSession struct {
	URL          string `json:"url"`
	AnonKey      string `json:"anon_key"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // epoch seconds
}

// syncRow is a cloud row as this device last saw it — the merge base.
type syncRow struct {
	ID   int64  `json:"id"`
	At   int64  `json:"at"`
	Text string `json:"text"`
}

type remoteRow struct {
	ID        int64  `json:"id"`
	At        int64  `json:"at"`
	Text      string `json:"text"`
	DeletedAt *int64 `json:"deleted_at"`
}

type pendingOp struct {
	Op        string `json:"op"` // "upsert" | "delete"
	ID        int64  `json:"id"`
	At        int64  `json:"at,omitempty"`
	Text      string `json:"text,omitempty"`
	DeletedAt int64  `json:"deleted_at,omitempty"`
}

type syncState struct {
	Rows    []syncRow        `json:"rows"`
	Tombs   map[string]int64 `json:"tombs"` // id (string: JSON keys) -> deleted_at ms
	Pending []pendingOp      `json:"pending"`
}

// sy is the process-wide sync handle. on stays false without a session, and
// every hook checks it first — the local-only path never pays for sync.
var sy struct {
	mu     sync.Mutex
	on     bool
	sess   syncSession
	state  syncState
	idByAt map[int64]int64 // in-memory At -> cloud id, valid for this run
	client *http.Client
}

func syncPath(name string) (string, error) {
	dir, err := storeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func loadSession() (bool, error) {
	path, err := syncPath(sessionFile)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, &sy.sess); err != nil {
		return false, err
	}
	return sy.sess.URL != "" && sy.sess.RefreshToken != "", nil
}

// saveSession writes 0600 — the refresh token is a credential.
func saveSession() error {
	path, err := syncPath(sessionFile)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(sy.sess, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

func loadSyncState() {
	sy.state = syncState{Tombs: map[string]int64{}}
	path, err := syncPath(stateFile)
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &sy.state)
	if sy.state.Tombs == nil {
		sy.state.Tombs = map[string]int64{}
	}
}

func saveSyncState() {
	path, err := syncPath(stateFile)
	if err != nil {
		return
	}
	data, _ := json.Marshal(sy.state)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// ---- HTTP ---------------------------------------------------------------

func httpClient() *http.Client {
	if sy.client == nil {
		sy.client = &http.Client{Timeout: 10 * time.Second}
	}
	return sy.client
}

type authResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func (a *authResponse) apply() {
	sy.sess.AccessToken = a.AccessToken
	sy.sess.RefreshToken = a.RefreshToken
	if a.ExpiresAt != 0 {
		sy.sess.ExpiresAt = a.ExpiresAt
	} else {
		sy.sess.ExpiresAt = time.Now().Unix() + a.ExpiresIn
	}
	if a.User.ID != "" {
		sy.sess.UserID = a.User.ID
		sy.sess.Email = a.User.Email
	}
}

// sbCall hits {url}{path} with the anon key and, when bearer is true, the
// user's access token. Non-2xx comes back as an error carrying the body —
// Supabase puts the useful message there.
func sbCall(method, path string, headers map[string]string, body, out any, bearer bool) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, sy.sess.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", sy.sess.AnonKey)
	req.Header.Set("Content-Type", "application/json")
	if bearer {
		req.Header.Set("Authorization", "Bearer "+sy.sess.AccessToken)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// refreshIfNeeded rotates the session before the access token expires.
// Supabase revokes a refresh token family on reuse, so the rotated token is
// persisted immediately — losing it silently signs the device out.
func refreshIfNeeded() error {
	if time.Now().Unix() < sy.sess.ExpiresAt-60 {
		return nil
	}
	var out authResponse
	err := sbCall("POST", "/auth/v1/token?grant_type=refresh_token", nil,
		map[string]string{"refresh_token": sy.sess.RefreshToken}, &out, false)
	if err != nil {
		return err
	}
	out.apply()
	return saveSession()
}

func restPath(query string) string {
	return "/rest/v1/" + syncTable + query
}

func fetchRemote() ([]remoteRow, error) {
	var rows []remoteRow
	err := sbCall("GET", restPath("?select=id,text,at,deleted_at"), nil, nil, &rows, true)
	return rows, err
}

func pushUpserts(rows []syncRow) error {
	if len(rows) == 0 {
		return nil
	}
	type up struct {
		ID     int64  `json:"id"`
		UserID string `json:"user_id"`
		Text   string `json:"text"`
		At     int64  `json:"at"`
	}
	body := make([]up, len(rows))
	for i, r := range rows {
		body[i] = up{ID: r.ID, UserID: sy.sess.UserID, Text: r.Text, At: r.At}
	}
	return sbCall("POST", restPath(""), map[string]string{
		"Prefer": "resolution=merge-duplicates,return=minimal",
	}, body, nil, true)
}

func pushDelete(id, deletedAt int64) error {
	q := fmt.Sprintf("?id=eq.%d&user_id=eq.%s", id, url.QueryEscape(sy.sess.UserID))
	return sbCall("PATCH", restPath(q), map[string]string{"Prefer": "return=minimal"},
		map[string]int64{"deleted_at": deletedAt}, nil, true)
}

// ---- merge --------------------------------------------------------------

type mergeResult struct {
	entries []Entry
	upserts []syncRow   // local truth the cloud is missing
	deletes []pendingOp // soft-deletes to push
	rows    []syncRow   // the new base
	tombs   map[int64]int64
	idByAt  map[int64]int64
}

func minuteOf(ms int64) int64 { return ms - ms%60000 }

// mergeSync is the whole sync brain, pure so it can be tested without a
// server: reload-matched local entries vs the last-sync base vs the cloud.
// Remote deletes win over local edits (same rule as the web app); a local
// edit wins over a remote edit because the local one is pushed last.
func mergeSync(local []Entry, base []syncRow, tombs map[int64]int64, remote []remoteRow, now int64) mergeResult {
	remoteActive := map[int64]remoteRow{}
	remoteDeleted := map[int64]int64{}
	for _, r := range remote {
		if r.DeletedAt != nil {
			remoteDeleted[r.ID] = *r.DeletedAt
		} else {
			remoteActive[r.ID] = r
		}
	}

	// Match base rows back to the reloaded entries: exact (minute, text)
	// first, then minute-only for local edits, leftovers are local deletes.
	used := make([]bool, len(local))
	byExact := map[string][]int{}
	byMinute := map[int64][]int{}
	for i, e := range local {
		k := fmt.Sprintf("%d|%s", minuteOf(e.At), e.Text)
		byExact[k] = append(byExact[k], i)
		byMinute[minuteOf(e.At)] = append(byMinute[minuteOf(e.At)], i)
	}
	take := func(idxs []int) int {
		for _, i := range idxs {
			if !used[i] {
				used[i] = true
				return i
			}
		}
		return -1
	}
	matched := make([]int, len(base)) // base idx -> local idx or -1
	for bi, b := range base {
		matched[bi] = take(byExact[fmt.Sprintf("%d|%s", minuteOf(b.At), b.Text)])
	}
	for bi, b := range base {
		if matched[bi] < 0 {
			matched[bi] = take(byMinute[minuteOf(b.At)]) // same minute, new text: an edit
		}
	}

	newTombs := map[int64]int64{}
	for id, ts := range tombs {
		if now-ts <= tombTTLMs {
			newTombs[id] = ts
		}
	}
	res := mergeResult{tombs: newTombs, idByAt: map[int64]int64{}}
	type pair struct {
		e  Entry
		id int64
	}
	var pairs []pair
	usedID := map[int64]bool{}

	for bi, b := range base {
		li := matched[bi]
		if li < 0 { // gone from the files: deleted locally
			res.tombs[b.ID] = now
			if _, ok := remoteActive[b.ID]; ok {
				res.deletes = append(res.deletes, pendingOp{Op: "delete", ID: b.ID, DeletedAt: now})
			}
			continue
		}
		if ts, ok := remoteDeleted[b.ID]; ok { // deleted elsewhere: drop it here
			res.tombs[b.ID] = ts
			continue
		}
		text := local[li].Text
		if text != b.Text { // edited locally — push it
			res.upserts = append(res.upserts, syncRow{ID: b.ID, At: b.At, Text: text})
		} else if r, ok := remoteActive[b.ID]; ok && r.Text != b.Text {
			text = r.Text // edited elsewhere — adopt it
		} else if _, ok := remoteActive[b.ID]; !ok {
			// the cloud never got it (a push that failed) — send it again
			res.upserts = append(res.upserts, syncRow{ID: b.ID, At: b.At, Text: text})
		}
		row := syncRow{ID: b.ID, At: b.At, Text: text}
		res.rows = append(res.rows, row)
		pairs = append(pairs, pair{Entry{Text: text, At: b.At}, b.ID})
		usedID[b.ID] = true
	}

	// Entries with no base row are new on this device: id = At, bumped past
	// every id the cloud already owns — colliding with one would silently
	// overwrite another device's row instead of inserting.
	taken := map[int64]bool{}
	for id := range usedID {
		taken[id] = true
	}
	for _, r := range remote {
		taken[r.ID] = true
	}
	for i, e := range local {
		if used[i] {
			continue
		}
		id := e.At
		for taken[id] {
			id++
		}
		taken[id] = true
		usedID[id] = true
		row := syncRow{ID: id, At: id, Text: e.Text}
		res.rows = append(res.rows, row)
		res.upserts = append(res.upserts, row)
		pairs = append(pairs, pair{Entry{Text: e.Text, At: id}, id})
	}

	// Cloud rows this device has never seen — unless it buried them.
	var downloadIDs []int64
	for id := range remoteActive {
		if !usedID[id] {
			if _, dead := res.tombs[id]; !dead {
				downloadIDs = append(downloadIDs, id)
			}
		}
	}
	sort.Slice(downloadIDs, func(i, j int) bool { return downloadIDs[i] < downloadIDs[j] })
	for _, id := range downloadIDs {
		r := remoteActive[id]
		res.rows = append(res.rows, syncRow{ID: r.ID, At: r.At, Text: r.Text})
		pairs = append(pairs, pair{Entry{Text: r.Text, At: r.At}, r.ID})
	}

	// At stays the in-memory identity, so collisions bump exactly like a
	// load from disk — and the id map is built from the bumped values.
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].e.At < pairs[j].e.At })
	seen := map[int64]bool{}
	for i := range pairs {
		for seen[pairs[i].e.At] {
			pairs[i].e.At++
		}
		seen[pairs[i].e.At] = true
		res.entries = append(res.entries, pairs[i].e)
		res.idByAt[pairs[i].e.At] = pairs[i].id
	}
	return res
}

// ---- reconcile ----------------------------------------------------------

func tombsFromState() map[int64]int64 {
	out := map[int64]int64{}
	for k, v := range sy.state.Tombs {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			out[id] = v
		}
	}
	return out
}

func tombsToState(t map[int64]int64) map[string]int64 {
	out := map[string]int64{}
	for id, v := range t {
		out[strconv.FormatInt(id, 10)] = v
	}
	return out
}

// flushPending replays queued ops in order; the queue keeps whatever fails.
func flushPending() error {
	var kept []pendingOp
	var firstErr error
	for i, op := range sy.state.Pending {
		if firstErr != nil {
			kept = append(kept, sy.state.Pending[i:]...)
			break
		}
		var err error
		if op.Op == "delete" {
			err = pushDelete(op.ID, op.DeletedAt)
		} else {
			err = pushUpserts([]syncRow{{ID: op.ID, At: op.At, Text: op.Text}})
		}
		if err != nil {
			firstErr = err
			kept = append(kept, op)
		}
	}
	sy.state.Pending = kept
	saveSyncState()
	return firstErr
}

// syncStartup is the once-per-run reconcile: flush the queue, pull, merge,
// push, rewrite the day files. Returns the entries the TUI should show. On
// any failure it returns local unchanged — the journal must open regardless,
// and the queue catches up next time.
func syncStartup(local []Entry, use24h bool) ([]Entry, error) {
	ok, err := loadSession()
	if err != nil || !ok {
		return local, err
	}
	loadSyncState()
	if err := refreshIfNeeded(); err != nil {
		return local, err
	}
	sy.on = true // live hooks may queue even if this pull fails
	if err := flushPending(); err != nil {
		return local, err
	}
	remote, err := fetchRemote()
	if err != nil {
		return local, err
	}
	res := mergeSync(local, sy.state.Rows, tombsFromState(), remote, time.Now().UnixMilli())

	// Push, queueing anything that fails so no edit is ever dropped.
	if err := pushUpserts(res.upserts); err != nil {
		for _, r := range res.upserts {
			sy.state.Pending = append(sy.state.Pending, pendingOp{Op: "upsert", ID: r.ID, At: r.At, Text: r.Text})
		}
	}
	for _, d := range res.deletes {
		if err := pushDelete(d.ID, d.DeletedAt); err != nil {
			sy.state.Pending = append(sy.state.Pending, d)
		}
	}

	sy.state.Rows = res.rows
	sy.state.Tombs = tombsToState(res.tombs)
	saveSyncState()
	sy.idByAt = res.idByAt
	if err := saveEntries(res.entries, use24h); err != nil {
		return local, err
	}
	return res.entries, nil
}

// ---- live hooks (called from the TUI on each mutation) ------------------

// queueOp records the op and tries to push it right away, off the UI thread.
func queueOp(op pendingOp) {
	sy.mu.Lock()
	sy.state.Pending = append(sy.state.Pending, op)
	saveSyncState()
	sy.mu.Unlock()
	go func() {
		sy.mu.Lock()
		defer sy.mu.Unlock()
		if refreshIfNeeded() != nil {
			return
		}
		_ = flushPending()
	}()
}

func syncLogged(at int64, text string) {
	if !sy.on {
		return
	}
	sy.mu.Lock()
	if sy.idByAt == nil {
		sy.idByAt = map[int64]int64{}
	}
	sy.idByAt[at] = at
	sy.state.Rows = append(sy.state.Rows, syncRow{ID: at, At: at, Text: text})
	sy.mu.Unlock()
	queueOp(pendingOp{Op: "upsert", ID: at, At: at, Text: text})
}

func syncEdited(at int64, text string) {
	if !sy.on {
		return
	}
	sy.mu.Lock()
	id, ok := sy.idByAt[at]
	if !ok {
		id = at
	}
	rowAt := at
	for i := range sy.state.Rows {
		if sy.state.Rows[i].ID == id {
			sy.state.Rows[i].Text = text
			rowAt = sy.state.Rows[i].At
			break
		}
	}
	sy.mu.Unlock()
	queueOp(pendingOp{Op: "upsert", ID: id, At: rowAt, Text: text})
}

func syncDeleted(at int64) {
	if !sy.on {
		return
	}
	now := time.Now().UnixMilli()
	sy.mu.Lock()
	id, ok := sy.idByAt[at]
	if !ok {
		id = at
	}
	delete(sy.idByAt, at)
	kept := sy.state.Rows[:0]
	for _, r := range sy.state.Rows {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	sy.state.Rows = kept
	sy.state.Tombs[strconv.FormatInt(id, 10)] = now
	sy.mu.Unlock()
	queueOp(pendingOp{Op: "delete", ID: id, DeletedAt: now})
}

// ---- commands -----------------------------------------------------------

func readLine(label string) (string, error) {
	fmt.Print(label)
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF && b.Len() > 0 {
				break
			}
			return "", err
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// cmdLogin: magic-link OTP over plain REST — no browser round-trip. The
// user pastes either the 6-digit code (if the email template shows one) or
// the whole magic link; the link's token param is a token_hash the verify
// endpoint accepts directly.
func cmdLogin(use24h bool) error {
	if _, err := loadSession(); err != nil {
		return err
	}
	if v := os.Getenv("JOURNAL_SUPABASE_URL"); v != "" {
		sy.sess.URL = v
	}
	if v := os.Getenv("JOURNAL_SUPABASE_ANON_KEY"); v != "" {
		sy.sess.AnonKey = v
	}
	var err error
	if sy.sess.URL == "" {
		if sy.sess.URL, err = readLine("Supabase URL (the web app's VITE_SUPABASE_URL): "); err != nil {
			return err
		}
	}
	if sy.sess.AnonKey == "" {
		if sy.sess.AnonKey, err = readLine("Anon key (the web app's VITE_SUPABASE_ANON_KEY): "); err != nil {
			return err
		}
	}
	sy.sess.URL = strings.TrimRight(sy.sess.URL, "/")
	email, err := readLine("Email: ")
	if err != nil {
		return err
	}
	err = sbCall("POST", "/auth/v1/otp", nil,
		map[string]any{"email": email, "create_user": true}, nil, false)
	if err != nil {
		return err
	}
	fmt.Println("Check your email. Paste the 6-digit code, or the whole magic link.")
	code, err := readLine("> ")
	if err != nil {
		return err
	}
	body := map[string]string{"type": "email", "email": email, "token": code}
	if u, uerr := url.Parse(code); uerr == nil && u.Scheme != "" {
		q := u.Query()
		th := q.Get("token_hash")
		if th == "" {
			th = q.Get("token")
		}
		if th == "" {
			return errors.New("that link has no token in it")
		}
		body = map[string]string{"type": "email", "token_hash": th}
	}
	var out authResponse
	if err := sbCall("POST", "/auth/v1/verify", nil, body, &out, false); err != nil {
		return err
	}
	out.apply()
	if err := saveSession(); err != nil {
		return err
	}
	fmt.Printf("Signed in as %s. Syncing…\n", sy.sess.Email)
	local, err := loadEntries()
	if err != nil {
		return err
	}
	merged, err := syncStartup(local, use24h)
	if err != nil {
		return err
	}
	fmt.Printf("Synced: %d entries.\n", len(merged))
	return nil
}

func cmdLogout() error {
	for _, name := range []string{sessionFile, stateFile} {
		path, err := syncPath(name)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	fmt.Println("Signed out. Local entries kept; run `journal login` to sync again.")
	return nil
}

func cmdSync(use24h bool) error {
	ok, err := loadSession()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not signed in — run `journal login` first")
	}
	local, err := loadEntries()
	if err != nil {
		return err
	}
	merged, err := syncStartup(local, use24h)
	if err != nil {
		return err
	}
	fmt.Printf("Synced: %d entries as %s.\n", len(merged), sy.sess.Email)
	return nil
}
