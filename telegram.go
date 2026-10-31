package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type TGUser struct {
	ID int64 `json:"id"`
}

type TGChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Username string `json:"username"`
}

type TGMessage struct {
	MessageID int64   `json:"message_id"`
	From      *TGUser `json:"from"`
	Chat      TGChat  `json:"chat"`
	Text      string  `json:"text"`
	Caption   string  `json:"caption"`
	Date      int64   `json:"date"`
}

type TGCallback struct {
	ID      string     `json:"id"`
	From    TGUser     `json:"from"`
	Message *TGMessage `json:"message"`
	Data    string     `json:"data"`
}

type TGUpdate struct {
	UpdateID      int64       `json:"update_id"`
	Message       *TGMessage  `json:"message"`
	CallbackQuery *TGCallback `json:"callback_query"`
}

type Btn struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type Inline struct {
	InlineKeyboard [][]Btn `json:"inline_keyboard"`
}

func kb(rows ...[]Btn) *Inline { return &Inline{InlineKeyboard: rows} }
func row(b ...Btn) []Btn       { return b }
func btn(t, d string) Btn      { return Btn{Text: t, Data: d} }

// Telegram — клиент Bot API.
type Telegram struct {
	token string
	c     *http.Client
}

func NewTelegram(token string) *Telegram {
	return &Telegram{token: token, c: httpClient(75 * time.Second)}
}

// httpClient — общий HTTP-клиент: прокси берётся из окружения контейнера
// (HTTPS_PROXY / HTTP_PROXY / NO_PROXY; поддерживаются http, https, socks5, socks5h).
func httpClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 20 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        10,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// cleanErr убирает URL (в нём токен) из транспортных ошибок.
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (t *Telegram) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+t.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: bad request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.c.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, cleanErr(err))
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: http %d", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (t *Telegram) GetUpdates(ctx context.Context, offset int64, timeout int) ([]TGUpdate, error) {
	var ups []TGUpdate
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

const tgLimit = 4000

func (t *Telegram) Send(ctx context.Context, chatID int64, text string, markup any) error {
	if r := []rune(text); len(r) > tgLimit {
		text = string(r[:tgLimit]) + "…"
	}
	p := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]any{"is_disabled": true}}
	if markup != nil {
		p["reply_markup"] = markup
	}
	return t.call(ctx, "sendMessage", p, nil)
}

func (t *Telegram) Answer(ctx context.Context, id, text string) {
	_ = t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func (t *Telegram) Typing(ctx context.Context, chatID int64) {
	_ = t.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}

func (t *Telegram) DropKeyboard(ctx context.Context, chatID, msgID int64) {
	_ = t.call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": chatID, "message_id": msgID}, nil)
}

// MessageLink возвращает открываемую ссылку на сообщение или "", если для этого
// типа чата постоянной ссылки не существует (например, личный чат с ботом).
func MessageLink(chat TGChat, msgID int64) string {
	if chat.Type != "supergroup" && chat.Type != "channel" {
		return ""
	}
	if chat.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", chat.Username, msgID)
	}
	s := strconv.FormatInt(chat.ID, 10)
	if strings.HasPrefix(s, "-100") {
		return fmt.Sprintf("https://t.me/c/%s/%d", s[4:], msgID)
	}
	return ""
}
