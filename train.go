package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

type Exercise struct {
	Kind      string      `json:"kind"` // situation | combo | dialogue
	ItemID    int64       `json:"item_id,omitempty"`
	GrammarID string      `json:"grammar_id,omitempty"`
	Gen       ExerciseGen `json:"gen"`
	Targets   []int64     `json:"targets,omitempty"`
	Role      string      `json:"role,omitempty"`
}

// Live — состояние текущего задания; текст диалога живёт только в памяти.
type Live struct {
	SessionID int64
	Ex        *Exercise
	Attempts  int
	UsedHint  bool
	Dialog    []DTurn
	UserTurns int
	Spont     map[int64]bool
}

func (b *Bot) startTraining(ctx context.Context) {
	if s, _ := b.st.ActiveSession(); s != nil {
		if b.live != nil && b.live.SessionID == s.ID {
			b.send(ctx, "Тренировка уже идёт — отвечай на текущее задание или нажми «Закончить».", nil)
			return
		}
		b.resume(ctx)
		return
	}
	items := b.st.SelectTraining(b.cfg.SessionTasks, b.cfg.NewPerSession)
	if len(items) == 0 {
		b.send(ctx, "Пока нечего тренировать: повторений на сегодня нет, а новых выражений не осталось. Пришли новый материал.", nil)
		return
	}
	plan := Plan{Items: items, Grammar: make([]string, len(items))}
	if g := b.st.GrammarForPractice(); len(g) > 0 {
		for i := range items {
			if i%2 == 1 { // каждое второе задание — совмещение с грамматикой
				plan.Grammar[i] = g[(i/2)%len(g)]
			}
		}
	}
	id, err := b.st.NewSession(plan)
	if err != nil {
		b.send(ctx, "Ошибка базы данных, не получилось начать.", nil)
		return
	}
	b.live = &Live{SessionID: id, Spont: map[int64]bool{}}
	b.send(ctx, fmt.Sprintf("Начинаем: %s и мини-диалог. Отвечай по-английски своими словами.", plural(len(items), "задание", "задания", "заданий")), nil)
	b.nextStep(ctx, 0)
}

func (b *Bot) resume(ctx context.Context) {
	s, _ := b.st.ActiveSession()
	if s == nil {
		b.send(ctx, "Активной тренировки нет.", kb(row(btn("Тренироваться", "train"))))
		return
	}
	// После перезапуска продолжаем новым заданием на том же шаге.
	b.live = &Live{SessionID: s.ID, Spont: map[int64]bool{}}
	b.nextStep(ctx, s.Step)
}

func (b *Bot) stopTraining(ctx context.Context) {
	s, _ := b.st.ActiveSession()
	if s == nil {
		b.send(ctx, "Активной тренировки нет.", nil)
		return
	}
	b.finish(ctx, s.ID)
}

func (b *Bot) finish(ctx context.Context, sid int64) {
	b.st.FinishSession(sid)
	b.live = nil
	st := b.st.SessionStats(sid)
	var parts []string
	add := func(k, label string) {
		if st[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", label, st[k]))
		}
	}
	add(ResIndependent, "сам вспомнил")
	add(ResHinted, "с подсказкой")
	add(ResFailed, "не вспомнил")
	add(ResExposed, "посмотрел ответ")
	add(ResUncertain, "спорно")
	add("spontaneous", "сам использовал в диалоге")
	msg := "Тренировка завершена."
	if len(parts) > 0 {
		msg += "\n" + strings.Join(parts, " · ")
	}
	b.send(ctx, msg, menu)
}

func exButtons() *Inline {
	return kb(row(btn("Подсказка", "hint"), btn("Показать ответ", "show")), row(btn("Пропустить", "skip"), btn("Закончить", "stop")))
}

// nextStep показывает задание номер step; после заданий — мини-диалог, затем итог.
func (b *Bot) nextStep(ctx context.Context, step int) {
	if b.live == nil {
		return
	}
	s, _ := b.st.ActiveSession()
	if s == nil || s.ID != b.live.SessionID {
		b.live = nil
		return
	}
	n := len(s.Plan.Items)
	for ; step < n; step++ {
		it, err := b.st.Item(s.Plan.Items[step])
		if err != nil {
			continue // единицу удалили (отмена добавления)
		}
		var gt *Topic
		if gid := s.Plan.Grammar[step]; gid != "" {
			gt, _ = b.st.Topic(gid)
		}
		b.tg.Typing(ctx, b.chatID)
		gen, err := b.llm.GenerateExercise(ctx, it, gt)
		if err != nil && gt != nil { // попробуем без грамматики
			gt = nil
			gen, err = b.llm.GenerateExercise(ctx, it, nil)
		}
		if err != nil {
			b.st.UpdateSession(s.ID, step, "")
			b.send(ctx, "Не получилось создать задание: "+failText(err)+". Тренировка приостановлена.",
				kb(row(btn("Попробовать снова", "resume"), btn("Закончить", "stop"))))
			b.live = nil
			return
		}
		ex := &Exercise{Kind: "situation", ItemID: it.ID, Gen: *gen}
		head := fmt.Sprintf("<b>Задание %d/%d</b>", step+1, n)
		if gt != nil {
			ex.Kind, ex.GrammarID = "combo", gt.ID
			head += " · грамматика: " + esc(gt.Title)
		}
		b.st.UpdateSession(s.ID, step, mustJSON(ex))
		b.live.Ex, b.live.Attempts, b.live.UsedHint = ex, 0, false
		b.send(ctx, fmt.Sprintf("%s\n\n%s\n\n%s", head, esc(gen.SituationRU), esc(gen.TaskRU)), exButtons())
		return
	}
	if step == n {
		b.startDialogue(ctx, s, n)
		return
	}
	b.finish(ctx, s.ID)
}

func (b *Bot) startDialogue(ctx context.Context, s *Session, step int) {
	var items []*Item
	for _, id := range s.Plan.Items {
		if it, err := b.st.Item(id); err == nil {
			items = append(items, it)
		}
		if len(items) == 3 {
			break
		}
	}
	if len(items) == 0 {
		b.finish(ctx, s.ID)
		return
	}
	b.tg.Typing(ctx, b.chatID)
	ds, err := b.llm.StartDialogue(ctx, items)
	if err != nil {
		b.send(ctx, "Мини-диалог не получился: "+failText(err)+".", nil)
		b.finish(ctx, s.ID)
		return
	}
	ex := &Exercise{Kind: "dialogue", Role: ds.RoleRU}
	for _, it := range items {
		ex.Targets = append(ex.Targets, it.ID)
	}
	b.st.UpdateSession(s.ID, step, mustJSON(ex))
	b.live.Ex, b.live.Dialog, b.live.UserTurns = ex, []DTurn{{Bot: true, Text: ds.OpeningEN}}, 0
	b.send(ctx, fmt.Sprintf("<b>Мини-диалог</b> (до %d реплик)\n%s\n\n%s", b.cfg.DialogueTurns, esc(ds.RoleRU), esc(ds.OpeningEN)),
		kb(row(btn("Закончить", "stop"))))
}

func (b *Bot) onAnswer(ctx context.Context, s *Session, text string, ref MsgRef) {
	if b.live == nil || b.live.SessionID != s.ID || b.live.Ex == nil {
		b.send(ctx, "Есть незавершённая тренировка. Продолжим? (Чтобы добавить лексику, используй <code>/add …</code>)",
			kb(row(btn("Продолжить", "resume"), btn("Завершить", "stop"))))
		return
	}
	refID, _ := b.st.InsertRef(ref) // только идентификаторы ответа, без текста
	if b.live.Ex.Kind == "dialogue" {
		b.dialogueTurn(ctx, s, text, refID)
		return
	}
	ex := b.live.Ex
	it, err := b.st.Item(ex.ItemID)
	if err != nil {
		b.nextStep(ctx, s.Step+1)
		return
	}
	var gt *Topic
	if ex.GrammarID != "" {
		gt, _ = b.st.Topic(ex.GrammarID)
	}
	b.tg.Typing(ctx, b.chatID)
	ev, err := b.llm.Evaluate(ctx, ex, it, gt, text)
	if err != nil {
		b.send(ctx, "Не удалось проверить ответ: "+failText(err)+". Попробуй отправить его ещё раз или пропусти задание.",
			kb(row(btn("Пропустить", "skip"), btn("Закончить", "stop"))))
		return
	}
	b.live.Attempts++
	lexRes, gramRes := Classify(ev, b.live.UsedHint)
	if gt == nil {
		gramRes = ""
	}
	a := Attempt{SessionID: s.ID, RefID: refID, ExerciseType: ex.Kind, AttemptNo: b.live.Attempts, Result: lexRes,
		GrammarRes: gramRes, UsedHint: b.live.UsedHint, Eval: ev, Correction: strings.Join(ev.Corrections, "; "),
		LexID: it.ID, GrammarID: ex.GrammarID}
	// Расписание меняет только первая попытка: повтор не даёт двойного повышения.
	if b.live.Attempts == 1 {
		a.Snapshot = b.applySRS(it.ID, lexRes, ex.GrammarID, gramRes)
	}
	aid, err := b.st.AddAttempt(a)
	if err != nil {
		log.Printf("db add attempt: %v", err)
	}
	b.expl[aid] = ev.ExplanationRU

	var sb strings.Builder
	sb.WriteString(esc(ev.FeedbackRU))
	if ev.MeaningOK && ev.NaturalOK && !ev.TargetProduced {
		sb.WriteString("\n\nПо-английски сказано верно, но целевое выражение из твоего словаря пока не прозвучало.")
	}
	for _, c := range ev.Corrections {
		sb.WriteString("\n• " + esc(c))
	}
	if lexRes == ResUncertain {
		sb.WriteString("\n\nОценка неоднозначна — интервал повторения не изменён.")
	}
	retry := b.live.Attempts == 1 && (lexRes == ResFailed || gramRes == ResFailed)
	aidS := strconv.FormatInt(aid, 10)
	if retry {
		sb.WriteString("\n\nПопробуй сказать это ещё раз.")
		b.send(ctx, sb.String(), kb(
			row(btn("Подсказка", "hint"), btn("Показать ответ", "show")),
			row(btn("Подробнее", "expl:"+aidS), btn("Не согласен", "disp:"+aidS)),
			row(btn("Дальше", "next"), btn("Закончить", "stop"))))
		return
	}
	if b.live.Attempts > 1 {
		sb.WriteString("\n\nОбразец: " + esc(ex.Gen.ModelAnswer))
	}
	b.send(ctx, sb.String(), kb(row(btn("Подробнее", "expl:"+aidS), btn("Не согласен", "disp:"+aidS))))
	b.nextStep(ctx, s.Step+1)
}

// applySRS применяет результат и возвращает снимок прежних состояний для отката.
func (b *Bot) applySRS(itemID int64, lexRes, gramID, gramRes string) []Review {
	at := time.Now()
	var snap []Review
	if lexRes != ResUncertain {
		r := b.st.Review("lex", strconv.FormatInt(itemID, 10))
		snap = append(snap, r)
		b.st.SaveReview(ApplyOutcome(r, lexRes, at, b.cfg.Ladder))
	}
	if gramID != "" && gramRes != "" && gramRes != ResUncertain {
		r := b.st.Review("gram", gramID)
		snap = append(snap, r)
		b.st.SaveReview(ApplyOutcome(r, gramRes, at, b.cfg.Ladder))
	}
	return snap
}

func (b *Bot) trainingButton(ctx context.Context, kind string) {
	s, _ := b.st.ActiveSession()
	if s == nil {
		b.send(ctx, "Активной тренировки нет.", nil)
		return
	}
	if b.live == nil || b.live.SessionID != s.ID || b.live.Ex == nil {
		b.resume(ctx)
		return
	}
	ex := b.live.Ex
	if ex.Kind == "dialogue" {
		return
	}
	switch kind {
	case "hint":
		b.live.UsedHint = true
		h := ex.Gen.HintRU
		if h == "" {
			h = "Подумай о разговорном выражении из твоего словаря."
		}
		b.send(ctx, "Подсказка: "+esc(h), kb(row(btn("Показать ответ", "show"), btn("Пропустить", "skip"))))
	case "show":
		it, err := b.st.Item(ex.ItemID)
		if err == nil {
			if b.live.Attempts == 0 { // ответ показан до попытки — это не воспроизведение
				snap := b.applySRS(it.ID, ResExposed, "", "")
				_, _ = b.st.AddAttempt(Attempt{SessionID: s.ID, ExerciseType: ex.Kind, AttemptNo: 1, Result: ResExposed,
					UsedHint: b.live.UsedHint, Snapshot: snap, LexID: it.ID, GrammarID: ex.GrammarID})
			}
			b.send(ctx, fmt.Sprintf("<b>%s</b> — %s\nОбразец: %s", esc(it.CanonicalForm), esc(it.MeaningRU), esc(ex.Gen.ModelAnswer)), nil)
		}
		b.nextStep(ctx, s.Step+1)
	case "skip", "next":
		b.nextStep(ctx, s.Step+1)
	}
}

func (b *Bot) dialogueTurn(ctx context.Context, s *Session, text string, refID int64) {
	var items []*Item
	for _, id := range b.live.Ex.Targets {
		if it, err := b.st.Item(id); err == nil {
			items = append(items, it)
		}
	}
	last := b.live.UserTurns+1 >= b.cfg.DialogueTurns
	b.tg.Typing(ctx, b.chatID)
	r, err := b.llm.ContinueDialogue(ctx, b.live.Ex.Role, items, b.live.Dialog, text, last)
	if err != nil {
		b.send(ctx, "Не удалось продолжить диалог: "+failText(err)+". Отправь реплику ещё раз или закончи.", kb(row(btn("Закончить", "stop"))))
		return
	}
	b.live.UserTurns++
	b.live.Dialog = append(b.live.Dialog, DTurn{Text: text}, DTurn{Bot: true, Text: r.ReplyEN})
	var used []string
	for _, id := range r.UsedTargets {
		if b.live.Spont[id] {
			continue
		}
		b.live.Spont[id] = true
		rv := b.st.Review("lex", strconv.FormatInt(id, 10))
		rv.Spontaneous++
		b.st.SaveReview(rv)
		_, _ = b.st.AddAttempt(Attempt{SessionID: s.ID, RefID: refID, ExerciseType: "dialogue", AttemptNo: 1,
			Result: ResIndependent, Spontaneous: true, LexID: id})
		for _, it := range items {
			if it.ID == id {
				used = append(used, "<b>"+esc(it.CanonicalForm)+"</b>")
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(esc(r.ReplyEN))
	if len(r.Corrections) > 0 {
		sb.WriteString("\n\n<i>Поправки:</i>")
		for _, c := range r.Corrections {
			sb.WriteString("\n• " + esc(c))
		}
	}
	if len(used) > 0 {
		sb.WriteString("\n\nСам использовал: " + strings.Join(used, ", "))
	}
	if last || r.Done {
		b.send(ctx, sb.String(), nil)
		b.finish(ctx, s.ID)
		return
	}
	b.send(ctx, sb.String(), kb(row(btn("Закончить", "stop"))))
}
