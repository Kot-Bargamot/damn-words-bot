package main

import "time"

const (
	ResIndependent = "independent"
	ResHinted      = "hinted"
	ResFailed      = "failed"
	ResExposed     = "exposed"
	ResUncertain   = "uncertain"
	ResDisputed    = "disputed"
)

// ApplyOutcome — детерминированная лестница интервалов; модель на неё не влияет.
func ApplyOutcome(r Review, result string, at time.Time, ladder []int) Review {
	day := int64(24 * 60 * 60)
	switch result {
	case ResIndependent:
		r.Stage++
		if r.Stage >= len(ladder) {
			r.Stage = len(ladder) - 1
		}
		r.NextDue = at.Unix() + int64(ladder[r.Stage])*day
		r.Independent++
	case ResHinted:
		r.NextDue = at.Unix() + day
		r.Hinted++
	case ResFailed, ResExposed:
		r.Stage = 0
		r.NextDue = at.Unix() + day
		r.Failed++
	default: // uncertain, disputed — без изменений
		return r
	}
	r.LastReviewed = at.Unix()
	return r
}

// Classify превращает независимые критерии проверки в категорию результата.
func Classify(e *Eval, usedHint bool) (lex, gram string) {
	if e.Confidence == "low" {
		return ResUncertain, gramRes(e, ResUncertain)
	}
	lex = ResFailed
	if e.TargetProduced && e.FormOK {
		lex = ResIndependent
		if usedHint {
			lex = ResHinted
		}
	}
	switch e.Grammar {
	case "ok":
		gram = ResIndependent
		if usedHint {
			gram = ResHinted
		}
	case "error", "not_attempted":
		gram = ResFailed
	}
	return
}

func gramRes(e *Eval, r string) string {
	if e.Grammar == "not_applicable" {
		return ""
	}
	return r
}
