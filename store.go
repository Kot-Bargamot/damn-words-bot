package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// В схеме НЕТ полей для исходного текста сообщений: только идентификаторы и ссылки.
const schema = `
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS message_refs (
  id INTEGER PRIMARY KEY, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
  source_url TEXT, material_type TEXT NOT NULL, created_at INTEGER NOT NULL,
  UNIQUE(chat_id, message_id));
CREATE TABLE IF NOT EXISTS lexical_items (
  id INTEGER PRIMARY KEY, canonical_form TEXT NOT NULL, form_key TEXT NOT NULL,
  meaning_ru TEXT NOT NULL, usage_note TEXT NOT NULL DEFAULT '', example TEXT NOT NULL DEFAULT '',
  example_origin TEXT NOT NULL, tags TEXT NOT NULL DEFAULT '[]', ambiguous INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS lex_form ON lexical_items(form_key);
CREATE TABLE IF NOT EXISTS lexical_item_sources (
  lexical_item_id INTEGER NOT NULL, message_ref_id INTEGER NOT NULL,
  PRIMARY KEY(lexical_item_id, message_ref_id));
CREATE TABLE IF NOT EXISTS add_batches (
  batch_id TEXT NOT NULL, lexical_item_id INTEGER NOT NULL, created_new INTEGER NOT NULL,
  message_ref_id INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS grammar_topics (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, repository_path TEXT NOT NULL,
  points_json TEXT NOT NULL DEFAULT '[]', content_markdown TEXT NOT NULL DEFAULT '',
  repository_revision TEXT NOT NULL DEFAULT '', sync_status TEXT NOT NULL DEFAULT 'pending',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS grammar_topic_sources (
  grammar_topic_id TEXT NOT NULL, message_ref_id INTEGER NOT NULL,
  PRIMARY KEY(grammar_topic_id, message_ref_id));
CREATE TABLE IF NOT EXISTS pending_grammar (
  id INTEGER PRIMARY KEY, topic_id TEXT NOT NULL, title TEXT NOT NULL, proposal_json TEXT NOT NULL,
  chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, source_url TEXT, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY, status TEXT NOT NULL, current_step INTEGER NOT NULL DEFAULT 0,
  plan_json TEXT NOT NULL, current_exercise_json TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL, finished_at INTEGER);
CREATE TABLE IF NOT EXISTS attempts (
  id INTEGER PRIMARY KEY, session_id INTEGER NOT NULL, message_ref_id INTEGER,
  exercise_type TEXT NOT NULL, attempt_no INTEGER NOT NULL, result TEXT NOT NULL,
  grammar_result TEXT NOT NULL DEFAULT '', used_hint INTEGER NOT NULL DEFAULT 0,
  meaning_ok INTEGER, target_produced INTEGER, form_ok INTEGER, grammar_ok TEXT,
  confidence TEXT, error_codes TEXT NOT NULL DEFAULT '[]', correction TEXT NOT NULL DEFAULT '',
  spontaneous_usage INTEGER NOT NULL DEFAULT 0, srs_snapshot TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS attempt_targets (
  attempt_id INTEGER NOT NULL, target_type TEXT NOT NULL, target_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS review_state (
  target_type TEXT NOT NULL, target_id TEXT NOT NULL, stage INTEGER NOT NULL DEFAULT -1,
  next_due_at INTEGER NOT NULL, last_reviewed_at INTEGER NOT NULL DEFAULT 0,
  independent_count INTEGER NOT NULL DEFAULT 0, hinted_count INTEGER NOT NULL DEFAULT 0,
  failed_count INTEGER NOT NULL DEFAULT 0, spontaneous_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(target_type, target_id));
CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY, type TEXT NOT NULL, derived_payload_json TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending', retry_count INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL, last_error_code TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
`

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("миграция: %w", err)
	}
	return &Store{db: db}, nil
}

func now() int64 { return time.Now().Unix() }

func (s *Store) KVGet(k string) string {
	var v string
	_ = s.db.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	return v
}

func (s *Store) KVSet(k, v string) {
	_, _ = s.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
}

// ---------- message refs ----------

type MsgRef struct {
	ID        int64
	ChatID    int64
	MessageID int64
	URL       string
	Type      string
}

func (s *Store) RefExists(chatID, msgID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM message_refs WHERE chat_id=? AND message_id=? AND material_type IN ('lexicon','grammar')`, chatID, msgID).Scan(&n)
	return n > 0
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func insertRef(x execer, r MsgRef) (int64, error) {
	var url any
	if r.URL != "" {
		url = r.URL
	}
	_, err := x.Exec(`INSERT INTO message_refs(chat_id,message_id,source_url,material_type,created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(chat_id,message_id) DO NOTHING`, r.ChatID, r.MessageID, url, r.Type, now())
	if err != nil {
		return 0, err
	}
	var id int64
	err = x.QueryRow(`SELECT id FROM message_refs WHERE chat_id=? AND message_id=?`, r.ChatID, r.MessageID).Scan(&id)
	return id, err
}

func (s *Store) InsertRef(r MsgRef) (int64, error) { return insertRef(s.db, r) }

func (s *Store) Refs(ids []int64) map[int64]MsgRef {
	out := map[int64]MsgRef{}
	for _, id := range ids {
		var r MsgRef
		var url sql.NullString
		if s.db.QueryRow(`SELECT id,chat_id,message_id,source_url,material_type FROM message_refs WHERE id=?`, id).
			Scan(&r.ID, &r.ChatID, &r.MessageID, &url, &r.Type) == nil {
			r.URL = url.String
			out[id] = r
		}
	}
	return out
}

// ---------- lexicon ----------

type Item struct {
	ID            int64
	CanonicalForm string
	MeaningRU     string
	UsageNote     string
	Example       string
	ExampleOrigin string
	Tags          []string
	Ambiguous     bool
}

func formKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func meaningKey(s string) string {
	return strings.Trim(strings.ToLower(strings.Join(strings.Fields(s), " ")), " .,;")
}

type AddedItem struct {
	Item       Item
	CreatedNew bool
}

// AddLexicon сохраняет производные единицы и ссылку на источник одной транзакцией.
func (s *Store) AddLexicon(ref MsgRef, cands []LexCandidate, batch string) ([]AddedItem, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	refID, err := insertRef(tx, ref)
	if err != nil {
		return nil, err
	}
	t := now()
	var out []AddedItem
	seen := map[int64]bool{}
	for _, c := range cands {
		fk := formKey(c.CanonicalForm)
		var id int64
		created := false
		rows, err := tx.Query(`SELECT id, meaning_ru FROM lexical_items WHERE form_key=? AND status='active'`, fk)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eid int64
			var m string
			_ = rows.Scan(&eid, &m)
			// Объединяем только при совпадении значения; разные значения — разные единицы.
			if !c.Ambiguous && meaningKey(m) == meaningKey(c.MeaningRU) {
				id = eid
			}
		}
		rows.Close()
		if id == 0 {
			tags, _ := json.Marshal(c.Tags)
			res, err := tx.Exec(`INSERT INTO lexical_items(canonical_form,form_key,meaning_ru,usage_note,example,example_origin,tags,ambiguous,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, c.CanonicalForm, fk, c.MeaningRU, c.UsageNote, c.Example, c.ExampleOrigin, string(tags), b2i(c.Ambiguous), t, t)
			if err != nil {
				return nil, err
			}
			id, _ = res.LastInsertId()
			created = true
			if _, err := tx.Exec(`INSERT OR IGNORE INTO review_state(target_type,target_id,next_due_at) VALUES('lex',?,?)`, strconv.FormatInt(id, 10), t); err != nil {
				return nil, err
			}
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		res, err := tx.Exec(`INSERT OR IGNORE INTO lexical_item_sources(lexical_item_id,message_ref_id) VALUES(?,?)`, id, refID)
		if err != nil {
			return nil, err
		}
		linked, _ := res.RowsAffected()
		if created || linked > 0 {
			if _, err := tx.Exec(`INSERT INTO add_batches(batch_id,lexical_item_id,created_new,message_ref_id) VALUES(?,?,?,?)`, batch, id, b2i(created), refID); err != nil {
				return nil, err
			}
		}
		out = append(out, AddedItem{Item: Item{ID: id, CanonicalForm: c.CanonicalForm, MeaningRU: c.MeaningRU,
			UsageNote: c.UsageNote, Example: c.Example, ExampleOrigin: c.ExampleOrigin}, CreatedNew: created})
	}
	return out, tx.Commit()
}

// UndoBatch удаляет только данные этого добавления: новые единицы и новые связи с источником.
func (s *Store) UndoBatch(batch string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT lexical_item_id, created_new, message_ref_id FROM add_batches WHERE batch_id=?`, batch)
	if err != nil {
		return 0, err
	}
	type r struct {
		id, ref int64
		created bool
	}
	var list []r
	for rows.Next() {
		var x r
		var c int
		_ = rows.Scan(&x.id, &c, &x.ref)
		x.created = c == 1
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		if _, err := tx.Exec(`DELETE FROM lexical_item_sources WHERE lexical_item_id=? AND message_ref_id=?`, x.id, x.ref); err != nil {
			return 0, err
		}
		if x.created {
			sid := strconv.FormatInt(x.id, 10)
			for _, q := range []string{
				`DELETE FROM lexical_items WHERE id=?`,
				`DELETE FROM lexical_item_sources WHERE lexical_item_id=?`,
			} {
				if _, err := tx.Exec(q, x.id); err != nil {
					return 0, err
				}
			}
			if _, err := tx.Exec(`DELETE FROM review_state WHERE target_type='lex' AND target_id=?`, sid); err != nil {
				return 0, err
			}
		}
		_, _ = tx.Exec(`DELETE FROM message_refs WHERE id=? AND NOT EXISTS (SELECT 1 FROM lexical_item_sources WHERE message_ref_id=?)
			AND NOT EXISTS (SELECT 1 FROM grammar_topic_sources WHERE message_ref_id=?)`, x.ref, x.ref, x.ref)
	}
	if _, err := tx.Exec(`DELETE FROM add_batches WHERE batch_id=?`, batch); err != nil {
		return 0, err
	}
	return len(list), tx.Commit()
}

func (s *Store) BatchItems(batch string) []Item {
	rows, err := s.db.Query(`SELECT lexical_item_id FROM add_batches WHERE batch_id=?`, batch)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []Item
	for _, id := range ids {
		if it, err := s.Item(id); err == nil {
			out = append(out, *it)
		}
	}
	return out
}

func (s *Store) Item(id int64) (*Item, error) {
	var it Item
	var tags string
	var amb int
	err := s.db.QueryRow(`SELECT id,canonical_form,meaning_ru,usage_note,example,example_origin,tags,ambiguous FROM lexical_items WHERE id=?`, id).
		Scan(&it.ID, &it.CanonicalForm, &it.MeaningRU, &it.UsageNote, &it.Example, &it.ExampleOrigin, &tags, &amb)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tags), &it.Tags)
	it.Ambiguous = amb == 1
	return &it, nil
}

func (s *Store) ItemSources(id int64) []MsgRef {
	rows, err := s.db.Query(`SELECT message_ref_id FROM lexical_item_sources WHERE lexical_item_id=?`, id)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var x int64
		_ = rows.Scan(&x)
		ids = append(ids, x)
	}
	rows.Close()
	m := s.Refs(ids)
	var out []MsgRef
	for _, id := range ids {
		out = append(out, m[id])
	}
	return out
}

// ---------- grammar ----------

type GPoint struct {
	Section string  `json:"section"`
	Text    string  `json:"text"`
	Sources []int64 `json:"sources"`
}

type Topic struct {
	ID         string
	Title      string
	Path       string
	Points     []GPoint
	Markdown   string
	Revision   string
	SyncStatus string
}

func (s *Store) Topics() []Topic {
	rows, err := s.db.Query(`SELECT id FROM grammar_topics ORDER BY title`)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []Topic
	for _, id := range ids {
		if t, err := s.Topic(id); err == nil {
			out = append(out, *t)
		}
	}
	return out
}

func (s *Store) Topic(id string) (*Topic, error) {
	var t Topic
	var pj string
	err := s.db.QueryRow(`SELECT id,title,repository_path,points_json,content_markdown,repository_revision,sync_status FROM grammar_topics WHERE id=?`, id).
		Scan(&t.ID, &t.Title, &t.Path, &pj, &t.Markdown, &t.Revision, &t.SyncStatus)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(pj), &t.Points)
	return &t, nil
}

// SaveTopic сохраняет локальную версию темы и ставит задачу синхронизации.
func (s *Store) SaveTopic(t *Topic, refID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	pj, _ := json.Marshal(t.Points)
	_, err = tx.Exec(`INSERT INTO grammar_topics(id,title,repository_path,points_json,content_markdown,sync_status,created_at,updated_at)
		VALUES(?,?,?,?,?,'pending',?,?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, points_json=excluded.points_json,
		content_markdown=excluded.content_markdown, sync_status='pending', updated_at=excluded.updated_at`,
		t.ID, t.Title, t.Path, string(pj), t.Markdown, ts, ts)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO grammar_topic_sources(grammar_topic_id,message_ref_id) VALUES(?,?)`, t.ID, refID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO review_state(target_type,target_id,next_due_at) VALUES('gram',?,?)`, t.ID, ts); err != nil {
		return err
	}
	// Одна ожидающая задача на тему: она всегда берёт актуальную версию.
	var n int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='github_sync' AND status='pending' AND derived_payload_json=?`, mustJSON(map[string]string{"topic_id": t.ID})).Scan(&n)
	if n == 0 {
		if _, err := tx.Exec(`INSERT INTO jobs(type,derived_payload_json,next_attempt_at,created_at) VALUES('github_sync',?,?,?)`,
			mustJSON(map[string]string{"topic_id": t.ID}), ts, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetTopicSync(id, status, revision string) {
	if revision != "" {
		_, _ = s.db.Exec(`UPDATE grammar_topics SET sync_status=?, repository_revision=? WHERE id=?`, status, revision, id)
	} else {
		_, _ = s.db.Exec(`UPDATE grammar_topics SET sync_status=? WHERE id=?`, status, id)
	}
}

type Pending struct {
	ID       int64
	TopicID  string
	Title    string
	Proposal GProposal
	Ref      MsgRef
}

func (s *Store) AddPending(p Pending) (int64, error) {
	var url any
	if p.Ref.URL != "" {
		url = p.Ref.URL
	}
	res, err := s.db.Exec(`INSERT INTO pending_grammar(topic_id,title,proposal_json,chat_id,message_id,source_url,created_at) VALUES(?,?,?,?,?,?,?)`,
		p.TopicID, p.Title, mustJSON(p.Proposal), p.Ref.ChatID, p.Ref.MessageID, url, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) TakePending(id int64) (*Pending, error) {
	var p Pending
	var pj string
	var url sql.NullString
	err := s.db.QueryRow(`SELECT id,topic_id,title,proposal_json,chat_id,message_id,source_url FROM pending_grammar WHERE id=?`, id).
		Scan(&p.ID, &p.TopicID, &p.Title, &pj, &p.Ref.ChatID, &p.Ref.MessageID, &url)
	if err != nil {
		return nil, err
	}
	_, _ = s.db.Exec(`DELETE FROM pending_grammar WHERE id=?`, id)
	_ = json.Unmarshal([]byte(pj), &p.Proposal)
	p.Ref.URL = url.String
	p.Ref.Type = "grammar"
	return &p, nil
}

// ---------- review state ----------

type Review struct {
	TargetType   string `json:"t"`
	TargetID     string `json:"id"`
	Stage        int    `json:"s"`
	NextDue      int64  `json:"d"`
	LastReviewed int64  `json:"l"`
	Independent  int    `json:"i"`
	Hinted       int    `json:"h"`
	Failed       int    `json:"f"`
	Spontaneous  int    `json:"sp"`
}

func (s *Store) Review(tt, id string) Review {
	r := Review{TargetType: tt, TargetID: id, Stage: -1, NextDue: now()}
	_ = s.db.QueryRow(`SELECT stage,next_due_at,last_reviewed_at,independent_count,hinted_count,failed_count,spontaneous_count
		FROM review_state WHERE target_type=? AND target_id=?`, tt, id).
		Scan(&r.Stage, &r.NextDue, &r.LastReviewed, &r.Independent, &r.Hinted, &r.Failed, &r.Spontaneous)
	return r
}

func (s *Store) SaveReview(r Review) {
	_, _ = s.db.Exec(`INSERT INTO review_state(target_type,target_id,stage,next_due_at,last_reviewed_at,independent_count,hinted_count,failed_count,spontaneous_count)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(target_type,target_id) DO UPDATE SET stage=excluded.stage,
		next_due_at=excluded.next_due_at,last_reviewed_at=excluded.last_reviewed_at,independent_count=excluded.independent_count,
		hinted_count=excluded.hinted_count,failed_count=excluded.failed_count,spontaneous_count=excluded.spontaneous_count`,
		r.TargetType, r.TargetID, r.Stage, r.NextDue, r.LastReviewed, r.Independent, r.Hinted, r.Failed, r.Spontaneous)
}

func endOfDay(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 23, 59, 59, 0, t.Location()).Unix()
}

func (s *Store) queryIDs(q string, args ...any) []int64 {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

// SelectTraining: просроченные → с ошибками → на сегодня → новые → одна контрольная.
func (s *Store) SelectTraining(total, newMax int) []int64 {
	t := time.Now()
	eod := endOfDay(t)
	base := `SELECT i.id FROM lexical_items i JOIN review_state r ON r.target_type='lex' AND r.target_id=CAST(i.id AS TEXT) WHERE i.status='active' AND `
	due := s.queryIDs(base+`r.last_reviewed_at>0 AND r.next_due_at<=? ORDER BY (r.next_due_at<=?) DESC, r.failed_count DESC, r.next_due_at ASC LIMIT ?`, eod, t.Unix(), total)
	out := due
	if rem := total - len(out); rem > 0 {
		n := newMax
		if n > rem {
			n = rem
		}
		out = append(out, s.queryIDs(base+`r.last_reviewed_at=0 ORDER BY i.created_at ASC LIMIT ?`, n)...)
	}
	if total-len(out) > 0 {
		out = append(out, s.queryIDs(base+`r.last_reviewed_at>0 AND r.next_due_at>? ORDER BY r.last_reviewed_at ASC LIMIT 1`, eod)...)
	}
	return out
}

func (s *Store) GrammarForPractice() []string {
	rows, err := s.db.Query(`SELECT g.id FROM grammar_topics g LEFT JOIN review_state r ON r.target_type='gram' AND r.target_id=g.id
		WHERE g.points_json != '[]' ORDER BY COALESCE(r.next_due_at,0) ASC, COALESCE(r.failed_count,0) DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

// ---------- sessions & attempts ----------

type Plan struct {
	Items   []int64  `json:"items"`
	Grammar []string `json:"grammar"`
}

type Session struct {
	ID       int64
	Step     int
	Plan     Plan
	Exercise string
}

func (s *Store) ActiveSession() (*Session, error) {
	var x Session
	var pj string
	err := s.db.QueryRow(`SELECT id,current_step,plan_json,current_exercise_json FROM sessions WHERE status='active' ORDER BY id DESC LIMIT 1`).
		Scan(&x.ID, &x.Step, &pj, &x.Exercise)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(pj), &x.Plan)
	return &x, nil
}

func (s *Store) NewSession(p Plan) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO sessions(status,plan_json,started_at) VALUES('active',?,?)`, mustJSON(p), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateSession(id int64, step int, exJSON string) {
	_, _ = s.db.Exec(`UPDATE sessions SET current_step=?, current_exercise_json=? WHERE id=?`, step, exJSON, id)
}

func (s *Store) FinishSession(id int64) {
	_, _ = s.db.Exec(`UPDATE sessions SET status='finished', finished_at=?, current_exercise_json='' WHERE id=?`, now(), id)
}

type Attempt struct {
	SessionID    int64
	RefID        int64
	ExerciseType string
	AttemptNo    int
	Result       string
	GrammarRes   string
	UsedHint     bool
	Eval         *Eval
	Correction   string
	Spontaneous  bool
	Snapshot     []Review
	LexID        int64
	GrammarID    string
}

func (s *Store) AddAttempt(a Attempt) (int64, error) {
	var mok, tp, fok any
	var gok, conf any
	codes := "[]"
	if a.Eval != nil {
		mok, tp, fok = b2i(a.Eval.MeaningOK), b2i(a.Eval.TargetProduced), b2i(a.Eval.FormOK)
		gok, conf = a.Eval.Grammar, a.Eval.Confidence
		codes = mustJSON(a.Eval.ErrorCodes)
	}
	var ref any
	if a.RefID > 0 {
		ref = a.RefID
	}
	snap := ""
	if len(a.Snapshot) > 0 {
		snap = mustJSON(a.Snapshot)
	}
	res, err := s.db.Exec(`INSERT INTO attempts(session_id,message_ref_id,exercise_type,attempt_no,result,grammar_result,used_hint,meaning_ok,target_produced,form_ok,grammar_ok,confidence,error_codes,correction,spontaneous_usage,srs_snapshot,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.SessionID, ref, a.ExerciseType, a.AttemptNo, a.Result, a.GrammarRes, b2i(a.UsedHint),
		mok, tp, fok, gok, conf, codes, a.Correction, b2i(a.Spontaneous), snap, now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if a.LexID > 0 {
		_, _ = s.db.Exec(`INSERT INTO attempt_targets VALUES(?,?,?)`, id, "lex", strconv.FormatInt(a.LexID, 10))
	}
	if a.GrammarID != "" {
		_, _ = s.db.Exec(`INSERT INTO attempt_targets VALUES(?,?,?)`, id, "gram", a.GrammarID)
	}
	return id, nil
}

// DisputeAttempt помечает попытку спорной и откатывает её влияние на расписание.
func (s *Store) DisputeAttempt(id int64) (bool, error) {
	var res, snap string
	if err := s.db.QueryRow(`SELECT result, srs_snapshot FROM attempts WHERE id=?`, id).Scan(&res, &snap); err != nil {
		return false, err
	}
	if res == "disputed" {
		return false, nil
	}
	if snap != "" {
		var rs []Review
		if json.Unmarshal([]byte(snap), &rs) == nil {
			for _, r := range rs {
				s.SaveReview(r)
			}
		}
	}
	_, err := s.db.Exec(`UPDATE attempts SET result='disputed', grammar_result=CASE WHEN grammar_result='' THEN '' ELSE 'disputed' END WHERE id=?`, id)
	return true, err
}

func (s *Store) SessionStats(id int64) map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(`SELECT result, spontaneous_usage, COUNT(*) FROM attempts WHERE session_id=? AND (attempt_no=1 OR spontaneous_usage=1) GROUP BY result, spontaneous_usage`, id)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		var sp, n int
		_ = rows.Scan(&r, &sp, &n)
		if sp == 1 {
			out["spontaneous"] += n
		} else {
			out[r] += n
		}
	}
	return out
}

func (s *Store) count(q string, args ...any) int {
	var n int
	_ = s.db.QueryRow(q, args...).Scan(&n)
	return n
}

// ---------- jobs ----------

type Job struct {
	ID      int64
	Type    string
	Payload string
	Retries int
}

func (s *Store) DueJobs() []Job {
	rows, err := s.db.Query(`SELECT id,type,derived_payload_json,retry_count FROM jobs WHERE status='pending' AND next_attempt_at<=? ORDER BY id LIMIT 20`, now())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		_ = rows.Scan(&j.ID, &j.Type, &j.Payload, &j.Retries)
		out = append(out, j)
	}
	return out
}

func (s *Store) JobDone(id int64) {
	_, _ = s.db.Exec(`UPDATE jobs SET status='done', last_error_code='' WHERE id=?`, id)
}

func (s *Store) JobFail(id int64, code string, retry bool, retries int) {
	if !retry {
		_, _ = s.db.Exec(`UPDATE jobs SET status='failed', last_error_code=? WHERE id=?`, code, id)
		return
	}
	d := time.Duration(1<<min(retries, 8)) * time.Minute
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	_, _ = s.db.Exec(`UPDATE jobs SET retry_count=retry_count+1, next_attempt_at=?, last_error_code=? WHERE id=?`, time.Now().Add(d).Unix(), code, id)
}

// RequeueTopic создаёт новую задачу синхронизации (например, после разрешения конфликта).
func (s *Store) RequeueTopic(id string) {
	ts := now()
	_, _ = s.db.Exec(`INSERT INTO jobs(type,derived_payload_json,next_attempt_at,created_at) VALUES('github_sync',?,?,?)`,
		mustJSON(map[string]string{"topic_id": id}), ts, ts)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
