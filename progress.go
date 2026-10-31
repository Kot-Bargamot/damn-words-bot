package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (b *Bot) progress(ctx context.Context) {
	st := b.st
	t := time.Now().Unix()
	active := st.count(`SELECT COUNT(*) FROM lexical_items WHERE status='active'`)
	if active == 0 {
		b.send(ctx, "Словарь пока пуст — пришли слово, фразу или текст.", nil)
		return
	}
	join := `FROM lexical_items i JOIN review_state r ON r.target_type='lex' AND r.target_id=CAST(i.id AS TEXT) WHERE i.status='active'`
	used := st.count(`SELECT COUNT(*) ` + join + ` AND (r.independent_count>0 OR r.spontaneous_count>0)`)
	due := st.count(`SELECT COUNT(*) `+join+` AND r.last_reviewed_at>0 AND r.next_due_at<=?`, t)
	fresh := st.count(`SELECT COUNT(*) ` + join + ` AND r.last_reviewed_at=0`)
	sessions := st.count(`SELECT COUNT(*) FROM sessions WHERE started_at>=?`, t-7*86400)
	checked := st.count(`SELECT COUNT(*) FROM attempts WHERE attempt_no=1 AND spontaneous_usage=0 AND result IN ('independent','hinted','failed','exposed')`)
	indep := st.count(`SELECT COUNT(*) FROM attempts WHERE attempt_no=1 AND spontaneous_usage=0 AND result='independent'`)
	lowData := st.count(`SELECT COUNT(*) ` + join + ` AND (r.independent_count+r.hinted_count+r.failed_count)<3`)

	var sb strings.Builder
	sb.WriteString("<b>Прогресс</b>\n\n")
	fmt.Fprintf(&sb, "Выражений в словаре: %d\nИспользовано самостоятельно: %d\nК повторению сейчас: %d (+ новых: %d)\nТренировок за 7 дней: %d\n",
		active, used, due, fresh, sessions)
	if checked > 0 {
		fmt.Fprintf(&sb, "Самостоятельных воспроизведений: %d%% (%d из %d)\n", indep*100/checked, indep, checked)
	}
	if lowData > 0 {
		fmt.Fprintf(&sb, "Мало данных для оценки (меньше 3 попыток): %d\n", lowData)
	}

	rows, err := st.db.Query(`SELECT i.canonical_form, r.failed_count ` + join + ` AND r.failed_count>=2 ORDER BY r.failed_count DESC LIMIT 5`)
	if err == nil {
		var lines []string
		for rows.Next() {
			var f string
			var n int
			_ = rows.Scan(&f, &n)
			lines = append(lines, fmt.Sprintf("• %s — %d", esc(f), n))
		}
		rows.Close()
		if len(lines) > 0 {
			sb.WriteString("\n<b>Часто не вспоминаются:</b>\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	rows, err = st.db.Query(`SELECT g.title FROM grammar_topics g JOIN review_state r ON r.target_type='gram' AND r.target_id=g.id
		WHERE r.failed_count>0 AND r.next_due_at<=? ORDER BY r.failed_count DESC LIMIT 5`, t+86400)
	if err == nil {
		var lines []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			lines = append(lines, "• "+esc(s))
		}
		rows.Close()
		if len(lines) > 0 {
			sb.WriteString("\n<b>Грамматика, требующая практики:</b>\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	b.send(ctx, sb.String(), kb(row(btn("Тренироваться", "train"))))
}
