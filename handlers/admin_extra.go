// handlers/admin_extra.go — Tipovačka 2.0
// Správa Extra otázek: přidání/smazání/toggle otázek, přidělování bodů za odpovědi.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"tipovacka/config"
	"tipovacka/db"
	"tipovacka/middleware"
	"tipovacka/models"
)

// ── Levenshtein distance (case-insensitive, rune-aware) ──────────────────────

func levenshtein(a, b string) int {
	ra := []rune(strings.ToLower(a))
	rb := []rune(strings.ToLower(b))
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	row := make([]int, lb+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := row[0]
		row[0] = i
		for j := 1; j <= lb; j++ {
			old := row[j]
			if ra[i-1] == rb[j-1] {
				row[j] = prev
			} else {
				m := prev
				if row[j-1] < m {
					m = row[j-1]
				}
				if row[j] < m {
					m = row[j]
				}
				row[j] = 1 + m
			}
			prev = old
		}
	}
	return row[lb]
}

// isFuzzyMatch returns true when answer is similar but not identical to any correct variant.
func isFuzzyMatch(answer string, correctVariants []string) (bool, int) {
	norm := strings.ToLower(strings.TrimSpace(answer))
	bestDist := 999
	for _, cv := range correctVariants {
		cv = strings.ToLower(strings.TrimSpace(cv))
		if norm == cv {
			return false, 0 // exact — handled separately
		}
		d := levenshtein(norm, cv)
		if d < bestDist {
			bestDist = d
		}
	}
	maxLen := len([]rune(norm))
	for _, cv := range correctVariants {
		if l := len([]rune(strings.TrimSpace(cv))); l > maxLen {
			maxLen = l
		}
	}
	threshold := 2
	if maxLen > 8 {
		threshold = maxLen / 4
		if threshold < 2 {
			threshold = 2
		}
		if threshold > 4 {
			threshold = 4
		}
	}
	return bestDist <= threshold, bestDist
}

// ── AnswerGroup — skupina odpovědí se stejným textem ─────────────────────────

type AnswerGroup struct {
	Text        string
	NormText    string
	Count       int
	AnswerIDs   []int
	UserNames   []string
	CurPoints   *int // společná hodnota bodů; nil = různé nebo nenastaveno
	AllEval     bool // všechny mají body
	IsMatch     bool // přesná shoda se správnou odpovědí
	IsSimilar   bool // fuzzy shoda
	SimilarDist int
}

// QStats — statistika vyhodnocení jedné otázky
type QStats struct {
	Total     int
	Evaluated int
}

// buildAnswerGroups sestrojí skupiny z plochého seznamu odpovědí.
func buildAnswerGroups(answers []AnswerWithUser, correctAnswer *string) ([]*AnswerGroup, QStats) {
	groupMap := map[string]*AnswerGroup{}
	var order []string // pořadí prvního výskytu

	for _, aw := range answers {
		norm := strings.ToLower(strings.TrimSpace(aw.Answer.Answer))
		if norm == "" {
			continue
		}
		g, ok := groupMap[norm]
		if !ok {
			g = &AnswerGroup{Text: aw.Answer.Answer, NormText: norm}
			groupMap[norm] = g
			order = append(order, norm)
		}
		g.Count++
		g.AnswerIDs = append(g.AnswerIDs, aw.Answer.ID)
		g.UserNames = append(g.UserNames, aw.User.Username)
		if aw.Answer.Points != nil {
			if g.CurPoints == nil && g.Count == 1 {
				v := *aw.Answer.Points
				g.CurPoints = &v
			} else if g.CurPoints != nil && *g.CurPoints != *aw.Answer.Points {
				g.CurPoints = nil
			}
		} else if g.Count > 1 && g.CurPoints != nil {
			g.CurPoints = nil
		}
	}

	// Správná odpověď — varianty
	var variants []string
	if correctAnswer != nil && *correctAnswer != "" {
		for _, v := range strings.Split(*correctAnswer, "|") {
			v = strings.TrimSpace(v)
			if v != "" {
				variants = append(variants, v)
			}
		}
	}

	// Příznaky match / similar; AllEval
	stats := QStats{Total: len(answers)}
	evalCount := 0
	for _, aw := range answers {
		if aw.Answer.Points != nil {
			evalCount++
		}
	}
	stats.Evaluated = evalCount

	groups := make([]*AnswerGroup, 0, len(order))
	for _, norm := range order {
		g := groupMap[norm]
		if len(variants) > 0 {
			for _, v := range variants {
				if strings.ToLower(strings.TrimSpace(v)) == norm {
					g.IsMatch = true
					break
				}
			}
			if !g.IsMatch {
				g.IsSimilar, g.SimilarDist = isFuzzyMatch(g.Text, variants)
			}
		}
		allEval := true
		for _, aw := range answers {
			if strings.ToLower(strings.TrimSpace(aw.Answer.Answer)) == norm {
				if aw.Answer.Points == nil {
					allEval = false
					break
				}
			}
		}
		g.AllEval = allEval
		groups = append(groups, g)
	}

	// Sloučit všechny IsMatch skupiny do jednoho řádku
	if len(variants) > 0 {
		var merged *AnswerGroup
		var rest []*AnswerGroup
		for _, g := range groups {
			if g.IsMatch {
				if merged == nil {
					// Kanonický text = první varianta správné odpovědi
					canonical := variants[0]
					merged = &AnswerGroup{
						Text:     canonical,
						NormText: strings.ToLower(strings.TrimSpace(canonical)),
						IsMatch:  true,
						AllEval:  true,
					}
				}
				merged.Count += g.Count
				merged.AnswerIDs = append(merged.AnswerIDs, g.AnswerIDs...)
				merged.UserNames = append(merged.UserNames, g.UserNames...)
				if !g.AllEval {
					merged.AllEval = false
				}
				if g.CurPoints != nil {
					merged.CurPoints = g.CurPoints // použij body z poslední správné skupiny
				}
			} else {
				rest = append(rest, g)
			}
		}
		if merged != nil {
			// Seřadit zbytek: similar → count desc → abecedně
			for i := 0; i < len(rest)-1; i++ {
				for j := i + 1; j < len(rest); j++ {
					a, b := rest[i], rest[j]
					swap := false
					switch {
					case a.IsSimilar != b.IsSimilar:
						swap = b.IsSimilar
					case a.Count != b.Count:
						swap = b.Count > a.Count
					default:
						swap = b.NormText < a.NormText
					}
					if swap {
						rest[i], rest[j] = rest[j], rest[i]
					}
				}
			}
			groups = append([]*AnswerGroup{merged}, rest...)
		} else {
			// Žádná správná odpověď — seřadit podle count desc
			for i := 0; i < len(groups)-1; i++ {
				for j := i + 1; j < len(groups); j++ {
					a, b := groups[i], groups[j]
					swap := false
					switch {
					case a.IsSimilar != b.IsSimilar:
						swap = b.IsSimilar
					case a.Count != b.Count:
						swap = b.Count > a.Count
					default:
						swap = b.NormText < a.NormText
					}
					if swap {
						groups[i], groups[j] = groups[j], groups[i]
					}
				}
			}
		}
	} else {
		// Bez správné odpovědi — seřadit count desc
		for i := 0; i < len(groups)-1; i++ {
			for j := i + 1; j < len(groups); j++ {
				if groups[j].Count > groups[i].Count {
					groups[i], groups[j] = groups[j], groups[i]
				}
			}
		}
	}
	return groups, stats
}

// AnswerWithUser sdružuje extra odpověď s uživatelem (sdílený typ v balíčku).
type AnswerWithUser struct {
	Answer *models.ExtraAnswer
	User   *models.User
}

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

		// Per question, load answers with user; build groups
		answersByQuestion := map[int][]AnswerWithUser{}
		groupsByQuestion := map[int][]*AnswerGroup{}
		statsByQuestion := map[int]QStats{}

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

			groups, stats := buildAnswerGroups(answersByQuestion[q.ID], q.CorrectAnswer)
			groupsByQuestion[q.ID] = groups
			statsByQuestion[q.ID] = stats
		}

		// Přímý dotaz na body správné odpovědi pro každou otázku
		// (spolehlivější než CurPoints z groupů, které může být nil při smíšených hodnotách)
		correctPtsByQuestion := map[int]*int{}
		for _, q := range questions {
			if q.CorrectAnswer == nil || *q.CorrectAnswer == "" {
				continue
			}
			variants := strings.Split(*q.CorrectAnswer, "|")
			var normVariants []string
			for _, v := range variants {
				v = strings.TrimSpace(v)
				if v != "" {
					normVariants = append(normVariants, strings.ToLower(v))
				}
			}
			if len(normVariants) == 0 {
				continue
			}
			var pts *int
			_ = db.Pool.QueryRow(ctx,
				`SELECT points FROM extra_answers
				  WHERE question_id=$1 AND LOWER(TRIM(answer)) = ANY($2)
				  AND points IS NOT NULL
				  LIMIT 1`, q.ID, normVariants).Scan(&pts)
			correctPtsByQuestion[q.ID] = pts
		}

		flash := middleware.GetFlash(w, r)

		RenderTemplate(w, r, tmpl, "admin/extra_answers.html", TemplateData{
			"User":                  admin,
			"Comp":                  comp,
			"Questions":             questions,
			"AnswersByQuestion":     answersByQuestion,
			"GroupsByQuestion":      groupsByQuestion,
			"StatsByQuestion":       statsByQuestion,
			"CorrectPtsByQuestion":  correctPtsByQuestion,
			"Flash":                 flash,
			"AutoDeadline":          autoDeadline,
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

// GET /admin/extra/{competition_id}/export  — XLSX export
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

	type qInfo struct {
		ID            int
		Order         int
		Text          string
		MaxPoints     int
		CorrectAnswer string
		AnswerOptions string
		IsClosed      bool
	}
	qRows, _ := db.Pool.Query(ctx,
		`SELECT id, order_num, text, max_points, correct_answer, is_closed, answer_options
		   FROM extra_questions WHERE competition_id=$1 ORDER BY order_num, id`, compID)
	var questions []qInfo
	for qRows.Next() {
		var q qInfo
		var corr, opts *string
		_ = qRows.Scan(&q.ID, &q.Order, &q.Text, &q.MaxPoints, &corr, &q.IsClosed, &opts)
		if corr != nil {
			q.CorrectAnswer = *corr
		}
		if opts != nil {
			q.AnswerOptions = *opts
		}
		questions = append(questions, q)
	}
	qRows.Close()

	type answerRow struct {
		QuestionID   int
		QuestionText string
		Username     string
		Answer       string
		Points       string
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

	// ── Sestavit XLSX ──────────────────────────────────────────────────────
	f := excelize.NewFile()
	defer f.Close()

	// Styly
	boldStyle, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"1a3a5c"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	closedStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "888888", Italic: true},
	})

	// ── List 1: Otázky ──
	sheet1 := "Otázky"
	f.SetSheetName("Sheet1", sheet1)
	headers1 := []string{"ID", "Pořadí", "Otázka", "Body", "Správná odpověď", "Možnosti (dropdown)", "Uzavřeno"}
	for ci, h := range headers1 {
		cell, _ := excelize.CoordinatesToCellName(ci+1, 1)
		_ = f.SetCellValue(sheet1, cell, h)
		_ = f.SetCellStyle(sheet1, cell, cell, headerStyle)
	}
	_ = f.SetColWidth(sheet1, "A", "A", 14)
	_ = f.SetColWidth(sheet1, "B", "B", 8)
	_ = f.SetColWidth(sheet1, "C", "C", 45)
	_ = f.SetColWidth(sheet1, "D", "D", 8)
	_ = f.SetColWidth(sheet1, "E", "E", 25)
	_ = f.SetColWidth(sheet1, "F", "F", 35)
	_ = f.SetColWidth(sheet1, "G", "G", 10)

	for ri, q := range questions {
		row := ri + 2
		closedStr := "ne"
		if q.IsClosed {
			closedStr = "ano"
		}
		rowData := []interface{}{q.ID, q.Order, q.Text, q.MaxPoints, q.CorrectAnswer, q.AnswerOptions, closedStr}
		for ci, val := range rowData {
			cell, _ := excelize.CoordinatesToCellName(ci+1, row)
			_ = f.SetCellValue(sheet1, cell, val)
		}
		if q.IsClosed {
			startCell, _ := excelize.CoordinatesToCellName(1, row)
			endCell, _ := excelize.CoordinatesToCellName(len(headers1), row)
			_ = f.SetCellStyle(sheet1, startCell, endCell, closedStyle)
		}
	}

	// ── List 2: Odpovědi ──
	sheet2 := "Odpovědi"
	_, _ = f.NewSheet(sheet2)
	headers2 := []string{"ID otázky", "Otázka", "Uživatel", "Odpověď", "Body"}
	for ci, h := range headers2 {
		cell, _ := excelize.CoordinatesToCellName(ci+1, 1)
		_ = f.SetCellValue(sheet2, cell, h)
		_ = f.SetCellStyle(sheet2, cell, cell, headerStyle)
	}
	_ = f.SetColWidth(sheet2, "A", "A", 14)
	_ = f.SetColWidth(sheet2, "B", "B", 45)
	_ = f.SetColWidth(sheet2, "C", "C", 18)
	_ = f.SetColWidth(sheet2, "D", "D", 30)
	_ = f.SetColWidth(sheet2, "E", "E", 8)

	prevQID := -1
	for ri, a := range answers {
		row := ri + 2
		ptsVal := interface{}(nil)
		if a.Points != "" {
			if n, err2 := strconv.Atoi(a.Points); err2 == nil {
				ptsVal = n
			}
		}
		// Zvýraznit začátek každé otázky
		qIDVal := interface{}(nil)
		if a.QuestionID != prevQID {
			qIDVal = a.QuestionID
			prevQID = a.QuestionID
		}
		rowData := []interface{}{qIDVal, a.QuestionText, a.Username, a.Answer, ptsVal}
		for ci, val := range rowData {
			cell, _ := excelize.CoordinatesToCellName(ci+1, row)
			if val != nil {
				_ = f.SetCellValue(sheet2, cell, val)
			}
		}
		// Tučně username
		usernameCell, _ := excelize.CoordinatesToCellName(3, row)
		_ = f.SetCellStyle(sheet2, usernameCell, usernameCell, boldStyle)
	}

	// ── List 3: Import šablona ──
	sheet3 := "Import šablona"
	_, _ = f.NewSheet(sheet3)
	importHeaders := []string{"pořadí", "otázka", "body", "správná_odpověď", "možnosti_dropdown"}
	for ci, h := range importHeaders {
		cell, _ := excelize.CoordinatesToCellName(ci+1, 1)
		_ = f.SetCellValue(sheet3, cell, h)
		_ = f.SetCellStyle(sheet3, cell, cell, headerStyle)
	}
	_ = f.SetColWidth(sheet3, "A", "A", 10)
	_ = f.SetColWidth(sheet3, "B", "B", 45)
	_ = f.SetColWidth(sheet3, "C", "C", 8)
	_ = f.SetColWidth(sheet3, "D", "D", 25)
	_ = f.SetColWidth(sheet3, "E", "E", 40)
	// Příklad řádku
	example := []interface{}{1, "Kdo vyhraje turnaj?", 5, "Česko", "Česko\nSlovensko\nKanada"}
	for ci, val := range example {
		cell, _ := excelize.CoordinatesToCellName(ci+1, 2)
		_ = f.SetCellValue(sheet3, cell, val)
	}

	safeName := strings.ReplaceAll(comp.Name, " ", "_")
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="extra_`+safeName+`.xlsx"`)
	if err2 := f.Write(w); err2 != nil {
		log.Printf("[extra export] write error: %v", err2)
	}
}

// POST /admin/extra/{competition_id}/import  — XLSX/CSV import otázek
func AdminExtraImport(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	ctx := context.Background()

	// Verify competition exists
	var compExists bool
	_ = db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competitions WHERE id=$1)`, compID).Scan(&compExists)
	if !compExists {
		middleware.SetFlash(w, r, "error", "Soutěž nenalezena.")
		http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
		return
	}

	if err := r.ParseMultipartForm(8 << 20); err != nil {
		middleware.SetFlash(w, r, "error", "Chyba při nahrání souboru.")
		http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
		return
	}

	file, header, err := r.FormFile("import_file")
	if err != nil {
		middleware.SetFlash(w, r, "error", "Soubor nenalezen.")
		http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
		return
	}
	defer file.Close()

	// Aktuální max order_num
	var maxOrder int
	_ = db.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(order_num), 0) FROM extra_questions WHERE competition_id=$1`, compID).Scan(&maxOrder)

	// Parsuj XLSX
	var rows [][]string
	name := strings.ToLower(header.Filename)
	if strings.HasSuffix(name, ".xlsx") {
		xf, err2 := excelize.OpenReader(file)
		if err2 != nil {
			middleware.SetFlash(w, r, "error", "Nelze načíst XLSX: "+err2.Error())
			http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
			return
		}
		defer xf.Close()
		// Hledej list "Import šablona" nebo první list
		target := "Import šablona"
		sheets := xf.GetSheetList()
		found := false
		for _, s := range sheets {
			if s == target {
				found = true
				break
			}
		}
		if !found && len(sheets) > 0 {
			target = sheets[0]
		}
		rows, _ = xf.GetRows(target)
	} else {
		middleware.SetFlash(w, r, "error", "Podporovaný formát: .xlsx")
		http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
		return
	}

	// Parsuj řádky — přeskoč hlavičku (řádek 1)
	// Sloupce: pořadí | otázka | body | správná_odpověď | možnosti_dropdown
	inserted := 0
	for ri, row := range rows {
		if ri == 0 {
			continue // header
		}
		if len(row) < 2 {
			continue
		}
		text := strings.TrimSpace(row[1])
		if text == "" {
			continue
		}
		maxPts := 3
		if len(row) >= 3 && strings.TrimSpace(row[2]) != "" {
			if n, err2 := strconv.Atoi(strings.TrimSpace(row[2])); err2 == nil && n > 0 {
				maxPts = n
			}
		}
		var correctPtr *string
		if len(row) >= 4 {
			if v := strings.TrimSpace(row[3]); v != "" {
				correctPtr = &v
			}
		}
		var optsPtr *string
		if len(row) >= 5 {
			if v := strings.TrimSpace(row[4]); v != "" {
				// Normalizuj: každá řádka = jedna volba
				var opts []string
				for _, line := range strings.Split(v, "\n") {
					line = strings.TrimSpace(line)
					if line != "" {
						opts = append(opts, line)
					}
				}
				if len(opts) > 0 {
					joined := strings.Join(opts, "\n")
					optsPtr = &joined
				}
			}
		}
		maxOrder++
		if _, err2 := db.Pool.Exec(ctx,
			`INSERT INTO extra_questions (competition_id, order_num, text, max_points, correct_answer, is_closed, answer_options)
			 VALUES ($1, $2, $3, $4, $5, FALSE, $6)`,
			compID, maxOrder, text, maxPts, correctPtr, optsPtr); err2 != nil {
			log.Printf("[extra import] INSERT error row %d: %v", ri, err2)
			continue
		}
		inserted++
	}

	LogAction(&admin.ID, admin.Username, "extra_import", "competition", &compID,
		fmt.Sprintf("Import %d extra otázek ze souboru %s", inserted, header.Filename), nil, nil)

	middleware.SetFlash(w, r, "ok", fmt.Sprintf("✅ Importováno %d otázek ze souboru.", inserted))
	http.Redirect(w, r, "/admin/extra/"+strconv.Itoa(compID)+"/answers", http.StatusSeeOther)
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

	// extra_reveal_at — "reveal_now=1" = ihned, jinak datetime-local vstup, prázdné = NULL
	var revealPtr *time.Time
	if r.FormValue("reveal_now") == "1" {
		now := time.Now()
		revealPtr = &now
	} else {
		revealStr := strings.TrimSpace(r.FormValue("extra_reveal_at"))
		if revealStr != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", revealStr, time.Local)
			if err == nil {
				revealPtr = &t
			}
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

// POST /admin/competitions/{competition_id}/extra-notify (AJAX)
// Odešle emailové upozornění uživatelům kteří ještě nevyplnili extra otázky.
func AdminExtraNotifyNow(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	compID, _ := strconv.Atoi(r.PathValue("competition_id"))
	ctx := context.Background()

	// Načti info o soutěži
	var compName string
	var extraDeadline *time.Time
	err := db.Pool.QueryRow(ctx,
		`SELECT name, extra_deadline FROM competitions WHERE id=$1`, compID).
		Scan(&compName, &extraDeadline)
	if err != nil {
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}

	// Deadline text pro email
	deadlineText := ""
	loc, _ := time.LoadLocation("Europe/Prague")
	if loc == nil {
		loc = time.UTC
	}
	if extraDeadline != nil {
		deadlineText = extraDeadline.In(loc).Format("02.01. 15:04")
	} else {
		var firstMatch time.Time
		err2 := db.Pool.QueryRow(ctx,
			`SELECT MIN(m.match_date) FROM matches m
			   JOIN rounds r ON r.id = m.round_id
			  WHERE r.competition_id = $1 AND m.match_date IS NOT NULL`, compID).Scan(&firstMatch)
		if err2 == nil && !firstMatch.IsZero() {
			deadlineText = firstMatch.In(loc).Format("02.01. 15:04")
		}
	}

	// Uživatelé s opt-in
	uRows, err := db.Pool.Query(ctx, `
		SELECT u.id, u.email, u.username
		FROM users u
		JOIN notification_settings ns ON ns.user_id = u.id
		WHERE ns.competition_id = $1
		  AND u.email IS NOT NULL AND u.email != ''
		  AND COALESCE(u.is_blocked,  false) = false
		  AND COALESCE(u.is_inactive, false) = false
		  AND COALESCE(u.is_approved, true)  = true
	`, compID)
	if err != nil {
		jsonError(w, "db_error", http.StatusInternalServerError)
		return
	}
	type recipient struct {
		ID    int
		Email string
	}
	var opted []recipient
	for uRows.Next() {
		var rec recipient
		var username string
		_ = uRows.Scan(&rec.ID, &rec.Email, &username)
		opted = append(opted, rec)
	}
	uRows.Close()

	if len(opted) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"sent":0}`))
		return
	}

	// Kdo už odpověděl na všechny otázky?
	qRows, _ := db.Pool.Query(ctx,
		`SELECT id FROM extra_questions WHERE competition_id=$1`, compID)
	var qIDs []int
	for qRows.Next() {
		var qid int
		_ = qRows.Scan(&qid)
		qIDs = append(qIDs, qid)
	}
	qRows.Close()

	answered := map[int]int{}
	if len(qIDs) > 0 {
		aRows, _ := db.Pool.Query(ctx,
			`SELECT DISTINCT user_id FROM extra_answers WHERE question_id = ANY($1)`, qIDs)
		for aRows.Next() {
			var uid int
			_ = aRows.Scan(&uid)
			answered[uid]++
		}
		aRows.Close()
	}

	totalQ := len(qIDs)
	var untipped []recipient
	for _, rec := range opted {
		if answered[rec.ID] < totalQ {
			untipped = append(untipped, rec)
		}
	}

	if len(untipped) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"sent":0}`))
		return
	}

	go func() {
		appURL := config.AppURL
		extraURL := appURL + fmt.Sprintf("/extra?competition_id=%d", compID)
		subject := fmt.Sprintf("🎯 Ještě nemáš extra tipy — %s", compName)
		deadlineInfo := ""
		if deadlineText != "" {
			deadlineInfo = fmt.Sprintf(`<p style="color:#64748b;margin:.5rem 0">⏰ Deadline: <strong>%s</strong></p>`, deadlineText)
		}
		bodyHTML := fmt.Sprintf(
			`<html><body style="font-family:sans-serif;max-width:500px;margin:auto;padding:1rem;background:#f0f4f8">`+
				`<div style="background:#131f2e;color:#fff;padding:1rem 1.5rem;border-radius:8px 8px 0 0">`+
				`<h2 style="margin:0;font-size:1.1rem">🎯 Nezapomeň na extra tipy!</h2>`+
				`</div>`+
				`<div style="background:#fff;padding:1.5rem;border-radius:0 0 8px 8px;border:1px solid #dde3ea;border-top:none">`+
				`<p style="margin-top:0">Ještě nemáš vyplněné extra otázky pro <strong>%s</strong>.</p>`+
				`%s`+
				`<div style="text-align:center;margin:1.5rem 0">`+
				`<a href="%s" style="background:#10b981;color:#fff;text-decoration:none;padding:.65rem 1.8rem;border-radius:6px;font-weight:700;font-size:.95rem">Tipovat extra →</a>`+
				`</div>`+
				`<p style="color:#94a3b8;font-size:.78rem;text-align:center;margin-bottom:0">`+
				`Nastavení upozornění: <a href="%s/profile" style="color:#64748b">/profile</a>`+
				`</p>`+
				`</div>`+
				`</body></html>`,
			compName, deadlineInfo, extraURL, appURL,
		)
		for _, rec := range untipped {
			if err := notifySendEmail(rec.Email, subject, bodyHTML); err != nil {
				log.Printf("[extra-notify] email chyba → %s: %v", rec.Email, err)
			}
		}
		log.Printf("[extra-notify] %s: odesláno %d emailů", compName, len(untipped))
	}()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"total":%d}`, len(untipped))))
}

// POST /admin/extra/questions/{question_id}/set-pts-for-correct (AJAX)
// Nastaví body pro všechny správné varianty, ostatním 0.
func AdminExtraSetPtsForCorrect(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	qID, _ := strconv.Atoi(r.PathValue("question_id"))
	if err := r.ParseForm(); err != nil {
		jsonError(w, "bad_request", http.StatusBadRequest)
		return
	}
	ptsStr := strings.TrimSpace(r.FormValue("points"))
	ctx := context.Background()

	var compID, maxPts int
	var correctAnswer *string
	err := db.Pool.QueryRow(ctx,
		`SELECT competition_id, max_points, correct_answer FROM extra_questions WHERE id=$1`, qID).
		Scan(&compID, &maxPts, &correctAnswer)
	if err != nil {
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}
	if correctAnswer == nil || strings.TrimSpace(*correctAnswer) == "" {
		jsonError(w, "no_correct_answer", http.StatusBadRequest)
		return
	}

	pts, e := strconv.Atoi(ptsStr)
	if e != nil || pts < 0 {
		jsonError(w, "invalid_points", http.StatusBadRequest)
		return
	}

	var normVariants []string
	for _, v := range strings.Split(*correctAnswer, "|") {
		v = strings.TrimSpace(v)
		if v != "" {
			normVariants = append(normVariants, strings.ToLower(v))
		}
	}

	_, _ = db.Pool.Exec(ctx,
		`UPDATE extra_answers SET points=$1 WHERE question_id=$2 AND LOWER(TRIM(answer)) = ANY($3)`,
		pts, qID, normVariants)
	_, _ = db.Pool.Exec(ctx,
		`UPDATE extra_answers SET points=0 WHERE question_id=$1 AND LOWER(TRIM(answer)) != ALL($2)`,
		qID, normVariants)

	go RecalculateStandings(compID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "points": pts})
}

// POST /admin/extra/questions/{question_id}/clear-correct (AJAX)
// Vymaže správnou odpověď a resetuje všechny body na NULL pro danou otázku.
func AdminExtraClearCorrect(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	qID, _ := strconv.Atoi(r.PathValue("question_id"))
	ctx := context.Background()

	var compID int
	err := db.Pool.QueryRow(ctx, `SELECT competition_id FROM extra_questions WHERE id=$1`, qID).Scan(&compID)
	if err != nil {
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}

	_, _ = db.Pool.Exec(ctx, `UPDATE extra_questions SET correct_answer=NULL WHERE id=$1`, qID)
	_, _ = db.Pool.Exec(ctx, `UPDATE extra_answers SET points=NULL WHERE question_id=$1`, qID)

	go RecalculateStandings(compID)

	LogAction(&admin.ID, admin.Username, "extra_answer", "question", &qID,
		"Reset správné odpovědi a bodů", nil, nil)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// POST /admin/extra/questions/{question_id}/set-correct-group (AJAX)
// Nastaví správnou odpověď pro otázku a přiřadí body všem odpovědím v dané skupině.
func AdminExtraSetCorrectGroup(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}
	qID, _ := strconv.Atoi(r.PathValue("question_id"))
	if err := r.ParseForm(); err != nil {
		jsonError(w, "bad_request", http.StatusBadRequest)
		return
	}

	answerText := strings.TrimSpace(r.FormValue("answer_text"))
	ptsStr := r.FormValue("points")
	ctx := context.Background()

	// Načti otázku
	var compID, maxPts int
	var curCorrect *string
	err := db.Pool.QueryRow(ctx,
		`SELECT competition_id, max_points, correct_answer FROM extra_questions WHERE id=$1`, qID).
		Scan(&compID, &maxPts, &curCorrect)
	if err != nil {
		jsonError(w, "not_found", http.StatusNotFound)
		return
	}

	// Body — admin může zadat libovolnou kladnou hodnotu
	pts := maxPts
	if ptsStr != "" {
		if p, e := strconv.Atoi(ptsStr); e == nil && p >= 0 {
			pts = p
		}
	}

	// Nastav správnou odpověď — přidej jako variantu (nenahrazuj stávající)
	newCorrect := answerText
	if curCorrect != nil && strings.TrimSpace(*curCorrect) != "" {
		// Zkontroluj jestli varianta už existuje
		already := false
		normNew := strings.ToLower(strings.TrimSpace(answerText))
		for _, v := range strings.Split(*curCorrect, "|") {
			if strings.ToLower(strings.TrimSpace(v)) == normNew {
				already = true
				break
			}
		}
		if !already {
			newCorrect = *curCorrect + "|" + answerText
		} else {
			newCorrect = *curCorrect // beze změny
		}
	}
	_, _ = db.Pool.Exec(ctx,
		`UPDATE extra_questions SET correct_answer=$1 WHERE id=$2`, newCorrect, qID)

	// Přiřaď body VŠEM správným variantám (nejen té právě kliknuté)
	allVariants := strings.Split(newCorrect, "|")
	var normVariants []string
	for _, v := range allVariants {
		v = strings.TrimSpace(v)
		if v != "" {
			normVariants = append(normVariants, strings.ToLower(v))
		}
	}

	// Správné skupiny → přiřazené body
	res, _ := db.Pool.Exec(ctx,
		`UPDATE extra_answers SET points=$1
		  WHERE question_id=$2 AND LOWER(TRIM(answer)) = ANY($3)`,
		pts, qID, normVariants)
	count := res.RowsAffected()

	// Nesprávné skupiny → 0 bodů (jen ty co nejsou v žádné variantě)
	_, _ = db.Pool.Exec(ctx,
		`UPDATE extra_answers SET points=0
		  WHERE question_id=$1 AND LOWER(TRIM(answer)) != ALL($2)`,
		qID, normVariants)

	go RecalculateStandings(compID)

	newVal := fmt.Sprintf("%q → %d b", answerText, pts)
	LogAction(&admin.ID, admin.Username, "extra_answer", "question", &qID,
		fmt.Sprintf("Správná odpověď nastavena: %q, %d tipérů → %d b", answerText, count, pts),
		nil, &newVal)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":             true,
		"evaluated":      count,
		"correct_answer": answerText,
		"points":         pts,
	})
}
