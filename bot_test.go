package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLadder(t *testing.T) {
	l := []int{1, 3, 7}
	at := time.Unix(0, 0)
	r := Review{Stage: -1}
	r = ApplyOutcome(r, ResIndependent, at, l)
	if r.Stage != 0 || r.NextDue != 86400 {
		t.Fatalf("independent: %+v", r)
	}
	r = ApplyOutcome(r, ResIndependent, at, l)
	if r.Stage != 1 || r.NextDue != 3*86400 {
		t.Fatalf("independent 2: %+v", r)
	}
	h := ApplyOutcome(r, ResHinted, at, l)
	if h.Stage != 1 || h.NextDue != 86400 {
		t.Fatalf("hinted: %+v", h)
	}
	f := ApplyOutcome(r, ResFailed, at, l)
	if f.Stage != 0 || f.Failed != 1 {
		t.Fatalf("failed: %+v", f)
	}
	u := ApplyOutcome(r, ResUncertain, at.Add(time.Hour), l)
	if u != r {
		t.Fatal("uncertain must not change state")
	}
	for i := 0; i < 10; i++ {
		r = ApplyOutcome(r, ResIndependent, at, l)
	}
	if r.Stage != 2 {
		t.Fatal("stage must be capped")
	}
}

func TestClassify(t *testing.T) {
	syn := &Eval{MeaningOK: true, NaturalOK: true, TargetProduced: false, Confidence: "high", Grammar: "not_applicable"}
	if l, _ := Classify(syn, false); l != ResFailed {
		t.Fatal("synonym is not a recall of the target")
	}
	ok := &Eval{TargetProduced: true, FormOK: true, Confidence: "high", Grammar: "ok"}
	if l, g := Classify(ok, true); l != ResHinted || g != ResHinted {
		t.Fatal("hint must downgrade")
	}
	if l, _ := Classify(&Eval{Confidence: "low"}, false); l != ResUncertain {
		t.Fatal("low confidence → uncertain")
	}
}

func TestMessageLink(t *testing.T) {
	if MessageLink(TGChat{ID: 5, Type: "private"}, 10) != "" {
		t.Fatal("private chats must not get fake links")
	}
	if got := MessageLink(TGChat{ID: -1001234, Type: "supergroup"}, 7); got != "https://t.me/c/1234/7" {
		t.Fatal(got)
	}
	if got := MessageLink(TGChat{ID: -1001234, Type: "supergroup", Username: "myeng"}, 7); got != "https://t.me/myeng/7" {
		t.Fatal(got)
	}
}

func TestRevealsTarget(t *testing.T) {
	if !revealsTarget("put something off", "Скажи, что ты put off звонок") {
		t.Fatal("should detect")
	}
	if revealsTarget("put something off", "Ты откладываешь звонок") {
		t.Fatal("false positive")
	}
}

func TestGrammarApplyRender(t *testing.T) {
	tp := &Topic{ID: "articles", Title: "Articles", Path: "grammar/articles.md"}
	ApplyProposal(tp, &GProposal{Points: []GPointIn{{"usage", "a — первое упоминание"}}}, 1, "")
	n := ApplyProposal(tp, &GProposal{Points: []GPointIn{{"usage", "A — первое  упоминание"}}}, 2, "")
	if n != 0 || len(tp.Points[0].Sources) != 2 {
		t.Fatal("duplicate must only add source")
	}
	ApplyProposal(tp, &GProposal{Conflicts: []GConflict{{ExistingIndex: 0, NewText: "x"}}}, 3, "replace")
	if tp.Points[0].Text != "x" {
		t.Fatal("replace")
	}
	md := RenderTopic(tp, map[int64]MsgRef{3: {ID: 3, ChatID: 9, MessageID: 4}})
	if !strings.Contains(md, "прямая ссылка недоступна") || strings.Contains(md, "sources/") {
		t.Fatal(md)
	}
}

func TestStoreNoRawTextAndUndo(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	raw := "I was going to bring it up yesterday but got distracted SECRET-ORIGINAL"
	c := []LexCandidate{{CanonicalForm: "bring something up", MeaningRU: "поднять тему", ExampleOrigin: "generated"}}
	if _, err := st.AddLexicon(MsgRef{ChatID: 1, MessageID: 1, Type: "lexicon"}, c, "b1"); err != nil {
		t.Fatal(err)
	}
	// повтор: та же единица, новый источник — без новой карточки
	a, _ := st.AddLexicon(MsgRef{ChatID: 1, MessageID: 2, Type: "lexicon"}, c, "b2")
	if a[0].CreatedNew {
		t.Fatal("dedup failed")
	}
	// другое значение — отдельная единица
	d, _ := st.AddLexicon(MsgRef{ChatID: 1, MessageID: 3, Type: "lexicon"},
		[]LexCandidate{{CanonicalForm: "bring something up", MeaningRU: "вырастить ребёнка", ExampleOrigin: "generated"}}, "b3")
	if !d[0].CreatedNew {
		t.Fatal("different meanings must not merge")
	}
	if n, _ := st.UndoBatch("b2"); n != 1 || st.count(`SELECT COUNT(*) FROM lexical_items`) != 2 {
		t.Fatal("undo of link must keep the item")
	}
	// в базе нет ни одного значения, содержащего исходный текст
	for _, tbl := range []string{"message_refs", "lexical_items", "jobs", "attempts", "sessions", "kv"} {
		rows, _ := st.db.Query(`SELECT * FROM ` + tbl)
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.RawBytes, len(cols))
			ptr := make([]any, len(cols))
			for i := range vals {
				ptr[i] = &vals[i]
			}
			_ = rows.Scan(ptr...)
			for _, v := range vals {
				if strings.Contains(string(v), "SECRET-ORIGINAL") {
					t.Fatalf("raw text found in %s", tbl)
				}
			}
		}
		rows.Close()
	}
	_ = raw
}

// Прокси из окружения контейнера применяется ко всем исходящим запросам: Telegram, Groq, GitHub.
func TestProxyFromEnvironment(t *testing.T) {
	var mu sync.Mutex
	var hosts []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		mu.Unlock()
		http.Error(w, "blocked by test proxy", http.StatusForbidden)
	}))
	defer proxy.Close()
	for _, k := range []string{"NO_PROXY", "no_proxy", "HTTP_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", proxy.URL)

	tg := NewTelegram("123:abc")
	_, err := tg.GetUpdates(context.Background(), 0, 0)
	if err == nil || strings.Contains(err.Error(), "123:abc") {
		t.Fatalf("expected error without token leak, got %v", err)
	}
	cfg := &Config{GroqModel: "m", GroqKey: "k", GroqBaseURL: "https://api.groq.com/openai/v1",
		GroqTimeout: 5 * time.Second, GroqMaxTokens: 10, GitHubRepo: "o/r", GitHubAPI: "https://api.github.com", GitHubBranch: "main"}
	var out map[string]any
	_ = NewGroq(cfg).JSON(context.Background(), "test", "s", "u", sObj(map[string]any{"a": sStr()}, "a"), &out, nil)
	_, _ = NewGitHub(cfg).RemoteSHA(context.Background(), "grammar/x.md")

	joined := strings.Join(hosts, ",")
	for _, h := range []string{"api.telegram.org", "api.groq.com", "api.github.com"} {
		if !strings.Contains(joined, h) {
			t.Fatalf("%s did not go through proxy: %s", h, joined)
		}
	}
}

// Strict-режим Groq: у каждого объекта все поля в required и additionalProperties=false.
func TestSchemasStrictCompatible(t *testing.T) {
	var check func(path string, s map[string]any)
	check = func(path string, s map[string]any) {
		switch s["type"] {
		case "object":
			if s["additionalProperties"] != false {
				t.Fatalf("%s: additionalProperties must be false", path)
			}
			props := s["properties"].(map[string]any)
			req := s["required"].([]string)
			if len(req) != len(props) {
				t.Fatalf("%s: all properties must be required", path)
			}
			for k, v := range props {
				check(path+"."+k, v.(map[string]any))
			}
		case "array":
			check(path+"[]", s["items"].(map[string]any))
		}
	}
	for name, s := range map[string]map[string]any{"lex": lexSchema, "pick": pickSchema, "proposal": proposalSchema,
		"exercise": exSchema, "eval": evalSchema, "dstart": dStartSchema, "dreply": dReplySchema} {
		check(name, s)
	}
}

// Ответ Groq разбирается, а 400 json_validate_failed приводит к попытке исправления.
func TestGroqJSONAndFixRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		rf := req["response_format"].(map[string]any)
		if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["strict"] != true {
			t.Errorf("unexpected response_format %v", rf)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Error("no auth")
		}
		if calls == 1 {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"code":"json_validate_failed"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"a\":\"ok\"}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	cfg := &Config{GroqModel: "m", GroqKey: "k", GroqBaseURL: srv.URL, GroqTimeout: 5 * time.Second,
		GroqMaxTokens: 10, GroqResponseFormat: "strict", GroqRetries: 2}
	var out struct{ A string }
	if err := NewGroq(cfg).JSON(context.Background(), "t", "s", "u", sObj(map[string]any{"a": sStr()}, "a"), &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.A != "ok" || calls != 2 {
		t.Fatalf("out=%v calls=%d", out, calls)
	}
}
