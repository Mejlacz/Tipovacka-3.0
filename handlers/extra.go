// handlers/extra.go — Tipovačka 2.0
// Extra otázky ke soutěži — uživatelský pohled + AJAX uložení odpovědí.
package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/models"
)



// GET /extra
func ExtraView(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireApproved(w, r)
		if user == nil {
			return
		}
		ctx := context.Background()

		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season, is_active, sport, sort_order, extra_deadline, extra_reveal_at
			   FROM competitions WHERE is_active = TRUE AND COALESCE(is_hidden,false)=false ORDER BY sort_order ASC NULLS LAST, id DESC`)
		var competitions []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season, &c.IsActive, &c.Sport, &c.SortOrder, &c.ExtraDeadline, &c.ExtraRevealAt)
			competitions = append(competitions, c)
		}
		compRows.Close()

		// Determine selected competition
		var compID int
		if v := r.URL.Query().Get("competition_id"); v != "" {
			compID, _ = strconv.Atoi(v)
		}
		if compID == 0 && len(competitions) > 0 {
			// Default: first active or first
			for _, c := range competitions {
				if c.IsActive {
					compID = c.ID
					break
				}
			}
			if compID == 0 {
				compID = competitions[0].ID
			}
		}

		var comp *models.Competition
		for _, c := range competitions {
			if c.ID == compID {
				comp = c
				break
			}
		}

		var questions []*models.ExtraQuestion
		answersMap := map[int]*models.ExtraAnswer{} // question_id → answer

		// ── Výpočet deadline a reveal ──────────────────────────────────────
		var effectiveDeadline *time.Time
		var isLocked bool
		var isRevealed bool

		if comp != nil {
			if comp.ExtraDeadline != nil {
				// Admin zadal vlastní deadline
				effectiveDeadline = comp.ExtraDeadline
			} else {
				// Auto: začátek prvního zápasu v soutěži
				var firstMatch time.Time
				err := db.Pool.QueryRow(ctx,
					`SELECT MIN(m.match_date) FROM matches m
					   JOIN rounds r ON r.id = m.round_id
					  WHERE r.competition_id = $1 AND m.match_date IS NOT NULL`, comp.ID).
					Scan(&firstMatch)
				if err == nil && !firstMatch.IsZero() {
					effectiveDeadline = &firstMatch
				}
			}

			if effectiveDeadline != nil {
				isLocked = time.Now().After(*effectiveDeadline)
			}

			// Reveal: pouze pokud admin explicitně nastavil extra_reveal_at (klik "Odkrýt všem")
			// NULL = ještě neodhaleno, i když deadline prošel
			if comp.ExtraRevealAt != nil {
				isRevealed = time.Now().After(*comp.ExtraRevealAt)
			}
		}

		if comp != nil {
			qRows, _ := db.Pool.Query(ctx,
				`SELECT id, competition_id, order_num, text, max_points, correct_answer, is_closed, answer_options
				   FROM extra_questions WHERE competition_id=$1 ORDER BY order_num, id`, comp.ID)
			for qRows.Next() {
				q := &models.ExtraQuestion{}
				_ = qRows.Scan(&q.ID, &q.CompetitionID, &q.OrderNum, &q.Text, &q.MaxPoints, &q.CorrectAnswer, &q.IsClosed, &q.AnswerOptions)
				// Přepis IsClosed: competition-level lock platí pro všechny otázky
				if isLocked {
					q.IsClosed = true
				}
				questions = append(questions, q)
			}
			qRows.Close()

			if len(questions) > 0 {
				qIDs := make([]int, len(questions))
				for i, q := range questions {
					qIDs[i] = q.ID
				}
				aRows, _ := db.Pool.Query(ctx,
					`SELECT id, question_id, user_id, answer, points, created_at
					   FROM extra_answers WHERE question_id = ANY($1) AND user_id=$2`,
					qIDs, user.ID)
				for aRows.Next() {
					a := &models.ExtraAnswer{}
					_ = aRows.Scan(&a.ID, &a.QuestionID, &a.UserID, &a.Answer, &a.Points, &a.CreatedAt)
					answersMap[a.QuestionID] = a
				}
				aRows.Close()
			}
		}

		RenderTemplate(w, r, tmpl, "extra.html", TemplateData{
			"User":                  user,
			"Competitions":          competitions,
			"Comp":                  comp,
			"Questions":             questions,
			"AnswersMap":            answersMap,
			"SelectedCompetitionID": compID,
			"EffectiveDeadline":     effectiveDeadline,
			"IsLocked":              isLocked,
			"IsRevealed":            isRevealed,
		})
	}
}

// POST /extra/save-ajax  (AJAX, JSON response)
func ExtraSaveAjax(w http.ResponseWriter, r *http.Request) {
	user := GetCurrentUser(r)
	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error":"not_logged_in"}`))
		return
	}
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"error":"bad_request"}`))
		return
	}
	questionID, _ := strconv.Atoi(r.FormValue("question_id"))
	answer := strings.TrimSpace(r.FormValue("answer"))

	ctx := context.Background()

	// Load question
	q := &models.ExtraQuestion{}
	err := db.Pool.QueryRow(ctx,
		`SELECT id, competition_id, order_num, text, max_points, correct_answer, is_closed, answer_options
		   FROM extra_questions WHERE id=$1`, questionID).
		Scan(&q.ID, &q.CompetitionID, &q.OrderNum, &q.Text, &q.MaxPoints, &q.CorrectAnswer, &q.IsClosed, &q.AnswerOptions)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"ok":false,"error":"question_not_found"}`))
		return
	}
	if q.IsClosed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"ok":false,"error":"closed"}`))
		return
	}

	// Zkontroluj competition-level deadline
	var compExtraDeadline *time.Time
	var compExtraRevealAt *time.Time
	_ = db.Pool.QueryRow(ctx,
		`SELECT extra_deadline, extra_reveal_at FROM competitions WHERE id=$1`, q.CompetitionID).
		Scan(&compExtraDeadline, &compExtraRevealAt)

	if compExtraDeadline != nil && time.Now().After(*compExtraDeadline) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"ok":false,"error":"closed"}`))
		return
	}
	// Pokud není custom deadline, zkontroluj první zápas soutěže
	if compExtraDeadline == nil {
		var firstMatch time.Time
		errFM := db.Pool.QueryRow(ctx,
			`SELECT MIN(m.match_date) FROM matches m
			   JOIN rounds r ON r.id = m.round_id
			  WHERE r.competition_id = $1 AND m.match_date IS NOT NULL`, q.CompetitionID).
			Scan(&firstMatch)
		if errFM == nil && !firstMatch.IsZero() && time.Now().After(firstMatch) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"ok":false,"error":"closed"}`))
			return
		}
	}
	_ = compExtraRevealAt // jen pro budoucí použití

	// Upsert answer
	var existingID int
	var oldAnswer string
	wasNew := true
	_ = db.Pool.QueryRow(ctx,
		`SELECT id, answer FROM extra_answers WHERE question_id=$1 AND user_id=$2`,
		questionID, user.ID).Scan(&existingID, &oldAnswer)
	if existingID > 0 {
		wasNew = false
		_, _ = db.Pool.Exec(ctx,
			`UPDATE extra_answers SET answer=$1 WHERE id=$2`, answer, existingID)
	} else {
		_ = db.Pool.QueryRow(ctx,
			`INSERT INTO extra_answers (question_id, user_id, answer, created_at) VALUES ($1,$2,$3,NOW()) RETURNING id`,
			questionID, user.ID, answer).Scan(&existingID)
	}

	// Log action
	qLabel := q.Text
	if len(qLabel) > 40 {
		qLabel = qLabel[:40]
	}
	var desc string
	if wasNew {
		desc = "Extra [" + qLabel + "]: '" + answer + "'"
	} else {
		desc = "Změna extra [" + qLabel + "]: '" + oldAnswer + "' → '" + answer + "'"
	}

	var oldStrPtr *string
	if !wasNew {
		s := `{"answer":` + jsonStr(oldAnswer) + `}`
		oldStrPtr = &s
	}
	newStr := `{"answer":` + jsonStr(answer) + `}`

	LogAction(&user.ID, user.Username, "extra_save", "extra_answer", &existingID,
		desc, oldStrPtr, &newStr)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"answer":` + jsonStr(answer) + `}`))
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
