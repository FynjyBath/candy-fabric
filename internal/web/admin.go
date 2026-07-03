package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"candyfactory/internal/game"
	"candyfactory/internal/links"
	"candyfactory/internal/store"
)

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.adminGate(s.handleAdminHome))
	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
	mux.HandleFunc("GET /admin/games", s.adminGate(s.handleAdminGames))
	mux.HandleFunc("GET /admin/games/new", s.adminGate(s.handleAdminGameNew))
	mux.HandleFunc("POST /admin/games/new", s.adminGate(s.handleAdminGameCreate))
	mux.HandleFunc("GET /admin/g/{gameId}", s.adminGate(s.handleAdminGame))
	mux.HandleFunc("GET /admin/g/{gameId}/edit", s.adminGate(s.handleAdminGameEdit))
	mux.HandleFunc("POST /admin/g/{gameId}/edit", s.adminGate(s.handleAdminGameEditSave))
	mux.HandleFunc("POST /admin/g/{gameId}/start", s.adminGate(s.handleAdminStart))
	mux.HandleFunc("POST /admin/g/{gameId}/extend", s.adminGate(s.handleAdminExtend))
	mux.HandleFunc("POST /admin/g/{gameId}/archive", s.adminGate(s.handleAdminArchive))
	mux.HandleFunc("POST /admin/g/{gameId}/event", s.adminGate(s.handleAdminEventAdd))
	mux.HandleFunc("POST /admin/g/{gameId}/event/{eventId}/update", s.adminGate(s.handleAdminEventUpdate))
	mux.HandleFunc("POST /admin/g/{gameId}/event/{eventId}/delete", s.adminGate(s.handleAdminEventDelete))
	mux.HandleFunc("POST /admin/g/{gameId}/event/{eventId}/toggle", s.adminGate(s.handleAdminEventToggle))
	mux.HandleFunc("POST /admin/g/{gameId}/anomaly/{anomalyId}/accept", s.adminGate(s.handleAdminAnomalyAccept))
	mux.HandleFunc("POST /admin/g/{gameId}/anomaly/{anomalyId}/reject", s.adminGate(s.handleAdminAnomalyReject))
	mux.HandleFunc("POST /admin/g/{gameId}/team/{teamId}/password", s.adminGate(s.handleAdminTeamPassword))
	mux.HandleFunc("GET /admin/api/g/{gameId}/state", s.adminGate(s.handleAdminState))
}

// adminGate — все /admin/* без сессии: GET — форма входа, мутации — 403.
func (s *Server) adminGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			if r.Method == http.MethodGet {
				s.render(w, "admin_login.html", map[string]any{"Error": "", "Next": r.URL.Path})
				return
			}
			http.Error(w, "требуется вход администратора", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !s.checkCSRF(r) {
			http.Error(w, "неверный CSRF-токен", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleAdminHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/games", http.StatusSeeOther)
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	creds, err := loadAdminCredentials(s.adminCreds)
	if err != nil {
		s.logger.Printf("ERROR чтение admin_credentials.json: %v", err)
		s.render(w, "admin_login.html", map[string]any{"Error": "Ошибка конфигурации сервера (admin_credentials.json)", "Next": r.FormValue("next")})
		return
	}
	if r.FormValue("login") == creds.Login && r.FormValue("password") == creds.Password {
		s.setSession(w, &Session{Role: "admin"})
		s.logger.Printf("INFO вход в админку (%s) — успех", creds.Login)
		next := r.FormValue("next")
		if !strings.HasPrefix(next, "/admin") {
			next = "/admin/games"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.logger.Printf("WARN вход в админку — неверные логин/пароль (логин %q)", r.FormValue("login"))
	s.render(w, "admin_login.html", map[string]any{"Error": "Неверные логин или пароль", "Next": r.FormValue("next")})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---------- Список игр ----------

func (s *Server) handleAdminGames(w http.ResponseWriter, r *http.Request) {
	games, err := s.store.ListGames(true)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	type row struct {
		*store.Game
		StatusNow string
		TeamCount int
		Remaining string
	}
	teamCounts, _ := s.store.TeamCounts()
	var rows []row
	for _, g := range games {
		rem := ""
		if g.Status(now) == "running" {
			d := g.EndAt().Sub(now).Round(time.Second)
			rem = fmtDuration(d)
		}
		rows = append(rows, row{g, g.Status(now), teamCounts[g.ID], rem})
	}
	s.render(w, "admin_games.html", map[string]any{"Games": rows, "CSRF": s.csrfToken(r)})
}

// parseLocalDateTime разбирает значение input[type=datetime-local] (с
// секундами или без) в часовом поясе сервера и возвращает время в UTC.
func parseLocalDateTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("не удалось разобрать время %q", v)
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
}

// ---------- Создание/редактирование игры ----------

// gameForm — сырые значения формы (для повторного показа при ошибках).
type gameForm struct {
	Title       string
	N           string
	StartAmount string
	StartSpeed  string
	DurationMin string
	StartAt     string // datetime-local, пусто = не задано
	Levels      []levelForm
	TaskRows    []string // n текстариев, по n ссылок в строках
	Teams       []teamForm
	Errors      []string
}

type levelForm struct {
	Level                                             int
	TaskCost, TestCost, Load, AmountBonus, SpeedBonus string
}

type teamForm struct {
	Name, UserID, Login, Password string
}

// defaultLevelParams — предзаполнение уровней; для n=3 совпадает с ТЗ 3.2.
func defaultLevelParams(n int) []store.Level {
	testCost := []int64{3000, 7000, 10000, 12000, 14000, 16000}
	amountBonus := []int64{12000, 25000, 50000, 75000, 110000, 160000}
	speedBonus := []int64{4, 7, 11, 15, 20, 26}
	var out []store.Level
	for r := 1; r <= n; r++ {
		i := r - 1
		if i >= len(testCost) {
			i = len(testCost) - 1
		}
		out = append(out, store.Level{
			Level: r, TaskCost: 12000, TestCost: testCost[i], Load: 2,
			AmountBonus: amountBonus[i], SpeedBonus: speedBonus[i],
		})
	}
	return out
}

func defaultGameForm() *gameForm {
	f := &gameForm{
		Title: "", N: "3", StartAmount: "20000", StartSpeed: "15", DurationMin: "85",
		TaskRows: make([]string, 3),
		Teams:    []teamForm{{}, {}},
	}
	for _, l := range defaultLevelParams(3) {
		f.Levels = append(f.Levels, levelFormFromStore(l))
	}
	return f
}

func levelFormFromStore(l store.Level) levelForm {
	return levelForm{
		Level:       l.Level,
		TaskCost:    strconv.FormatInt(l.TaskCost, 10),
		TestCost:    strconv.FormatInt(l.TestCost, 10),
		Load:        strconv.FormatInt(l.Load, 10),
		AmountBonus: strconv.FormatInt(l.AmountBonus, 10),
		SpeedBonus:  strconv.FormatInt(l.SpeedBonus, 10),
	}
}

func (s *Server) handleAdminGameNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, "admin_game_edit.html", map[string]any{
		"Form": defaultGameForm(), "CSRF": s.csrfToken(r), "IsNew": true,
	})
}

// parseGameForm читает форму и валидирует её; возвращает данные для store.
func (s *Server) parseGameForm(r *http.Request) (*gameForm, *store.Game, []store.Level, [][]store.TaskInput, []store.TeamInput) {
	f := &gameForm{
		Title:       strings.TrimSpace(r.FormValue("title")),
		N:           strings.TrimSpace(r.FormValue("n")),
		StartAmount: strings.TrimSpace(r.FormValue("start_amount")),
		StartSpeed:  strings.TrimSpace(r.FormValue("start_speed")),
		DurationMin: strings.TrimSpace(r.FormValue("duration_min")),
		StartAt:     strings.TrimSpace(r.FormValue("start_at")),
	}
	fail := func(format string, a ...any) {
		f.Errors = append(f.Errors, fmt.Sprintf(format, a...))
	}
	if f.Title == "" {
		fail("Укажите название игры")
	}
	n, err := strconv.Atoi(f.N)
	if err != nil || n < 2 || n > 6 {
		fail("Число уровней n должно быть целым от 2 до 6")
		n = 3
	}
	startAmount, err := strconv.ParseInt(f.StartAmount, 10, 64)
	if err != nil || startAmount < 0 {
		fail("Стартовые запасы должны быть неотрицательным целым")
	}
	startSpeed, err := strconv.ParseInt(f.StartSpeed, 10, 64)
	if err != nil || startSpeed < 0 {
		fail("Стартовая скорость должна быть неотрицательным целым")
	}
	durMin, err := strconv.ParseFloat(f.DurationMin, 64)
	if err != nil || durMin <= 0 {
		fail("Длительность (в минутах) должна быть положительным числом")
	}
	// Уровни.
	var levels []store.Level
	for lvl := 1; lvl <= n; lvl++ {
		get := func(name string) string { return strings.TrimSpace(r.FormValue(fmt.Sprintf("%s_%d", name, lvl))) }
		lf := levelForm{Level: lvl, TaskCost: get("task_cost"), TestCost: get("test_cost"),
			Load: get("load"), AmountBonus: get("amount_bonus"), SpeedBonus: get("speed_bonus")}
		f.Levels = append(f.Levels, lf)
		parse := func(v, label string) int64 {
			x, err := strconv.ParseInt(v, 10, 64)
			if err != nil || x < 0 {
				fail("Уровень %d: %s — нужно неотрицательное целое", lvl, label)
			}
			return x
		}
		levels = append(levels, store.Level{
			Level:       lvl,
			TaskCost:    parse(lf.TaskCost, "цена задачи"),
			TestCost:    parse(lf.TestCost, "цена теста"),
			Load:        parse(lf.Load, "нагрузка"),
			AmountBonus: parse(lf.AmountBonus, "бонус к запасам"),
			SpeedBonus:  parse(lf.SpeedBonus, "бонус к скорости"),
		})
	}
	// Задачи: n текстариев по n ссылок (нормализация по разделу 6).
	seen := map[int]string{}
	var tasksByLevel [][]store.TaskInput
	for lvl := 1; lvl <= n; lvl++ {
		raw := r.FormValue(fmt.Sprintf("tasks_%d", lvl))
		f.TaskRows = append(f.TaskRows, raw)
		var row []store.TaskInput
		var lines []string
		for _, line := range strings.Split(raw, "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		if len(lines) != n {
			fail("Уровень %d: нужно ровно %d ссылок (по одной в строке), получено %d", lvl, n, len(lines))
		}
		for i, line := range lines {
			chapterID, err := links.Normalize(line)
			if err != nil {
				fail("Уровень %d, строка %d: %v", lvl, i+1, err)
				continue
			}
			if prev, dup := seen[chapterID]; dup {
				fail("Уровень %d, строка %d: дубликат задачи chapterid=%d (уже есть в %s)", lvl, i+1, chapterID, prev)
				continue
			}
			seen[chapterID] = fmt.Sprintf("уровне %d", lvl)
			row = append(row, store.TaskInput{
				ChapterID: chapterID,
				URL:       links.CanonicalURL(s.InformaticsBase, chapterID),
			})
		}
		tasksByLevel = append(tasksByLevel, row)
	}
	// Команды (минимум 2, логины уникальны).
	names := r.Form["team_name"]
	userIDs := r.Form["team_user_id"]
	logins := r.Form["team_login"]
	passwords := r.Form["team_password"]
	var teams []store.TeamInput
	loginSeen := map[string]bool{}
	for i := range names {
		name := strings.TrimSpace(names[i])
		if name == "" && strings.TrimSpace(get(logins, i)) == "" {
			continue // пустая строка формы
		}
		tf := teamForm{Name: name, UserID: get(userIDs, i), Login: get(logins, i), Password: get(passwords, i)}
		f.Teams = append(f.Teams, tf)
		if name == "" {
			fail("Команда %d: укажите имя", i+1)
		}
		uid, err := strconv.Atoi(strings.TrimSpace(tf.UserID))
		if err != nil || uid <= 0 {
			fail("Команда %q: informatics user_id должен быть положительным целым", name)
		}
		login := strings.TrimSpace(tf.Login)
		if login == "" {
			fail("Команда %q: укажите логин", name)
		}
		if loginSeen[login] {
			fail("Команда %q: логин %q уже используется в этой игре", name, login)
		}
		loginSeen[login] = true
		if tf.Password == "" {
			fail("Команда %q: укажите пароль", name)
		}
		teams = append(teams, store.TeamInput{Name: name, InformaticsUserID: uid, Login: login, Password: tf.Password})
	}
	if len(teams) < 2 {
		fail("Нужно минимум 2 команды")
	}
	if len(f.Teams) < 2 {
		for len(f.Teams) < 2 {
			f.Teams = append(f.Teams, teamForm{})
		}
	}
	g := &store.Game{
		Title:       f.Title,
		N:           n,
		StartAmount: startAmount,
		StartSpeed:  startSpeed,
		DurationSec: int64(durMin * 60),
	}
	if f.StartAt != "" {
		if t, err := parseLocalDateTime(f.StartAt); err != nil {
			fail("Не удалось разобрать плановое время старта")
		} else {
			g.StartAt = &t
		}
	}
	return f, g, levels, tasksByLevel, teams
}

func get(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}

func (s *Server) handleAdminGameCreate(w http.ResponseWriter, r *http.Request) {
	f, g, levels, tasks, teams := s.parseGameForm(r)
	if len(f.Errors) > 0 {
		s.render(w, "admin_game_edit.html", map[string]any{"Form": f, "CSRF": s.csrfToken(r), "IsNew": true})
		return
	}
	id, err := s.store.CreateGame(g, levels, tasks, teams)
	if err != nil {
		f.Errors = append(f.Errors, "Ошибка сохранения: "+err.Error())
		s.render(w, "admin_game_edit.html", map[string]any{"Form": f, "CSRF": s.csrfToken(r), "IsNew": true})
		return
	}
	s.logger.Printf("INFO admin: создана игра %d %q", id, g.Title)
	http.Redirect(w, r, fmt.Sprintf("/admin/g/%d", id), http.StatusSeeOther)
}

// editableGame возвращает игру, если она ещё в статусе draft.
func (s *Server) editableGame(w http.ResponseWriter, r *http.Request) *store.Game {
	g := s.gameFromPath(w, r)
	if g == nil {
		return nil
	}
	if g.Status(time.Now()) != "draft" {
		http.Error(w, "конфигурация игры после старта не редактируется", http.StatusConflict)
		return nil
	}
	return g
}

func (s *Server) handleAdminGameEdit(w http.ResponseWriter, r *http.Request) {
	g := s.editableGame(w, r)
	if g == nil {
		return
	}
	levels, _ := s.store.GetLevels(g.ID)
	tasks, _ := s.store.GetTasks(g.ID)
	teams, _ := s.store.GetTeams(g.ID)
	f := &gameForm{
		Title:       g.Title,
		N:           strconv.Itoa(g.N),
		StartAmount: strconv.FormatInt(g.StartAmount, 10),
		StartSpeed:  strconv.FormatInt(g.StartSpeed, 10),
		DurationMin: strconv.FormatFloat(float64(g.DurationSec)/60, 'f', -1, 64),
	}
	if g.StartAt != nil {
		f.StartAt = g.StartAt.Local().Format("2006-01-02T15:04")
	}
	for _, l := range levels {
		f.Levels = append(f.Levels, levelFormFromStore(l))
	}
	rows := make([]string, g.N)
	for _, t := range tasks {
		if t.Level >= 1 && t.Level <= g.N {
			if rows[t.Level-1] != "" {
				rows[t.Level-1] += "\n"
			}
			rows[t.Level-1] += t.URL
		}
	}
	f.TaskRows = rows
	for _, t := range teams {
		f.Teams = append(f.Teams, teamForm{Name: t.Name, UserID: strconv.Itoa(t.InformaticsUserID), Login: t.Login, Password: t.Password})
	}
	s.render(w, "admin_game_edit.html", map[string]any{"Form": f, "CSRF": s.csrfToken(r), "IsNew": false, "Game": g})
}

func (s *Server) handleAdminGameEditSave(w http.ResponseWriter, r *http.Request) {
	g := s.editableGame(w, r)
	if g == nil {
		return
	}
	f, ng, levels, tasks, teams := s.parseGameForm(r)
	if len(f.Errors) > 0 {
		s.render(w, "admin_game_edit.html", map[string]any{"Form": f, "CSRF": s.csrfToken(r), "IsNew": false, "Game": g})
		return
	}
	ng.ID = g.ID
	if err := s.store.UpdateGameConfig(ng, levels, tasks, teams); err != nil {
		f.Errors = append(f.Errors, "Ошибка сохранения: "+err.Error())
		s.render(w, "admin_game_edit.html", map[string]any{"Form": f, "CSRF": s.csrfToken(r), "IsNew": false, "Game": g})
		return
	}
	s.logger.Printf("INFO admin: изменена конфигурация игры %d %q", g.ID, ng.Title)
	http.Redirect(w, r, fmt.Sprintf("/admin/g/%d", g.ID), http.StatusSeeOther)
}

// ---------- Жизненный цикл ----------

func (s *Server) handleAdminStart(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	if g.Status(time.Now()) != "draft" {
		http.Error(w, "игра уже стартовала", http.StatusConflict)
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.store.SetGameStartAt(g.ID, &now); err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	if err := s.store.EnsureTaskOrder(g.ID); err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("INFO admin: старт игры %d %q в %s", g.ID, g.Title, now.Format(time.RFC3339))
	http.Redirect(w, r, fmt.Sprintf("/admin/g/%d", g.ID), http.StatusSeeOther)
}

func (s *Server) handleAdminExtend(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	if err := s.store.ExtendGameDuration(g.ID, 900); err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("INFO admin: игра %d продлена на 15 минут", g.ID)
	http.Redirect(w, r, fmt.Sprintf("/admin/g/%d", g.ID), http.StatusSeeOther)
}

func (s *Server) handleAdminArchive(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	if err := s.store.ArchiveGame(g.ID); err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("INFO admin: игра %d отправлена в архив", g.ID)
	http.Redirect(w, r, "/admin/games", http.StatusSeeOther)
}

// ---------- Страница игры ----------

func (s *Server) handleAdminGame(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	snap, err := s.snapshot(g)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	teams, _ := s.store.GetTeams(g.ID)
	anomalies, _ := s.store.GetAnomalies(g.ID, true)
	events := snap.Events

	teamByID := map[int64]store.Team{}
	for _, t := range teams {
		teamByID[t.ID] = t
	}
	taskByID := map[int64]store.Task{}
	for _, t := range snap.Tasks {
		taskByID[t.ID] = t
	}
	type eventRow struct {
		*store.Event
		TeamName  string
		ChapterID int
		TypeRu    string
	}
	typeRu := map[string]string{"buy_task": "покупка задачи", "buy_test": "покупка теста", "solve": "решение"}
	var eventRows []eventRow
	for _, e := range events {
		er := eventRow{Event: e, TypeRu: typeRu[e.Type]}
		if t, ok := teamByID[e.TeamID]; ok {
			er.TeamName = t.Name
		}
		if e.TaskID != nil {
			if t, ok := taskByID[*e.TaskID]; ok {
				er.ChapterID = t.ChapterID
			}
		}
		eventRows = append(eventRows, er)
	}
	type anomalyRow struct {
		*store.Anomaly
		TeamName  string
		ChapterID int
		ReasonRu  string
	}
	reasonRu := map[string]string{
		"not_bought":     "задача не куплена",
		"already_passed": "задача уже решена",
		"out_of_time":    "посылка вне времени игры",
	}
	var anomalyRows []anomalyRow
	for _, a := range anomalies {
		ar := anomalyRow{Anomaly: a, ReasonRu: reasonRu[a.Reason]}
		if t, ok := teamByID[a.TeamID]; ok {
			ar.TeamName = t.Name
		}
		if t, ok := taskByID[a.TaskID]; ok {
			ar.ChapterID = t.ChapterID
		}
		anomalyRows = append(anomalyRows, ar)
	}
	// Панель предупреждений валидации.
	type warnRow struct {
		EventID int64
		Text    string
	}
	var warns []warnRow
	for _, wrn := range snap.Result.Warnings {
		warns = append(warns, warnRow{wrn.EventID, wrn.Text})
	}
	pollerErr, pollerErrAt := s.PollerError()
	s.render(w, "admin_game.html", map[string]any{
		"Game":        g,
		"StatusNow":   snap.Status,
		"Teams":       teams,
		"Tasks":       snap.Tasks,
		"Events":      eventRows,
		"Anomalies":   anomalyRows,
		"Warnings":    warns,
		"CSRF":        s.csrfToken(r),
		"StateURL":    fmt.Sprintf("/admin/api/g/%d/state", g.ID),
		"RefreshSec":  s.pageRefresh.Seconds(),
		"PollerErr":   pollerErr,
		"PollerErrAt": pollerErrAt,
	})
}

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	snap, err := s.snapshot(g)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	st := s.buildState(snap, "admin", 0)
	pollerErr, pollerErrAt := s.PollerError()
	writeJSON(w, map[string]any{
		"state":         st,
		"poller_error":  pollerErr,
		"poller_err_at": pollerErrAt,
	})
}

// ---------- События ----------

// eventJSONResp — ответ мутаций событий (fetch из админки).
func jsonOK(w http.ResponseWriter) { writeJSON(w, map[string]any{"ok": true}) }

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"error": msg})
}

func jsonWarning(w http.ResponseWriter, msg string) {
	writeJSON(w, map[string]any{"warning": msg})
}

// parseEventForm читает общие поля события из формы.
func (s *Server) parseEventForm(r *http.Request, g *store.Game) (*store.Event, string) {
	teamID, err := strconv.ParseInt(r.FormValue("team_id"), 10, 64)
	if err != nil {
		return nil, "не указана команда"
	}
	team, err := s.store.GetTeam(teamID)
	if err != nil || team.GameID != g.ID {
		return nil, "команда не найдена в этой игре"
	}
	taskID, err := strconv.ParseInt(r.FormValue("task_id"), 10, 64)
	if err != nil {
		return nil, "не указана задача"
	}
	found := false
	tasks, _ := s.store.GetTasks(g.ID)
	for _, t := range tasks {
		if t.ID == taskID {
			found = true
		}
	}
	if !found {
		return nil, "задача не найдена в этой игре"
	}
	typ := r.FormValue("type")
	if typ != "buy_task" && typ != "buy_test" && typ != "solve" {
		return nil, "неизвестный тип события"
	}
	at := time.Now().UTC().Truncate(time.Second)
	if v := strings.TrimSpace(r.FormValue("at")); v != "" {
		t, err := parseLocalDateTime(v)
		if err != nil {
			return nil, "не удалось разобрать время события"
		}
		at = t
	}
	return &store.Event{
		GameID: g.ID, TeamID: teamID, TaskID: &taskID, Type: typ, At: at,
		Source: "manual", Enabled: true, Comment: strings.TrimSpace(r.FormValue("comment")),
	}, ""
}

// purchaseWarning проверяет достаточность средств/производительности на момент
// события (3.3). excludeEventID > 0 исключает редактируемое событие из
// расчёта (его старая версия не должна влиять на проверку новой).
// Возвращает текст предупреждения или "".
func (s *Server) purchaseWarning(g *store.Game, e *store.Event, excludeEventID int64) string {
	if e.Type != "buy_task" && e.Type != "buy_test" {
		return ""
	}
	levels, err := s.store.GetLevels(g.ID)
	if err != nil {
		return ""
	}
	tasks, err := s.store.GetTasks(g.ID)
	if err != nil {
		return ""
	}
	teams, err := s.store.GetTeams(g.ID)
	if err != nil {
		return ""
	}
	allEvents, err := s.store.GetEvents(g.ID)
	if err != nil {
		return ""
	}
	events := allEvents[:0:0]
	for _, ev := range allEvents {
		if ev.ID != excludeEventID {
			events = append(events, ev)
		}
	}
	res := game.Compute(g, levels, tasks, teams, events, e.At)
	ts := res.Teams[e.TeamID]
	if ts == nil {
		return ""
	}
	var lvl *store.Level
	for i := range tasks {
		if tasks[i].ID == *e.TaskID {
			for j := range levels {
				if levels[j].Level == tasks[i].Level {
					lvl = &levels[j]
				}
			}
		}
	}
	if lvl == nil {
		return ""
	}
	switch e.Type {
	case "buy_task":
		if ts.Amount < lvl.TaskCost {
			return "Недостаточно средств"
		}
		if ts.Speed < lvl.Load {
			return "Недостаточно производительности"
		}
	case "buy_test":
		if ts.Amount < lvl.TestCost {
			return "Недостаточно средств"
		}
	}
	return ""
}

func (s *Server) handleAdminEventAdd(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	e, errMsg := s.parseEventForm(r, g)
	if errMsg != "" {
		jsonErr(w, http.StatusBadRequest, errMsg)
		return
	}
	// Предупреждение о нехватке средств: сохранение только после явного
	// подтверждения (confirmed=1); администратор — окончательный авторитет.
	if r.FormValue("confirmed") != "1" {
		if warn := s.purchaseWarning(g, e, 0); warn != "" {
			jsonWarning(w, warn)
			return
		}
	}
	id, err := s.store.AddEvent(e)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сохранения: "+err.Error())
		return
	}
	s.logger.Printf("INFO admin: событие %d добавлено (игра %d, команда %d, задача %d, %s, %s)",
		id, g.ID, e.TeamID, *e.TaskID, e.Type, e.At.Format(time.RFC3339))
	jsonOK(w)
}

func (s *Server) eventFromPath(w http.ResponseWriter, r *http.Request, g *store.Game) *store.Event {
	id, err := strconv.ParseInt(r.PathValue("eventId"), 10, 64)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "событие не найдено")
		return nil
	}
	e, err := s.store.GetEvent(id)
	if err != nil || e.GameID != g.ID {
		jsonErr(w, http.StatusNotFound, "событие не найдено")
		return nil
	}
	return e
}

func (s *Server) handleAdminEventUpdate(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	e := s.eventFromPath(w, r, g)
	if e == nil {
		return
	}
	if e.Source == "auto" {
		// У auto-событий можно поправить только время (5.2).
		t, err := parseLocalDateTime(r.FormValue("at"))
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "не удалось разобрать время события")
			return
		}
		if err := s.store.UpdateEventAt(e.ID, g.ID, t); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.logger.Printf("INFO admin: время auto-события %d изменено на %s", e.ID, t.Format(time.RFC3339))
		jsonOK(w)
		return
	}
	ne, errMsg := s.parseEventForm(r, g)
	if errMsg != "" {
		jsonErr(w, http.StatusBadRequest, errMsg)
		return
	}
	ne.ID = e.ID
	ne.GameID = g.ID
	// Как и при добавлении: покупка «в минус» сохраняется только после
	// явного подтверждения.
	if r.FormValue("confirmed") != "1" {
		if warn := s.purchaseWarning(g, ne, e.ID); warn != "" {
			jsonWarning(w, warn)
			return
		}
	}
	if err := s.store.UpdateManualEvent(ne); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Printf("INFO admin: событие %d изменено (команда %d, задача %d, %s, %s)",
		e.ID, ne.TeamID, *ne.TaskID, ne.Type, ne.At.Format(time.RFC3339))
	jsonOK(w)
}

func (s *Server) handleAdminEventDelete(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	e := s.eventFromPath(w, r, g)
	if e == nil {
		return
	}
	if e.Source != "manual" {
		jsonErr(w, http.StatusConflict, "auto-события нельзя удалять — их можно только отключить")
		return
	}
	if err := s.store.DeleteManualEvent(e.ID, g.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Printf("INFO admin: событие %d удалено (игра %d)", e.ID, g.ID)
	jsonOK(w)
}

func (s *Server) handleAdminEventToggle(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	e := s.eventFromPath(w, r, g)
	if e == nil {
		return
	}
	if err := s.store.SetEventEnabled(e.ID, g.ID, !e.Enabled); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Printf("INFO admin: событие %d переключено (enabled=%v)", e.ID, !e.Enabled)
	jsonOK(w)
}

// ---------- Аномалии ----------

func (s *Server) anomalyFromPath(w http.ResponseWriter, r *http.Request, g *store.Game) *store.Anomaly {
	id, err := strconv.ParseInt(r.PathValue("anomalyId"), 10, 64)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "аномалия не найдена")
		return nil
	}
	a, err := s.store.GetAnomaly(id)
	if err != nil || a.GameID != g.ID {
		jsonErr(w, http.StatusNotFound, "аномалия не найдена")
		return nil
	}
	if a.Resolved {
		jsonErr(w, http.StatusConflict, "аномалия уже решена")
		return nil
	}
	return a
}

func (s *Server) handleAdminAnomalyAccept(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	a := s.anomalyFromPath(w, r, g)
	if a == nil {
		return
	}
	if err := s.store.AcceptAnomaly(a); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Printf("INFO admin: аномалия %d принята (игра %d, run %d)", a.ID, g.ID, a.RunID)
	jsonOK(w)
}

func (s *Server) handleAdminAnomalyReject(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	a := s.anomalyFromPath(w, r, g)
	if a == nil {
		return
	}
	if err := s.store.ResolveAnomaly(a.ID, g.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Printf("INFO admin: аномалия %d отклонена (игра %d, run %d)", a.ID, g.ID, a.RunID)
	jsonOK(w)
}

// ---------- Команды ----------

func (s *Server) handleAdminTeamPassword(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	teamID, err := strconv.ParseInt(r.PathValue("teamId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	team, err := s.store.GetTeam(teamID)
	if err != nil || team.GameID != g.ID {
		http.NotFound(w, r)
		return
	}
	pw := r.FormValue("password")
	if pw == "" {
		http.Error(w, "пустой пароль", http.StatusBadRequest)
		return
	}
	if err := s.store.SetTeamPassword(teamID, pw); err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("INFO admin: сменён пароль команды %d (игра %d)", teamID, g.ID)
	http.Redirect(w, r, fmt.Sprintf("/admin/g/%d", g.ID), http.StatusSeeOther)
}
