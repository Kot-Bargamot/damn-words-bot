package main

import (
	"fmt"
	"strings"
)

func normPoint(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// ApplyProposal вносит изменения в тему. mode: "" (конфликтов нет), "replace", "clarify".
func ApplyProposal(t *Topic, p *GProposal, refID int64, mode string) int {
	changed := 0
	for _, c := range p.Conflicts {
		if c.ExistingIndex < 0 || c.ExistingIndex >= len(t.Points) {
			continue
		}
		switch mode {
		case "replace":
			t.Points[c.ExistingIndex].Text = c.NewText
			t.Points[c.ExistingIndex].Sources = []int64{refID}
			changed++
		case "clarify":
			t.Points = append(t.Points, GPoint{Section: t.Points[c.ExistingIndex].Section,
				Text: "Уточнение: " + c.NewText, Sources: []int64{refID}})
			changed++
		}
	}
	for _, np := range p.Points {
		dup := false
		for i := range t.Points {
			if t.Points[i].Section == np.Section && normPoint(t.Points[i].Text) == normPoint(np.Text) {
				dup = true
				if !containsID(t.Points[i].Sources, refID) {
					t.Points[i].Sources = append(t.Points[i].Sources, refID)
				}
			}
		}
		if !dup {
			t.Points = append(t.Points, GPoint{Section: np.Section, Text: strings.TrimSpace(np.Text), Sources: []int64{refID}})
			changed++
		}
	}
	return changed
}

func containsID(s []int64, v int64) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// RenderTopic формирует Markdown-документ. Источники — только ссылки на сообщения.
func RenderTopic(t *Topic, refs map[int64]MsgRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nid: %s\ntitle: %s\n---\n\n# %s\n", t.ID, yamlStr(t.Title), t.Title)

	order := []int64{}
	num := map[int64]int{}
	for _, p := range t.Points {
		for _, s := range p.Sources {
			if _, ok := num[s]; !ok {
				order = append(order, s)
				num[s] = len(order)
			}
		}
	}
	for _, sec := range Sections {
		var lines []string
		for _, p := range t.Points {
			if p.Section != sec {
				continue
			}
			var marks []string
			for _, s := range p.Sources {
				marks = append(marks, fmt.Sprintf("[%d]", num[s]))
			}
			lines = append(lines, "- "+strings.ReplaceAll(p.Text, "\n", " ")+" "+strings.Join(marks, ""))
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "\n## %s\n\n%s\n", SectionTitles[sec], strings.Join(lines, "\n"))
		}
	}
	if len(order) > 0 {
		b.WriteString("\n## Источники\n\n")
		for i, id := range order {
			r, ok := refs[id]
			switch {
			case ok && r.URL != "":
				fmt.Fprintf(&b, "%d. [Сообщение в Telegram](%s)\n", i+1, r.URL)
			case ok:
				fmt.Fprintf(&b, "%d. Сообщение Telegram: chat `%d`, message `%d` — прямая ссылка недоступна для этого типа чата\n", i+1, r.ChatID, r.MessageID)
			default:
				fmt.Fprintf(&b, "%d. Источник недоступен\n", i+1)
			}
		}
	}
	return b.String()
}

func yamlStr(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func topicSourceIDs(t *Topic) []int64 {
	var ids []int64
	for _, p := range t.Points {
		for _, s := range p.Sources {
			if !containsID(ids, s) {
				ids = append(ids, s)
			}
		}
	}
	return ids
}
