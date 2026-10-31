package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Bot struct {
	cfg    *Config
	tg     *Telegram
	llm    *LLM
	st     *Store
	gh     *GitHub
	chatID int64
	live   *Live
	expl   map[int64]string // подробные разборы держим только в памяти
	wake   chan struct{}
}

func esc(s string) string { return html.EscapeString(s) }

var menu = map[string]any{
	"keyboard": [][]map[string]string{
		{{"text": "Тренироваться"}, {"text": "Прогресс"}},
		{{"text": "Грамматика"}, {"text": "Закончить"}},
	},
	"resize_keyboard": true, "is_persistent": true,
}

func (b *Bot) send(ctx context.Context, text string, markup any) {
	if err := b.tg.Send(ctx, b.chatID, text, markup); err != nil {
		log.Printf("tg send error: %v", err)
	}
}

func (b *Bot) Run(ctx context.Context) {
	if id, _ := strconv.ParseInt(b.st.KVGet("chat_id"), 10, 64); id != 0 {
		b.chatID = id
	} else {
		b.chatID = b.cfg.AllowedUserID // личный чат: chat_id = user_id
	}
	if s, _ := b.st.ActiveSession(); s != nil {
		b.send(ctx, "Есть незавершённая тренировка. Продолжим?", kb(row(btn("Продолжить", "resume"), btn("Завершить", "stop"))))
	}
	go b.worker(ctx)

	offset, _ := strconv.ParseInt(b.st.KVGet("offset"), 10, 64)
	for ctx.Err() == nil {
		ups, err := b.tg.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range ups {
			b.safeHandle(ctx, u)
			offset = u.UpdateID + 1
			b.st.KVSet("offset", strconv.FormatInt(offset, 10))
		}
	}
}

func (b *Bot) safeHandle(ctx context.Context, u TGUpdate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in handler: %v", r)
		}
	}()
	hctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if cq := u.CallbackQuery; cq != nil {
		if cq.From.ID != b.cfg.AllowedUserID {
			b.tg.Answer(hctx, cq.ID, "")
			return
		}
		if cq.Message != nil {
			b.setChat(cq.Message.Chat.ID)
		}
		b.tg.Answer(hctx, cq.ID, "")
		b.onCallback(hctx, cq)
		return
	}
	m := u.Message
	if m == nil || m.From == nil || m.From.ID != b.cfg.AllowedUserID {
		return // чужие сообщения игнорируются
	}
	b.setChat(m.Chat.ID)
	b.onMessage(hctx, m)
}

func (b *Bot) setChat(id int64) {
	if id != b.chatID {
		b.chatID = id
		b.st.KVSet("chat_id", strconv.FormatInt(id, 10))
	}
}

var grammarTag = regexp.MustCompile(`(?i)(^|\s)#grammar\b`)

func (b *Bot) onMessage(ctx context.Context, m *TGMessage) {
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	text = strings.TrimSpace(text)
	if text == "" {
		b.send(ctx, "Пока я понимаю только текст.", nil)
		return
	}
	ref := MsgRef{ChatID: m.Chat.ID, MessageID: m.MessageID, URL: MessageLink(m.Chat, m.MessageID)}

	if strings.HasPrefix(text, "/") {
		cmd, arg, _ := strings.Cut(text, " ")
		cmd, _, _ = strings.Cut(cmd, "@")
		switch cmd {
		case "/start", "/help":
			b.help(ctx)
		case "/train":
			b.startTraining(ctx)
		case "/stop":
			b.stopTraining(ctx)
		case "/progress":
			b.progress(ctx)
		case "/grammar":
			b.grammarList(ctx)
		case "/add":
			arg = strings.TrimSpace(arg)
			if arg == "" {
				b.send(ctx, "Напиши материал после команды: <code>/add bring something up</code>", nil)
				return
			}
			ref.Type = "lexicon"
			b.addLexicon(ctx, arg, ref)
		default:
			b.send(ctx, "Не знаю такой команды. /help — список команд.", nil)
		}
		return
	}
	switch text {
	case "Тренироваться":
		b.startTraining(ctx)
		return
	case "Прогресс":
		b.progress(ctx)
		return
	case "Грамматика":
		b.grammarList(ctx)
		return
	case "Закончить":
		b.stopTraining(ctx)
		return
	}
	if grammarTag.MatchString(text) {
		ref.Type = "grammar"
		b.addGrammar(ctx, strings.TrimSpace(grammarTag.ReplaceAllString(text, " ")), ref)
		return
	}
	if s, _ := b.st.ActiveSession(); s != nil {
		ref.Type = "answer"
		b.onAnswer(ctx, s, text, ref)
		return
	}
	ref.Type = "lexicon"
	b.addLexicon(ctx, text, ref)
}

func (b *Bot) help(ctx context.Context) {
	b.send(ctx, `<b>Тренажёр разговорного английского</b>

• Просто пришли слово, фразу или текст — я выделю полезные разговорные выражения.
• Сообщение с <code>#grammar</code> — заметка по грамматике, пополнит базу в GitHub.
• /train — короткая тренировка (5 заданий + мини-диалог).
• Во время тренировки обычный текст — это ответ. Добавить лексику: <code>/add …</code>, грамматику: <code>#grammar</code>.
• /progress — прогресс, /grammar — изученные темы, /stop — закончить тренировку.

Исходные сообщения я не храню — только ссылки на них.`, menu)
}

func newBatchID() string {
	var x [6]byte
	_, _ = rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

func failText(err error) string {
	if errors.Is(err, ErrQuota) {
		return "лимит запросов Groq исчерпан"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "модель не ответила вовремя"
	}
	return "модель недоступна или вернула некорректный ответ"
}

// ---------- лексика ----------

func (b *Bot) addLexicon(ctx context.Context, text string, ref MsgRef) {
	if b.st.RefExists(ref.ChatID, ref.MessageID) {
		b.send(ctx, "Это сообщение уже обработано.", nil)
		return
	}
	b.tg.Typing(ctx, b.chatID)
	res, err := b.llm.ExtractLexicon(ctx, text, b.cfg.MaxItems)
	if err != nil {
		b.send(ctx, "Не удалось обработать: "+failText(err)+". Материал <b>не сохранён</b> — пришли его ещё раз чуть позже.", nil)
		return
	}
	if len(res.Items) == 0 {
		b.send(ctx, "Не нашёл здесь полезных разговорных выражений. Ничего не сохранено.", nil)
		return
	}
	batch := newBatchID()
	added, err := b.st.AddLexicon(ref, res.Items, batch)
	if err != nil {
		log.Printf("db add lexicon: %v", err)
		b.send(ctx, "Ошибка базы данных — материал не сохранён, пришли его ещё раз.", nil)
		return
	}
	var forms []string
	known := 0
	for _, a := range added {
		forms = append(forms, "<b>"+esc(a.Item.CanonicalForm)+"</b>")
		if !a.CreatedNew {
			known++
		}
	}
	msg := fmt.Sprintf("Добавлено %s:\n%s", plural(len(added), "выражение", "выражения", "выражений"), strings.Join(forms, " · "))
	if known > 0 {
		msg += fmt.Sprintf("\n\n%d уже были в словаре — добавил источник, история обучения сохранена.", known)
	}
	if res.MoreAvailable {
		msg += fmt.Sprintf("\n\nВ тексте есть ещё полезное, но лимит — %d за сообщение. Пришли нужный фрагмент отдельно.", b.cfg.MaxItems)
	}
	b.send(ctx, msg, kb(row(btn("Посмотреть", "view:"+batch), btn("Отменить добавление", "undo:"+batch))))
}

func plural(n int, one, few, many string) string {
	w := many
	if n%10 == 1 && n%100 != 11 {
		w = one
	} else if n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14) {
		w = few
	}
	return fmt.Sprintf("%d %s", n, w)
}

func refLabel(r MsgRef) string {
	if r.URL != "" {
		return fmt.Sprintf(`<a href="%s">сообщение</a>`, esc(r.URL))
	}
	return "ссылка недоступна для этого типа чата"
}

func (b *Bot) viewBatch(ctx context.Context, batch string) {
	items := b.st.BatchItems(batch)
	if len(items) == 0 {
		b.send(ctx, "Это добавление уже отменено или не найдено.", nil)
		return
	}
	var sb strings.Builder
	for _, it := range items {
		fmt.Fprintf(&sb, "<b>%s</b> — %s\n", esc(it.CanonicalForm), esc(it.MeaningRU))
		if it.UsageNote != "" {
			fmt.Fprintf(&sb, "<i>%s</i>\n", esc(it.UsageNote))
		}
		if it.Example != "" {
			origin := "из твоего материала"
			if it.ExampleOrigin == "generated" {
				origin = "пример составлен моделью"
			}
			fmt.Fprintf(&sb, "› %s (%s)\n", esc(it.Example), origin)
		}
		if it.Ambiguous {
			sb.WriteString("⚠ значение по контексту неоднозначно\n")
		}
		var src []string
		for _, r := range b.st.ItemSources(it.ID) {
			src = append(src, refLabel(r))
		}
		fmt.Fprintf(&sb, "Источник: %s\n\n", strings.Join(src, ", "))
	}
	b.send(ctx, sb.String(), nil)
}

// ---------- грамматика ----------

func (b *Bot) addGrammar(ctx context.Context, text string, ref MsgRef) {
	if text == "" {
		b.send(ctx, "После <code>#grammar</code> нужна сама заметка.", nil)
		return
	}
	if b.st.RefExists(ref.ChatID, ref.MessageID) {
		b.send(ctx, "Это сообщение уже обработано.", nil)
		return
	}
	b.tg.Typing(ctx, b.chatID)
	topics := b.st.Topics()
	pick, err := b.llm.PickTopic(ctx, text, topics)
	if err != nil {
		b.send(ctx, "Не удалось обработать заметку: "+failText(err)+". Ничего не сохранено — пришли её ещё раз позже.", nil)
		return
	}
	if !pick.IsGrammar {
		b.send(ctx, "Это не похоже на грамматическую заметку, поэтому в базу грамматики я её не добавил. Если это лексика — пришли без <code>#grammar</code>.", nil)
		return
	}
	topic, err := b.st.Topic(pick.TopicID)
	if err != nil { // новая тема
		topic = &Topic{ID: pick.TopicID, Title: pick.Title, Path: "grammar/" + pick.TopicID + ".md"}
	}
	prop, err := b.llm.ProposeGrammar(ctx, text, topic.Title, topic.Points)
	if err != nil {
		b.send(ctx, "Не удалось структурировать заметку: "+failText(err)+". Ничего не сохранено — пришли её ещё раз позже.", nil)
		return
	}
	if prop.Insufficient {
		q := prop.Question
		if q == "" {
			q = "Недостаточно содержания для правила."
		}
		b.send(ctx, "В базу пока ничего не добавил: "+esc(q)+"\n\nДополни заметку и пришли её целиком ещё раз (исходный текст я не храню).", nil)
		return
	}
	if len(prop.Points) == 0 && len(prop.Conflicts) == 0 {
		b.send(ctx, fmt.Sprintf("Всё из заметки уже есть в теме «%s». Ничего не изменено.", esc(topic.Title)), nil)
		return
	}
	if len(prop.Conflicts) > 0 {
		pid, err := b.st.AddPending(Pending{TopicID: topic.ID, Title: topic.Title, Proposal: *prop, Ref: ref})
		if err != nil {
			b.send(ctx, "Ошибка базы данных — заметка не сохранена.", nil)
			return
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Заметка противоречит теме «%s»:\n\n", esc(topic.Title))
		for _, c := range prop.Conflicts {
			fmt.Fprintf(&sb, "• Было: %s\n  Новое: %s\n  %s\n\n", esc(topic.Points[c.ExistingIndex].Text), esc(c.NewText), esc(c.Description))
		}
		sb.WriteString("Что сделать? Остальные пункты заметки применятся вместе с выбором.")
		id := strconv.FormatInt(pid, 10)
		b.send(ctx, sb.String(), kb(row(btn("Заменить", "gr:"+id+":replace"), btn("Как уточнение", "gr:"+id+":clarify")), row(btn("Отменить изменение", "gr:"+id+":cancel"))))
		return
	}
	b.commitGrammar(ctx, topic, prop, ref, "")
}

func (b *Bot) commitGrammar(ctx context.Context, topic *Topic, prop *GProposal, ref MsgRef, mode string) {
	refID, err := b.st.InsertRef(ref)
	if err != nil {
		b.send(ctx, "Ошибка базы данных — заметка не сохранена.", nil)
		return
	}
	n := ApplyProposal(topic, prop, refID, mode)
	topic.Markdown = RenderTopic(topic, b.st.Refs(topicSourceIDs(topic)))
	if err := b.st.SaveTopic(topic, refID); err != nil {
		log.Printf("db save topic: %v", err)
		b.send(ctx, "Ошибка базы данных — заметка не сохранена.", nil)
		return
	}
	b.send(ctx, fmt.Sprintf("Тема «%s» обновлена локально: %s. Синхронизация с GitHub — в очереди, сообщу о результате.",
		esc(topic.Title), plural(n, "новый пункт", "новых пункта", "новых пунктов")), nil)
	b.kick()
}

func (b *Bot) grammarList(ctx context.Context) {
	ts := b.st.Topics()
	if len(ts) == 0 {
		b.send(ctx, "Грамматических тем пока нет. Пришли заметку с <code>#grammar</code>.", nil)
		return
	}
	status := map[string]string{"synced": "синхронизировано", "pending": "ожидает синхронизации",
		"conflict": "конфликт в GitHub", "local_only": "только локально", "error": "ошибка синхронизации"}
	var sb strings.Builder
	sb.WriteString("<b>Изученная грамматика</b>\n\n")
	for _, t := range ts {
		u := fmt.Sprintf("https://github.com/%s/blob/%s/%s", b.cfg.GitHubRepo, b.cfg.GitHubBranch, t.Path)
		fmt.Fprintf(&sb, "• <a href=\"%s\">%s</a> — %s, %s\n", esc(u), esc(t.Title),
			plural(len(t.Points), "пункт", "пункта", "пунктов"), status[t.SyncStatus])
	}
	b.send(ctx, sb.String(), nil)
}

// ---------- синхронизация ----------

func (b *Bot) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Bot) worker(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		for _, j := range b.st.DueJobs() {
			b.runJob(ctx, j)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.wake:
		}
	}
}

func (b *Bot) runJob(ctx context.Context, j Job) {
	if j.Type != "github_sync" {
		b.st.JobFail(j.ID, "unknown_type", false, 0)
		return
	}
	var p struct {
		TopicID string `json:"topic_id"`
	}
	_ = jsonUnmarshal(j.Payload, &p)
	t, err := b.st.Topic(p.TopicID)
	if err != nil {
		b.st.JobFail(j.ID, "topic_missing", false, 0)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	sha, err := b.gh.SyncTopic(sctx, t)
	switch {
	case err == nil:
		b.st.SetTopicSync(t.ID, "synced", sha)
		b.st.JobDone(j.ID)
		b.send(ctx, fmt.Sprintf("Тема «%s» синхронизирована с GitHub.", esc(t.Title)), nil)
	case errors.Is(err, ErrConflict):
		b.st.SetTopicSync(t.ID, "conflict", "")
		b.st.JobFail(j.ID, "conflict", false, 0)
		b.send(ctx, fmt.Sprintf("Файл <code>%s</code> в GitHub изменён вне бота — перезаписывать молча не буду. Локальная версия сохранена, тренировки работают.", esc(t.Path)),
			kb(row(btn("Перезаписать версией бота", "ghforce:"+t.ID))))
	case errors.Is(err, ErrNoToken):
		b.st.SetTopicSync(t.ID, "local_only", "")
		b.st.JobFail(j.ID, "no_token", false, 0)
		b.send(ctx, "Тема сохранена только локально: не задан GITHUB_TOKEN для записи в репозиторий.", nil)
	default:
		log.Printf("github sync topic=%s err=%v", t.ID, err)
		b.st.SetTopicSync(t.ID, "pending", "")
		b.st.JobFail(j.ID, errCode(err), true, j.Retries)
	}
}

func (b *Bot) forceSync(ctx context.Context, id string) {
	t, err := b.st.Topic(id)
	if err != nil {
		return
	}
	sha, err := b.gh.RemoteSHA(ctx, t.Path)
	if err != nil {
		b.send(ctx, "GitHub недоступен, попробуй позже.", nil)
		return
	}
	b.st.SetTopicSync(t.ID, "pending", sha)
	if sha == "" { // файл удалили — создаём заново
		_, _ = b.st.db.Exec(`UPDATE grammar_topics SET repository_revision='' WHERE id=?`, t.ID)
	}
	b.st.RequeueTopic(t.ID)
	b.kick()
	b.send(ctx, "Перезапишу файл версией бота.", nil)
}

// ---------- кнопки ----------

func (b *Bot) onCallback(ctx context.Context, cq *TGCallback) {
	d := cq.Data
	kind, arg, _ := strings.Cut(d, ":")
	dropKB := func() {
		if cq.Message != nil {
			b.tg.DropKeyboard(ctx, cq.Message.Chat.ID, cq.Message.MessageID)
		}
	}
	switch kind {
	case "view":
		b.viewBatch(ctx, arg)
	case "undo":
		dropKB()
		n, err := b.st.UndoBatch(arg)
		switch {
		case err != nil:
			b.send(ctx, "Не удалось отменить — ошибка базы данных.", nil)
		case n == 0:
			b.send(ctx, "Нечего отменять.", nil)
		default:
			b.send(ctx, "Добавление отменено. Ранее существовавшие выражения и их история не тронуты.", nil)
		}
	case "gr":
		dropKB()
		idS, mode, _ := strings.Cut(arg, ":")
		id, _ := strconv.ParseInt(idS, 10, 64)
		p, err := b.st.TakePending(id)
		if err != nil {
			b.send(ctx, "Это решение уже принято.", nil)
			return
		}
		if mode == "cancel" {
			b.send(ctx, "Изменение отменено, тема не тронута.", nil)
			return
		}
		topic, err := b.st.Topic(p.TopicID)
		if err != nil {
			topic = &Topic{ID: p.TopicID, Title: p.Title, Path: "grammar/" + p.TopicID + ".md"}
		}
		b.commitGrammar(ctx, topic, &p.Proposal, p.Ref, mode)
	case "ghforce":
		dropKB()
		b.forceSync(ctx, arg)
	case "train":
		b.startTraining(ctx)
	case "resume":
		dropKB()
		b.resume(ctx)
	case "stop":
		dropKB()
		b.stopTraining(ctx)
	case "hint", "show", "skip", "next":
		dropKB()
		b.trainingButton(ctx, kind)
	case "expl":
		id, _ := strconv.ParseInt(arg, 10, 64)
		if e, ok := b.expl[id]; ok && e != "" {
			b.send(ctx, esc(e), nil)
		} else {
			b.send(ctx, "Подробный разбор уже недоступен.", nil)
		}
	case "disp":
		id, _ := strconv.ParseInt(arg, 10, 64)
		ok, err := b.st.DisputeAttempt(id)
		switch {
		case err != nil:
			b.send(ctx, "Не нашёл эту попытку.", nil)
		case ok:
			b.send(ctx, "Пометил оценку как спорную — на расписание повторений она не повлияет.", nil)
		default:
			b.send(ctx, "Уже помечено.", nil)
		}
	}
}
