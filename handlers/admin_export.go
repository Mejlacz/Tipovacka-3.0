// handlers/admin_export.go — Tipovačka 2.0
// CSV export: tipy soutěže, kompletní záloha, týmy, zápasy, uživatelé.
// XLSX export pomocí excelize.
package handlers

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/xuri/excelize/v2"
	"tipovacka/db"
)

// ─── GET /admin/tips/{competition_id}/export ───────────────────────────────────
// Stáhne CSV se všemi tipy dané soutěže (pro zálohu/obnovu).
// Sloupce: tip_id,user_id,username,match_id,datum,domaci,hoste,tip_home,tip_away,body

func AdminTipsExportCSV(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	compID, err := strconv.Atoi(chi.URLParam(r, "competition_id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := context.Background()

	// Verify competition exists
	var compName string
	err = db.Pool.QueryRow(ctx, `SELECT name FROM competitions WHERE id=$1`, compID).Scan(&compName)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT t.id, t.user_id, u.username, t.match_id,
		       m.match_date,
		       ht.name, at.name,
		       t.home_score, t.away_score, t.points
		FROM tips t
		JOIN users u ON u.id = t.user_id
		JOIN matches m ON m.id = t.match_id
		JOIN rounds ro ON ro.id = m.round_id
		JOIN teams ht ON ht.id = m.home_team_id
		JOIN teams at ON at.id = m.away_team_id
		WHERE ro.competition_id = $1
		ORDER BY m.match_date, t.user_id`, compID)
	if err != nil {
		http.Error(w, "DB error", 500)
		return
	}
	defer rows.Close()

	today := time.Now().Format("2006-01-02")
	safeName := sanitizeFilename(compName, 30)
	filename := fmt.Sprintf("tipy_%s_%s.csv", safeName, today)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	wr := csv.NewWriter(w)
	// UTF-8 BOM so Excel opens it correctly
	w.Write([]byte{0xEF, 0xBB, 0xBF}) //nolint
	wr.Write([]string{"tip_id", "user_id", "username", "match_id", "datum", "domaci", "hoste", "tip_home", "tip_away", "body"}) //nolint

	for rows.Next() {
		var tipID, userID, matchID, tipHome, tipAway int
		var username, homeTeam, awayTeam string
		var matchDate *time.Time
		var points *int
		_ = rows.Scan(&tipID, &userID, &username, &matchID, &matchDate, &homeTeam, &awayTeam, &tipHome, &tipAway, &points)

		dateStr := ""
		if matchDate != nil {
			dateStr = matchDate.Format("02.01.2006 15:04")
		}
		pointsStr := ""
		if points != nil {
			pointsStr = strconv.Itoa(*points)
		}
		wr.Write([]string{ //nolint
			strconv.Itoa(tipID),
			strconv.Itoa(userID),
			username,
			strconv.Itoa(matchID),
			dateStr,
			homeTeam,
			awayTeam,
			strconv.Itoa(tipHome),
			strconv.Itoa(tipAway),
			pointsStr,
		})
	}
	wr.Flush()
}

// ─── GET /admin/export/csv ─────────────────────────────────────────────────────
// Obecný CSV export: ?type=tips|matches|teams|users|leaderboard
// Nepovinné filtry: ?competition_id=X &only_finished=1

func AdminGeneralExportCSV(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	ctx := context.Background()
	exportType := r.URL.Query().Get("type")
	compIDStr := r.URL.Query().Get("competition_id")
	onlyFinished := r.URL.Query().Get("only_finished") == "1"

	var compID int
	if compIDStr != "" {
		compID, _ = strconv.Atoi(compIDStr)
	}

	today := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("tipovacka_%s_%s.csv", exportType, today)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write([]byte{0xEF, 0xBB, 0xBF}) //nolint
	wr := csv.NewWriter(w)

	switch exportType {
	case "teams":
		wr.Write([]string{"id", "nazev", "alias", "soutez"}) //nolint
		q := `SELECT t.id, t.name, COALESCE(t.display_name,''), COALESCE(c.name,'')
		      FROM teams t LEFT JOIN competitions c ON c.id = t.competition_id`
		args := []interface{}{}
		if compID > 0 {
			q += " WHERE t.competition_id=$1"
			args = append(args, compID)
		}
		q += " ORDER BY t.name"
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		for rows.Next() {
			var id int
			var name, alias, comp string
			_ = rows.Scan(&id, &name, &alias, &comp)
			wr.Write([]string{strconv.Itoa(id), name, alias, comp}) //nolint
		}

	case "matches":
		wr.Write([]string{"id", "soutez", "kolo", "datum", "domaci", "hoste", "score_d", "score_h", "odehra"}) //nolint
		q := `SELECT m.id, c.name, ro.name, m.match_date, ht.name, at.name,
		             m.home_score, m.away_score, m.is_finished
		      FROM matches m
		      JOIN rounds ro ON ro.id = m.round_id
		      JOIN competitions c ON c.id = ro.competition_id
		      JOIN teams ht ON ht.id = m.home_team_id
		      JOIN teams at ON at.id = m.away_team_id
		      WHERE 1=1`
		args := []interface{}{}
		idx := 1
		if compID > 0 {
			q += fmt.Sprintf(" AND ro.competition_id=$%d", idx)
			args = append(args, compID)
			idx++
		}
		if onlyFinished {
			q += fmt.Sprintf(" AND m.is_finished=$%d", idx)
			args = append(args, true)
		}
		q += " ORDER BY m.match_date"
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		for rows.Next() {
			var id int
			var compName, roundName, homeTeam, awayTeam string
			var matchDate *time.Time
			var homeScore, awayScore *int
			var isFinished bool
			_ = rows.Scan(&id, &compName, &roundName, &matchDate, &homeTeam, &awayTeam, &homeScore, &awayScore, &isFinished)
			dateStr := ""
			if matchDate != nil {
				dateStr = matchDate.Format("02.01.2006 15:04")
			}
			scoreH, scoreA := "", ""
			if homeScore != nil {
				scoreH = strconv.Itoa(*homeScore)
			}
			if awayScore != nil {
				scoreA = strconv.Itoa(*awayScore)
			}
			finished := "Ne"
			if isFinished {
				finished = "Ano"
			}
			wr.Write([]string{strconv.Itoa(id), compName, roundName, dateStr, homeTeam, awayTeam, scoreH, scoreA, finished}) //nolint
		}

	case "tips":
		wr.Write([]string{"tip_id", "uzivatel", "soutez", "kolo", "zapas", "tip_d", "tip_h", "result_d", "result_h", "body"}) //nolint
		q := `SELECT t.id, u.username, c.name, ro.name,
		             ht.name || ' – ' || at.name,
		             t.home_score, t.away_score,
		             m.home_score, m.away_score, t.points
		      FROM tips t
		      JOIN users u ON u.id = t.user_id
		      JOIN matches m ON m.id = t.match_id
		      JOIN rounds ro ON ro.id = m.round_id
		      JOIN competitions c ON c.id = ro.competition_id
		      JOIN teams ht ON ht.id = m.home_team_id
		      JOIN teams at ON at.id = m.away_team_id
		      WHERE 1=1`
		args := []interface{}{}
		idx := 1
		if compID > 0 {
			q += fmt.Sprintf(" AND ro.competition_id=$%d", idx)
			args = append(args, compID)
			idx++
		}
		if onlyFinished {
			q += fmt.Sprintf(" AND m.is_finished=$%d", idx)
			args = append(args, true)
		}
		q += " ORDER BY m.match_date, t.user_id"
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		for rows.Next() {
			var tipID int
			var username, compName, roundName, matchName string
			var tipH, tipA int
			var resH, resA, points *int
			_ = rows.Scan(&tipID, &username, &compName, &roundName, &matchName, &tipH, &tipA, &resH, &resA, &points)
			resHStr, resAStr, ptsStr := "", "", ""
			if resH != nil {
				resHStr = strconv.Itoa(*resH)
			}
			if resA != nil {
				resAStr = strconv.Itoa(*resA)
			}
			if points != nil {
				ptsStr = strconv.Itoa(*points)
			}
			wr.Write([]string{strconv.Itoa(tipID), username, compName, roundName, matchName, strconv.Itoa(tipH), strconv.Itoa(tipA), resHStr, resAStr, ptsStr}) //nolint
		}

	case "users":
		wr.Write([]string{"nick", "jmeno", "prijmeni", "email", "role", "registrace"}) //nolint
		rows, _ := db.Pool.Query(ctx,
			`SELECT username, COALESCE(first_name,''), COALESCE(last_name,''), COALESCE(email,''),
			        is_owner, is_admin, created_at
			 FROM users ORDER BY username`)
		defer rows.Close()
		for rows.Next() {
			var username, first, last, email string
			var isOwner, isAdmin bool
			var createdAt *time.Time
			_ = rows.Scan(&username, &first, &last, &email, &isOwner, &isAdmin, &createdAt)
			role := "user"
			if isOwner {
				role = "owner"
			} else if isAdmin {
				role = "admin"
			}
			createdStr := ""
			if createdAt != nil {
				createdStr = createdAt.Format("02.01.2006")
			}
			wr.Write([]string{username, first, last, email, role, createdStr}) //nolint
		}

	case "leaderboard":
		wr.Write([]string{"poradi", "nick", "body", "presne_3b", "winner_1b", "miss_0b", "pocet"}) //nolint

		type lbRow struct {
			username string
			points   int
			exact    int
			winner   int
			miss     int
			count    int
		}
		byUser := map[string]*lbRow{}

		q := `SELECT u.username, t.points
		      FROM tips t
		      JOIN users u ON u.id = t.user_id
		      JOIN matches m ON m.id = t.match_id
		      JOIN rounds ro ON ro.id = m.round_id
		      WHERE t.points IS NOT NULL`
		args := []interface{}{}
		if compID > 0 {
			q += " AND ro.competition_id=$1"
			args = append(args, compID)
		}
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		for rows.Next() {
			var username string
			var points int
			_ = rows.Scan(&username, &points)
			row, ok := byUser[username]
			if !ok {
				row = &lbRow{username: username}
				byUser[username] = row
			}
			row.points += points
			row.count++
			switch points {
			case 3:
				row.exact++
			case 1:
				row.winner++
			default:
				row.miss++
			}
		}

		// Sort by points desc
		type ranked struct {
			rank int
			lbRow
		}
		sorted := make([]lbRow, 0, len(byUser))
		for _, v := range byUser {
			sorted = append(sorted, *v)
		}
		// bubble sort for simplicity
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].points > sorted[i].points {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		for rank, row := range sorted {
			wr.Write([]string{ //nolint
				strconv.Itoa(rank + 1),
				row.username,
				strconv.Itoa(row.points),
				strconv.Itoa(row.exact),
				strconv.Itoa(row.winner),
				strconv.Itoa(row.miss),
				strconv.Itoa(row.count),
			})
		}

	default:
		http.Error(w, "Neznámý typ exportu. Použij ?type=teams|matches|tips|users|leaderboard", 400)
		return
	}

	wr.Flush()
}

// ─── GET /admin/export/xlsx ───────────────────────────────────────────────────
// Obecný XLSX export: ?type=tips|matches|teams|users|leaderboard
// Nepovinné filtry: ?competition_id=X &only_finished=1

func AdminGeneralExportXLSX(w http.ResponseWriter, r *http.Request) {
	admin := RequireAdmin(w, r)
	if admin == nil {
		return
	}
	ctx := context.Background()
	exportType := r.URL.Query().Get("type")
	compIDStr := r.URL.Query().Get("competition_id")
	onlyFinished := r.URL.Query().Get("only_finished") == "1"

	var compID int
	if compIDStr != "" {
		compID, _ = strconv.Atoi(compIDStr)
	}

	f := excelize.NewFile()
	defer f.Close()

	// Helper: write header row with bold style
	styleID, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"1a3a5c"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})

	writeHeader := func(sheet string, headers []string) {
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, styleID)
		}
	}

	writeRow := func(sheet string, rowIdx int, vals []interface{}) {
		for i, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(i+1, rowIdx)
			_ = f.SetCellValue(sheet, cell, v)
		}
	}

	switch exportType {
	case "teams":
		sheet := "Týmy"
		_ = f.SetSheetName("Sheet1", sheet)
		writeHeader(sheet, []string{"ID", "Název", "Alias", "Soutěž"})
		q := `SELECT t.id, t.name, COALESCE(t.display_name,''), COALESCE(c.name,'')
		      FROM teams t LEFT JOIN competitions c ON c.id = t.competition_id`
		args := []interface{}{}
		if compID > 0 {
			q += " WHERE t.competition_id=$1"
			args = append(args, compID)
		}
		q += " ORDER BY t.name"
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		rowIdx := 2
		for rows.Next() {
			var id int
			var name, alias, comp string
			_ = rows.Scan(&id, &name, &alias, &comp)
			writeRow(sheet, rowIdx, []interface{}{id, name, alias, comp})
			rowIdx++
		}

	case "matches":
		sheet := "Zápasy"
		_ = f.SetSheetName("Sheet1", sheet)
		writeHeader(sheet, []string{"ID", "Soutěž", "Kolo", "Datum", "Domácí", "Hosté", "Skóre", "Odehrán"})
		q := `SELECT m.id, c.name, ro.name, m.match_date, ht.name, at.name,
		             m.home_score, m.away_score, m.is_finished
		      FROM matches m
		      JOIN rounds ro ON ro.id = m.round_id
		      JOIN competitions c ON c.id = ro.competition_id
		      JOIN teams ht ON ht.id = m.home_team_id
		      JOIN teams at ON at.id = m.away_team_id
		      WHERE 1=1`
		args := []interface{}{}
		idx := 1
		if compID > 0 {
			q += fmt.Sprintf(" AND ro.competition_id=$%d", idx)
			args = append(args, compID)
			idx++
		}
		if onlyFinished {
			q += fmt.Sprintf(" AND m.is_finished=$%d", idx)
			args = append(args, true)
		}
		q += " ORDER BY m.match_date"
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		rowIdx := 2
		for rows.Next() {
			var id int
			var compName, roundName, homeTeam, awayTeam string
			var matchDate *time.Time
			var homeScore, awayScore *int
			var isFinished bool
			_ = rows.Scan(&id, &compName, &roundName, &matchDate, &homeTeam, &awayTeam, &homeScore, &awayScore, &isFinished)
			dateStr := ""
			if matchDate != nil {
				dateStr = matchDate.Format("02.01.2006 15:04")
			}
			score := ""
			if homeScore != nil && awayScore != nil {
				score = strconv.Itoa(*homeScore) + ":" + strconv.Itoa(*awayScore)
			}
			finished := "Ne"
			if isFinished {
				finished = "Ano"
			}
			writeRow(sheet, rowIdx, []interface{}{id, compName, roundName, dateStr, homeTeam, awayTeam, score, finished})
			rowIdx++
		}

	case "tips":
		sheet := "Tipy"
		_ = f.SetSheetName("Sheet1", sheet)
		writeHeader(sheet, []string{"ID tipu", "Uživatel", "Soutěž", "Kolo", "Zápas", "Tip D", "Tip H", "Výsledek D", "Výsledek H", "Body"})
		q := `SELECT t.id, u.username, c.name, ro.name,
		             ht.name || ' – ' || at.name,
		             t.home_score, t.away_score,
		             m.home_score, m.away_score, t.points
		      FROM tips t
		      JOIN users u ON u.id = t.user_id
		      JOIN matches m ON m.id = t.match_id
		      JOIN rounds ro ON ro.id = m.round_id
		      JOIN competitions c ON c.id = ro.competition_id
		      JOIN teams ht ON ht.id = m.home_team_id
		      JOIN teams at ON at.id = m.away_team_id
		      WHERE 1=1`
		args := []interface{}{}
		idx := 1
		if compID > 0 {
			q += fmt.Sprintf(" AND ro.competition_id=$%d", idx)
			args = append(args, compID)
			idx++
		}
		if onlyFinished {
			q += fmt.Sprintf(" AND m.is_finished=$%d", idx)
			args = append(args, true)
		}
		q += " ORDER BY m.match_date, t.user_id"
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		rowIdx := 2
		for rows.Next() {
			var tipID int
			var username, compName, roundName, matchName string
			var tipH, tipA int
			var resH, resA, points *int
			_ = rows.Scan(&tipID, &username, &compName, &roundName, &matchName, &tipH, &tipA, &resH, &resA, &points)
			resHVal, resAVal, ptsVal := interface{}(""), interface{}(""), interface{}("")
			if resH != nil {
				resHVal = *resH
			}
			if resA != nil {
				resAVal = *resA
			}
			if points != nil {
				ptsVal = *points
			}
			writeRow(sheet, rowIdx, []interface{}{tipID, username, compName, roundName, matchName, tipH, tipA, resHVal, resAVal, ptsVal})
			rowIdx++
		}

	case "users":
		sheet := "Uživatelé"
		_ = f.SetSheetName("Sheet1", sheet)
		writeHeader(sheet, []string{"Nick", "Jméno", "Příjmení", "Email", "Role", "Registrace"})
		rows, _ := db.Pool.Query(ctx,
			`SELECT username, COALESCE(first_name,''), COALESCE(last_name,''), COALESCE(email,''),
			        is_owner, is_admin, created_at
			 FROM users ORDER BY username`)
		defer rows.Close()
		rowIdx := 2
		for rows.Next() {
			var username, first, last, email string
			var isOwner, isAdmin bool
			var createdAt *time.Time
			_ = rows.Scan(&username, &first, &last, &email, &isOwner, &isAdmin, &createdAt)
			role := "user"
			if isOwner {
				role = "owner"
			} else if isAdmin {
				role = "admin"
			}
			createdStr := ""
			if createdAt != nil {
				createdStr = createdAt.Format("02.01.2006")
			}
			writeRow(sheet, rowIdx, []interface{}{username, first, last, email, role, createdStr})
			rowIdx++
		}

	case "leaderboard":
		sheet := "Žebříček"
		_ = f.SetSheetName("Sheet1", sheet)
		writeHeader(sheet, []string{"Pořadí", "Nick", "Body", "Přesné (3b)", "Správný vítěz (1b)", "Špatné (0b)", "Počet tipů"})

		type lbRow struct {
			username string
			points   int
			exact    int
			winner   int
			miss     int
			count    int
		}
		byUser := map[string]*lbRow{}

		q := `SELECT u.username, t.points
		      FROM tips t
		      JOIN users u ON u.id = t.user_id
		      JOIN matches m ON m.id = t.match_id
		      JOIN rounds ro ON ro.id = m.round_id
		      WHERE t.points IS NOT NULL`
		args := []interface{}{}
		if compID > 0 {
			q += " AND ro.competition_id=$1"
			args = append(args, compID)
		}
		rows, _ := db.Pool.Query(ctx, q, args...)
		defer rows.Close()
		for rows.Next() {
			var username string
			var points int
			_ = rows.Scan(&username, &points)
			row, ok := byUser[username]
			if !ok {
				row = &lbRow{username: username}
				byUser[username] = row
			}
			row.points += points
			row.count++
			switch points {
			case 3:
				row.exact++
			case 1:
				row.winner++
			default:
				row.miss++
			}
		}

		sorted := make([]lbRow, 0, len(byUser))
		for _, v := range byUser {
			sorted = append(sorted, *v)
		}
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].points > sorted[i].points {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		for rank, row := range sorted {
			writeRow(sheet, rank+2, []interface{}{rank + 1, row.username, row.points, row.exact, row.winner, row.miss, row.count})
		}

	default:
		http.Error(w, "Neznámý typ exportu. Použij ?type=teams|matches|tips|users|leaderboard", 400)
		return
	}

	today := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("tipovacka_%s_%s.xlsx", exportType, today)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if err := f.Write(w); err != nil {
		http.Error(w, "Chyba při generování XLSX: "+err.Error(), 500)
	}
}

// sanitizeFilename replaces spaces/unsafe chars with underscores and truncates.
func sanitizeFilename(s string, maxLen int) string {
	b := []byte(s)
	for i, c := range b {
		if c == ' ' || c == '/' || c == '\\' || c == ':' {
			b[i] = '_'
		}
	}
	if len(b) > maxLen {
		b = b[:maxLen]
	}
	return string(b)
}
