package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Groq — клиент OpenAI-совместимого Chat Completions API Groq с ответами в JSON.
type Groq struct {
	cfg  *Config
	c    *http.Client
	mu   sync.Mutex // один запрос за раз + минимальный интервал между запросами
	last time.Time
}

func NewGroq(cfg *Config) *Groq {
	return &Groq{cfg: cfg, c: httpClient(cfg.GroqTimeout)}
}

var ErrQuota = errors.New("лимит запросов Groq исчерпан")

// errSchema — Groq сам отклонил ответ, не совпавший со схемой (best-effort режим).
var errSchema = errors.New("ответ модели не совпал со схемой")

type retryableErr struct {
	err   error
	delay time.Duration
}

func (e retryableErr) Error() string { return e.err.Error() }

func (g *Groq) responseFormat(op string, schema map[string]any) map[string]any {
	switch g.cfg.GroqResponseFormat {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "best_effort":
		return map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": op, "strict": false, "schema": schema}}
	default:
		return map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": op, "strict": true, "schema": schema}}
	}
}

func (g *Groq) generate(ctx context.Context, op, system, user string, schema map[string]any) (string, error) {
	g.mu.Lock()
	if w := g.cfg.GroqMinInterval - time.Since(g.last); w > 0 {
		time.Sleep(w)
	}
	defer func() { g.last = time.Now(); g.mu.Unlock() }()

	if g.cfg.GroqResponseFormat == "json_object" {
		// Без схемы на стороне API — передаём её в инструкции, проверка остаётся за валидатором.
		system += "\nОтвет — JSON-объект строго по этой JSON Schema:\n" + mustJSON(schema)
	}
	req := map[string]any{
		"model": g.cfg.GroqModel,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":           g.cfg.GroqTemperature,
		"max_completion_tokens": g.cfg.GroqMaxTokens,
		"response_format":       g.responseFormat(op, schema),
	}
	if g.cfg.GroqReasoningEffort != "" {
		req["reasoning_effort"] = g.cfg.GroqReasoningEffort
	}
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.GroqBaseURL+"/chat/completions", bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+g.cfg.GroqKey)
	resp, err := g.c.Do(hr)
	if err != nil {
		return "", retryableErr{err: fmt.Errorf("groq: %w", cleanErr(err)), delay: 2 * time.Second}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	switch {
	case resp.StatusCode == 429:
		d := 10 * time.Second
		if s, err := strconv.ParseFloat(resp.Header.Get("retry-after"), 64); err == nil && s > 0 {
			d = time.Duration(s*1000) * time.Millisecond
		}
		return "", retryableErr{err: ErrQuota, delay: d}
	case resp.StatusCode >= 500:
		return "", retryableErr{err: fmt.Errorf("groq: http %d", resp.StatusCode), delay: 3 * time.Second}
	case resp.StatusCode == 400 && bytes.Contains(raw, []byte("json_validate_failed")):
		return "", errSchema
	case resp.StatusCode != 200:
		var e struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		code := e.Error.Code
		if code == "" {
			code = e.Error.Type
		}
		return "", fmt.Errorf("groq: http %d %s", resp.StatusCode, code)
	}

	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Choices) == 0 {
		return "", fmt.Errorf("groq: пустой ответ")
	}
	c := strings.TrimSpace(r.Choices[0].Message.Content)
	if c == "" {
		return "", fmt.Errorf("groq: пустой ответ (%s)", r.Choices[0].FinishReason)
	}
	return c, nil
}

// JSON выполняет запрос, разбирает ответ в out и проверяет его validate.
// Сетевые сбои и 429 повторяются; невалидный ответ — одна попытка исправления.
func (g *Groq) JSON(ctx context.Context, op, system, user string, schema map[string]any, out any, validate func() error) error {
	start := time.Now()
	fixTried := false
	prompt := user
	var lastErr error
	for attempt := 0; attempt <= g.cfg.GroqRetries; attempt++ {
		text, err := g.generate(ctx, op, system, prompt, schema)
		if err == nil {
			if err = json.Unmarshal([]byte(text), out); err == nil && validate != nil {
				err = validate()
			}
			if err == nil {
				log.Printf("llm op=%s status=ok dur=%s", op, time.Since(start).Round(time.Millisecond))
				return nil
			}
			err = fmt.Errorf("%w: %v", errSchema, err)
		}
		lastErr = err
		if errors.Is(err, errSchema) {
			if fixTried {
				break
			}
			fixTried = true
			prompt = user + "\n\nПредыдущий ответ не прошёл проверку (" + err.Error() + "). Верни корректный JSON строго по схеме."
			continue
		}
		var re retryableErr
		if errors.As(err, &re) && attempt < g.cfg.GroqRetries && re.delay <= 60*time.Second {
			select {
			case <-time.After(re.delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		break
	}
	var re retryableErr
	if errors.As(lastErr, &re) {
		lastErr = re.err
	}
	log.Printf("llm op=%s status=error dur=%s err=%q", op, time.Since(start).Round(time.Millisecond), errCode(lastErr))
	return lastErr
}

// errCode — короткий код ошибки для логов, без пользовательских данных.
func errCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrQuota):
		return "quota"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errSchema):
		return "invalid_output"
	}
	s := err.Error()
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
