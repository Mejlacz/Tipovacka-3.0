// handlers/admin_extra.go — Tipovačka 2.0
// Správa Extra otázek: přidání/smazání/toggle otázek, přidělování bodů za odpovědi.
package handlers

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// GET /admin/extra/{competition_id}/questions/new
func AdminExtraQuestionNewForm(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder)
		if err != nil {
			http.Redirect(w, r, "/extra", http.StatusSeeOther)
			return
		}

		// Načti pouze aktivní soutěže pro výběr týmů
		compRows, _ := db.Pool.Query(ctx,
			`SELECT id, name, season FROM competitions WHERE is_active = TRUE ORDER BY sort_order ASC NULLS LAST, id DESC`)
		var competitions []*models.Competition
		for compRows.Next() {
			c := &models.Competition{}
			_ = compRows.Scan(&c.ID, &c.Name, &c.Season)
			competitions = append(competitions, c)
		}
		compRows.Close()

		RenderTemplate(w, r, tmpl, "admin/extra_question_new.html", TemplateData{
			"User":         admin,
			"Comp":         comp,
			"Competitions": competitions,
		})
	}
}

// POST /admin/extra/{competition_id}/questions/new
func AdminExtraQuestionNewSubmit(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	text := strings.TrimSpace(r.FormValue("text"))
	maxPts, _ := strconv.Atoi(r.FormValue("max_points"))
	if maxPts < 1 {
		maxPts = 3
	}
	correctAnswer := strings.TrimSpace(r.FormValue("correct_answer"))
	// answer_options: newline-separated list; only if checkbox "use_dropdown" checked
	useDropdown := r.FormValue("use_dropdown") == "1"
	answerOptionsRaw := strings.TrimSpace(r.FormValue("answer_options"))

	ctx := context.Background()

	// Verify competition exists
	var compExists bool
	_ = db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competitions WHERE id=$1)`, compID).Scan(&compExists)
	if !compExists {
		http.Redirect(w, r, "/extra", http.StatusSeeOther)
		return
	}

	// Next order_num
	var orderNum int
	_ = db.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(order_num), 0) + 1 FROM extra_questions WHERE competition_id=$1`, compID).
		Scan(&orderNum)

	var correctPtr *string
	if correctAnswer != "" {
		correctPtr = &correctAnswer
	}
	var answerOptionsPtr *string
	if useDropdown && answerOptionsRaw != "" {
		// Normalizuj: odstraň prázdné řádky, trim každou volbu
		var opts []string
		for _, line := range strings.Split(answerOptionsRaw, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				opts = append(opts, line)
			}
		}
		if len(opts) > 0 {
			joined := strings.Join(opts, "\n")
			answerOptionsPtr = &joined
		}
	}

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO extra_questions (competition_id, order_num, text, max_points, correct_answer, is_closed, answer_options)
		 VALUES ($1, $2, $3, $4, $5, FALSE, $6)`,
		compID, orderNum, text, maxPts, correctPtr, answerOptionsPtr); err != nil {
		log.Printf("[extra_question] INSERT error: %v", err)
		middleware.SetFlash(w, r, "error", "Chyba při ukládání otázky: "+err.Error())
		http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/questions/new", http.StatusSeeOther)
		return
	}

	middleware.SetFlash(w, r, "ok", "Otázka přidána.")
	http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
}

// POST /admin/extra/questions/{question_id}/delete
func AdminExtraQuestionDelete(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	qID, _ := strconv.Atoi(r.PathValue("question_id"))
	ctx := context.Background()

	var compID int
	_ = db.Pool.QueryRow(ctx, `SELECT competition_id FROM extra_questions WHERE id=$1`, qID).Scan(&compID)
	_, _ = db.Pool.Exec(ctx, `DELETE FROM extra_questions WHERE id=$1`, qID)

	if compID > 0 {
		http.Redirect(w, r, "/extra?competition_id="+strconv.Itoa(compID), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/extra", http.StatusSeeOther)
	}
}

// POST /admin/extra/questions/{question_id}/toggle-close
func AdminExtraQuestionToggleClose(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	qID, _ := strconv.Atoi(r.PathValue("question_id"))
	ctx := context.Background()

	var compID int
	_ = db.Pool.QueryRow(ctx, `SELECT competition_id FROM extra_questions WHERE id=$1`, qID).Scan(&compID)
	_, _ = db.Pool.Exec(ctx,
		`UPDATE extra_questions SET is_closed = NOT is_closed WHERE id=$1`, qID)

	if compID > 0 {
		http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/extra", http.StatusSeeOther)
	}
}

// GET /admin/extra/{competition_id}/answers
func AdminExtraAnswersView(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := RequireAdmin(w, r)
		if admin == nil {
			return
		}
		compID, _ := strconv.Atoi(r.PathValue("competition_id"))
		ctx := context.Background()

		comp := &models.Competition{}
		err := db.Pool.QueryRow(ctx,
			`SELECT id, name, season, is_active, sport, sort_order, extra_deadline, extra_reveal_at FROM competitions WHERE id=$1`, compID).
			Scan(&comp.ID, &comp.Name, &comp.Season, &comp.IsActive, &comp.Sport, &comp.SortOrder, &comp.ExtraDeadline, &comp.ExtraRevealAt)
		if err != nil {
			http.Redirect(w, r, "/extra", http.StatusSeeOther)
			return
		}

		// Výpočet auto-deadline (první zápas)
		var autoDeadline *time.Time
		var firstMatch time.Time
		errFM := db.Pool.QueryRow(ctx,
			`SELECT MIN(m.match_date) FROM matches m
			   JOIN rounds r ON r.id = m.round_id
			  WHERE r.competition_id = $1 AND m.match_date IS NOT NULL`, compID).Scan(&firstMatch)
		if errFM == nil && !firstMatch.IsZero() {
			autoDeadline = &firstMatch
		}

		qRows, _ := db.Pool.Query(ctx,
			`SELECT id, competition_id, order_num, text, max_points, correct_answer, is_closed, answer_options
			   FROM extra_questions WHERE competition_id=$1 ORDER BY order_num, id`, compID)
		var questions []*models.ExtraQuestion
		for qRows.Next() {
			q := &models.ExtraQuestion{}
			_ = qRows.Scan(&q.ID, &q.CompetitionID, &q.OrderNum, &q.Text, &q.MaxPoints, &q.CorrectAnswer, &q.IsClosed, &q.AnswerOptions)
			questions = append(questions, q)
		}
		qRows.Close()

		// Per question, load answers with user
		type AnswerWithUser struct {
			Answer *models.ExtraAnswer
			User   *models.User
		}
		answersByQuestion := map[int][]AnswerWithUser{}
		for _, q := range questions {
			aRows, _ := db.Pool.Query(ctx,
				`SELECT ea.id, ea.question_id, ea.user_id, ea.answer, ea.points, ea.created_at,
				        u.id, u.username
				   FROM extra_answers ea
				   JOIN users u ON u.id = ea.user_id
				  WHERE ea.question_id=$1
				  ORDER BY u.username`, q.ID)
			for aRows.Next() {
				ea := &models.ExtraAnswer{}
				u := &models.User{}
				_ = aRows.Scan(&ea.ID, &ea.QuestionID, &ea.UserID, &ea.Answer, &ea.Points, &ea.CreatedAt,
					&u.ID, &u.Username)
				ea.User = u
				answersByQuestion[q.ID] = append(answersByQuestion[q.ID], AnswerWithUser{ea, u})
			}
			aRows.Close()
		}

		flash := middleware.GetFlash(w, r)

		RenderTemplate(w, r, tmpl, "admin/extra_answers.html", TemplateData{
			"User":              admin,
			"Comp":              comp,
			"Questions":         questions,
			"AnswersByQuestion": answersByQuestion,
			"Flash":             flash,
			"AutoDeadline":      autoDeadline,
		})
	}
}

// POST /admin/extra/{competition_id}/answers
func AdminExtraAnswersSave(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	ctx := context.Background()

	saved := 0
	for key, vals := range r.Form {
		val := vals[0]
		if strings.HasPrefix(key, "pts_") {
			ansIDStr := key[4:]
			ansID, err := strconv.Atoi(ansIDStr)
			if err != nil {
				continue
			}
			var pts *int
			if strings.TrimSpace(val) != "" {
				n, err := strconv.Atoi(val)
				if err == nil {
					pts = &n
				}
			}
			_, _ = db.Pool.Exec(ctx, `UPDATE extra_answers SET points=$1 WHERE id=$2`, pts, ansID)
			saved++
		} else if strings.HasPrefix(key, "correct_") {
			qIDStr := key[8:]
			qID, err := strconv.Atoi(qIDStr)
			if err != nil {
				continue
			}
			corrVal := strings.TrimSpace(val)
			var corrPtr *string
			if corrVal != "" {
				corrPtr = &corrVal
			}
			_, _ = db.Pool.Exec(ctx, `UPDATE extra_questions SET correct_answer=$1 WHERE id=$2`, corrPtr, qID)
		}
	}

	// Recalculate standings
	go RecalculateStandings(compID)

	middleware.SetFlash(w, r, "ok", fmt.Sprintf("Uloženo bodování (%d odpovědí).", saved))
	http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
}

// POST /admin/extra/{competition_id}/auto-evaluate
func AdminExtraAutoEvaluate(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	qFilterStr := r.URL.Query().Get("q")
	var qFilter int
	if qFilterStr != "" {
		qFilter, _ = strconv.Atoi(qFilterStr)
	}
	ctx := context.Background()

	qRows, _ := db.Pool.Query(ctx,
		`SELECT id, max_points, correct_answer FROM extra_questions
		  WHERE competition_id=$1 AND correct_answer IS NOT NULL AND correct_answer != ''`, compID)
	type qInfo struct {
		ID            int
		MaxPoints     int
		CorrectAnswer string
	}
	var questions []qInfo
	for qRows.Next() {
		var q qInfo
		_ = qRows.Scan(&q.ID, &q.MaxPoints, &q.CorrectAnswer)
		questions = append(questions, q)
	}
	qRows.Close()

	if qFilter > 0 {
		var filtered []qInfo
		for _, q := range questions {
			if q.ID == qFilter {
				filtered = append(filtered, q)
			}
		}
		questions = filtered
	}

	evaluated := 0
	for _, q := range questions {
		// Build set of valid variants (case-insensitive, trimmed)
		variants := map[string]bool{}
		for _, v := range strings.Split(q.CorrectAnswer, "|") {
			v = strings.TrimSpace(strings.ToLower(v))
			if v != "" {
				variants[v] = true
			}
		}

		aRows, _ := db.Pool.Query(ctx,
			`SELECT id, answer FROM extra_answers WHERE question_id=$1`, q.ID)
		type ansRow struct{ id int; answer string }
		var answers []ansRow
		for aRows.Next() {
			var a ansRow
			_ = aRows.Scan(&a.id, &a.answer)
			answers = append(answers, a)
		}
		aRows.Close()

		for _, a := range answers {
			ans := strings.TrimSpace(strings.ToLower(a.answer))
			var pts int
			if variants[ans] {
				pts = q.MaxPoints
			}
			_, _ = db.Pool.Exec(ctx, `UPDATE extra_answers SET points=$1 WHERE id=$2`, pts, a.id)
			evaluated++
		}
	}

	go RecalculateStandings(compID)
	_ = admin

	middleware.SetFlash(w, r, "ok", fmt.Sprintf("🎯 Vyhodnoceno %d odpovědí dle správné odpovědi.", evaluated))
	http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
}

// GET /admin/extra/{competition_id}/export  — CSV export
func AdminExtraExport(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	ctx := context.Background()

	comp := &models.Competition{}
	err := db.Pool.QueryRow(ctx,
		`SELECT id, name FROM competitions WHERE id=$1`, compID).Scan(&comp.ID, &comp.Name)
	if err != nil {
		http.Redirect(w, r, "/extra", http.StatusSeeOther)
		return
	}

	qRows, _ := db.Pool.Query(ctx,
		`SELECT id, order_num, text, max_points, correct_answer, is_closed
		   FROM extra_questions WHERE competition_id=$1 ORDER BY order_num, id`, compID)
	type qInfo struct {
		ID            int
		Order         int
		Text          string
		MaxPoints     int
		CorrectAnswer string
		IsClosed      bool
	}
	var questions []qInfo
	for qRows.Next() {
		var q qInfo
		var corr *string
		_ = qRows.Scan(&q.ID, &q.Order, &q.Text, &q.MaxPoints, &corr, &q.IsClosed)
		if corr != nil {
			q.CorrectAnswer = *corr
		}
		questions = append(questions, q)
	}
	qRows.Close()

	// Answers
	type answerRow struct {
		QuestionID int
		QuestionText string
		Username   string
		Answer     string
		Points     string
	}
	var answers []answerRow
	if len(questions) > 0 {
		qIDs := make([]int, len(questions))
		qTexts := map[int]string{}
		for i, q := range questions {
			qIDs[i] = q.ID
			qTexts[q.ID] = q.Text
		}
		aRows, _ := db.Pool.Query(ctx,
			`SELECT ea.question_id, u.username, ea.answer, ea.points
			   FROM extra_answers ea
			   JOIN users u ON u.id = ea.user_id
			  WHERE ea.question_id = ANY($1)
			  ORDER BY ea.question_id, u.username`, qIDs)
		for aRows.Next() {
			var ar answerRow
			var pts *int
			_ = aRows.Scan(&ar.QuestionID, &ar.Username, &ar.Answer, &pts)
			ar.QuestionText = qTexts[ar.QuestionID]
			if pts != nil {
				ar.Points = strconv.Itoa(*pts)
			}
			answers = append(answers, ar)
		}
		aRows.Close()
	}

	safeName := strings.ReplaceAll(comp.Name, " ", "_")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="extra_`+safeName+`.csv"`)

	cw := csv.NewWriter(w)
	// Section 1: Questions
	_ = cw.Write([]string{"=== OTÁZKY ==="})
	_ = cw.Write([]string{"id", "order", "text", "max_points", "correct_answer", "is_closed"})
	for _, q := range questions {
		closed := "0"
		if q.IsClosed {
			closed = "1"
		}
		_ = cw.Write([]string{
			strconv.Itoa(q.ID), strconv.Itoa(q.Order), q.Text,
			strconv.Itoa(q.MaxPoints), q.CorrectAnswer, closed,
		})
	}
	_ = cw.Write([]string{})
	// Section 2: Answers
	_ = cw.Write([]string{"=== ODPOVĚDI ==="})
	_ = cw.Write([]string{"question_id", "question_text", "username", "answer", "points"})
	for _, a := range answers {
		_ = cw.Write([]string{
			strconv.Itoa(a.QuestionID), a.QuestionText, a.Username, a.Answer, a.Points,
		})
	}
	cw.Flush()
}

// POST /admin/extra/answers/set-ajax  — admin sets extra answer for a user (AJAX)
func AdminSetExtraAnswerAjax(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"ok":false,"error":"forbidden"}`))
		return
	}
	if err := r.ParseForm(); err != nil {
		jsonError(w, "bad_request", http.StatusBadRequest)
		return
	}
	questionID, _ := strconv.Atoi(r.FormValue("question_id"))
	userID, _ := strconv.Atoi(r.FormValue("user_id"))
	subIndex, _ := strconv.Atoi(r.FormValue("sub_index"))
	answerText := strings.TrimSpace(r.FormValue("answer_text"))

	ctx := context.Background()

	// Load question
	q := &models.ExtraQuestion{}
	err := db.Pool.QueryRow(ctx,
		`SELECT id, competition_id, order_num, text, max_points, correct_answer, is_closed, answer_options
		   FROM extra_questions WHERE id=$1`, questionID).
		Scan(&q.ID, &q.CompetitionID, &q.OrderNum, &q.Text, &q.MaxPoints, &q.CorrectAnswer, &q.IsClosed, &q.AnswerOptions)
	if err != nil {
		jsonError(w, "question_not_found", http.StatusNotFound)
		return
	}

	// Verify user exists
	var uname string
	_ = db.Pool.QueryRow(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&uname)
	if uname == "" {
		jsonError(w, "user_not_found", http.StatusNotFound)
		return
	}

	// Upsert answer
	var existingID int
	var existingAnswer string
	_ = db.Pool.QueryRow(ctx,
		`SELECT id, answer FROM extra_answers WHERE question_id=$1 AND user_id=$2`, questionID, userID).
		Scan(&existingID, &existingAnswer)

	if existingID > 0 {
		// Update the specific sub_index, keep other parts
		parts := strings.Split(existingAnswer, "|~~|")
		for len(parts) <= subIndex {
			parts = append(parts, "")
		}
		parts[subIndex] = answerText
		newAnswer := strings.Join(parts, "|~~|")
		_, _ = db.Pool.Exec(ctx,
			`UPDATE extra_answers SET answer=$1, points=NULL WHERE id=$2`, newAnswer, existingID)
	} else {
		// New answer — fill sub_index, rest empty
		numParts := len(strings.Split(q.Text, "|~~|"))
		if numParts < subIndex+1 {
			numParts = subIndex + 1
		}
		parts := make([]string, numParts)
		parts[subIndex] = answerText
		newAnswer := strings.Join(parts, "|~~|")
		_ = db.Pool.QueryRow(ctx,
			`INSERT INTO extra_answers (question_id, user_id, answer, created_at) VALUES ($1,$2,$3,NOW()) RETURNING id`,
			questionID, userID, newAnswer).Scan(&existingID)
	}

	// Audit log
	LogAction(&admin.ID, admin.Username, "admin_set_extra_answer", "extra_answer", &existingID,
		fmt.Sprintf("Extra odpověď za %s: q=%d[%d] '%s'", uname, questionID, subIndex, answerText),
		nil, nil)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"answer":` + jsonStr(answerText) + `}`))
}

// GET /admin/extra/teams-ajax?competition_id=X  — vrátí JSON pole názvů týmů soutěže
func AdminExtraTeamsAjax(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`[]`))
		return
	}
	compID, _ := strconv.Atoi(r.URL.Query().Get("competition_id"))
	if compID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
		return
	}
	ctx := context.Background()
	// Načti týmy přes competition_teams i přímý FK (UNION odstraní duplikáty)
	rows, err := db.Pool.Query(ctx, `
		SELECT DISTINCT COALESCE(t.display_name, t.name)
		FROM teams t
		JOIN competition_teams ct ON ct.team_id = t.id
		WHERE ct.competition_id = $1
		UNION
		SELECT DISTINCT COALESCE(display_name, name)
		FROM teams
		WHERE competition_id = $1
		ORDER BY 1`, compID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
		return
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	jsonBytes, _ := json.Marshal(names)
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes)
}

// POST /admin/extra/{competition_id}/deadline-settings
// Nastavuje extra_deadline a extra_reveal_at pro soutěž.
func AdminExtraDeadlineSettings(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	ctx := context.Background()

	// extra_deadline — prázdné = auto (první zápas)
	deadlineStr := strings.TrimSpace(r.FormValue("extra_deadline"))
	var deadlinePtr *time.Time
	if deadlineStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", deadlineStr, time.Local)
		if err == nil {
			deadlinePtr = &t
		}
	}

	// extra_reveal_at — prázdné = auto (shodné s deadline)
	revealStr := strings.TrimSpace(r.FormValue("extra_reveal_at"))
	var revealPtr *time.Time
	if revealStr == "now" {
		now := time.Now()
		revealPtr = &now
	} else if revealStr != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", revealStr, time.Local)
		if err == nil {
			revealPtr = &t
		}
	}

	_, err := db.Pool.Exec(ctx,
		`UPDATE competitions SET extra_deadline=$1, extra_reveal_at=$2 WHERE id=$3`,
		deadlinePtr, revealPtr, compID)
	if err != nil {
		log.Printf("[extra deadline] UPDATE error: %v", err)
		middleware.SetFlash(w, r, "error", "Chyba při ukládání: "+err.Error())
	} else {
		middleware.SetFlash(w, r, "ok", "Nastavení deadline/reveal uloženo.")
	}

	http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
}
