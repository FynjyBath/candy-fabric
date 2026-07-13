// Package web — HTTP-сервер: публичные страницы, страницы команд, JSON-API
// и админка (раздел 8 ТЗ).
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"candyfactory/internal/answer"
	"candyfactory/internal/game"
	"candyfactory/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server — состояние HTTP-приложения.
type Server struct {
	store       *store.Store
	logger      *log.Logger
	secret      []byte
	adminCreds  string // путь к admin_credentials.json
	pageRefresh time.Duration
	cache       *stateCache
	tmpl        *template.Template
	buyMu       sync.Mutex // сериализация покупок и ответов команд
	// Анти-брутфорс ответов (мат-режим): по паре (команда, ячейка) растущая
	// задержка после неверных попыток. Доступ — под buyMu.
	answerStrikes map[string]answerStrike

	// Оформление всего сайта: candy | neuro | hamster. Хранится в файле,
	// меняется из админки, применяется ко всем страницам сразу.
	themePath string
	themeMu   sync.RWMutex
	theme     string

	// Баннер опросчика: функция возвращает (текст последней ошибки, время).
	PollerError func() (string, time.Time)
	// Статус опросчика: время последнего цикла, решений в нём и всего.
	PollerStatus func() (time.Time, int, int)
	// CheckGame — разовая проверка игры против информатикса (в горутине).
	CheckGame func(gameID int64)
	// Базовый URL информатикса для канонических ссылок.
	InformaticsBase string
}

type Config struct {
	Store           *store.Store
	Logger          *log.Logger
	Secret          []byte
	AdminCredsPath  string
	ThemePath       string // файл с текущим оформлением сайта
	PageRefresh     time.Duration
	PollerError     func() (string, time.Time)
	PollerStatus    func() (time.Time, int, int)
	CheckGame       func(gameID int64)
	InformaticsBase string
}

// answerStrike — счётчик неверных ответов и момент, до которого приём
// следующего ответа по этой ячейке заблокирован.
type answerStrike struct {
	count int
	until time.Time
}

// pruneAnswerStrikes ограничивает рост карты штрафов: при большом размере
// удаляет давно истёкшие записи (решённые/заброшенные ячейки). Вызывается
// под buyMu. Порог высок, поэтому в горячем пути почти всегда no-op.
func (s *Server) pruneAnswerStrikes(now time.Time) {
	if len(s.answerStrikes) < 10000 {
		return
	}
	for k, st := range s.answerStrikes {
		if now.Sub(st.until) > time.Hour {
			delete(s.answerStrikes, k)
		}
	}
}

// answerCooldown — задержка после count-го подряд неверного ответа. Первая
// ошибка бесплатна (опечатка), дальше задержка растёт экспоненциально
// (2, 4, 8, …), но не больше 30 секунд. Честная команда после верного
// расчёта укладывается в пару попыток, а перебор из тысяч вариантов
// растягивается на часы.
func answerCooldown(count int) time.Duration {
	if count <= 1 {
		return 0
	}
	d := time.Duration(1<<uint(count-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// Доступные оформления сайта.
var validThemes = map[string]string{
	"candy":   "Конфетная фабрика",
	"neuro":   "Токены и нейросети",
	"hamster": "Тапаем хомяка",
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		store:           cfg.Store,
		logger:          cfg.Logger,
		secret:          cfg.Secret,
		adminCreds:      cfg.AdminCredsPath,
		themePath:       cfg.ThemePath,
		theme:           "candy",
		answerStrikes:   map[string]answerStrike{},
		pageRefresh:     cfg.PageRefresh,
		cache:           newStateCache(),
		PollerError:     cfg.PollerError,
		PollerStatus:    cfg.PollerStatus,
		CheckGame:       cfg.CheckGame,
		InformaticsBase: cfg.InformaticsBase,
	}
	if s.themePath != "" {
		if b, err := os.ReadFile(s.themePath); err == nil {
			t := strings.TrimSpace(string(b))
			if _, ok := validThemes[t]; ok {
				s.theme = t
			}
		}
	}
	if s.pageRefresh <= 0 {
		s.pageRefresh = time.Second
	}
	if s.PollerError == nil {
		s.PollerError = func() (string, time.Time) { return "", time.Time{} }
	}
	if s.PollerStatus == nil {
		s.PollerStatus = func() (time.Time, int, int) { return time.Time{}, 0, 0 }
	}
	if s.CheckGame == nil {
		s.CheckGame = func(int64) {}
	}
	funcs := template.FuncMap{
		"addOne": func(i int) int { return i + 1 },
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"localTime": func(t time.Time) string { return t.Local().Format("02.01.2006 15:04:05") },
		"localTimePtr": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.Local().Format("02.01.2006 15:04:05")
		},
		"inputTime": func(t time.Time) string { return t.Local().Format("2006-01-02T15:04:05") },
		"statusRu": func(st string) string {
			switch st {
			case "draft":
				return "черновик"
			case "running":
				return "идёт"
			case "finished":
				return "завершена"
			case "archived":
				return "в архиве"
			}
			return st
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = tmpl
	return s, nil
}

// Handler собирает карту маршрутов (8.1).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /g/{gameId}", s.handlePublicBoard)
	mux.HandleFunc("GET /g/{gameId}/team", s.handleTeamLoginForm)
	mux.HandleFunc("POST /g/{gameId}/team", s.handleTeamLogin)
	mux.HandleFunc("GET /g/{gameId}/team/{teamId}", s.handleTeamPage)
	mux.HandleFunc("GET /api/g/{gameId}/state", s.handlePublicState)
	mux.HandleFunc("GET /api/g/{gameId}/team/{teamId}/state", s.handleTeamState)
	mux.HandleFunc("POST /api/g/{gameId}/team/{teamId}/buy", s.handleTeamBuy)
	mux.HandleFunc("POST /api/g/{gameId}/team/{teamId}/answer", s.handleTeamAnswer)

	s.registerAdmin(mux)
	return mux
}

// ---------- Вспомогательное ----------

// Theme — текущее оформление сайта.
func (s *Server) Theme() string {
	s.themeMu.RLock()
	defer s.themeMu.RUnlock()
	return s.theme
}

// SetTheme меняет оформление сайта и сохраняет его на диск.
func (s *Server) SetTheme(t string) error {
	if _, ok := validThemes[t]; !ok {
		return fmt.Errorf("неизвестное оформление %q", t)
	}
	s.themeMu.Lock()
	s.theme = t
	s.themeMu.Unlock()
	if s.themePath != "" {
		return os.WriteFile(s.themePath, []byte(t+"\n"), 0o644)
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	// Тема сайта нужна каждой странице — подкладываем централизованно.
	if m, ok := data.(map[string]any); ok {
		if _, exists := m["Theme"]; !exists {
			t := s.Theme()
			m["Theme"] = t
			m["SiteTitle"] = validThemes[t]
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Printf("ERROR шаблон %s: %v", name, err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// gameFromPath возвращает игру по {gameId}; неизвестный id — 404.
func (s *Server) gameFromPath(w http.ResponseWriter, r *http.Request) *store.Game {
	id, err := strconv.ParseInt(r.PathValue("gameId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	g, err := s.store.GetGame(id)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	return g
}

// ---------- Публичные страницы ----------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	games, err := s.store.ListGames(false)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	type row struct {
		*store.Game
		StatusNow string
	}
	var rows []row
	for _, g := range games {
		rows = append(rows, row{g, g.Status(now)})
	}
	s.render(w, "index.html", map[string]any{"Games": rows})
}

func (s *Server) handlePublicBoard(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	s.render(w, "board.html", map[string]any{
		"Game":       g,
		"Mode":       "public",
		"StateURL":   fmt.Sprintf("/api/g/%d/state", g.ID),
		"RefreshSec": s.pageRefresh.Seconds(),
		"CSRF":       "",
	})
}

func (s *Server) handlePublicState(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	snap, err := s.snapshot(g)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.buildState(snap, "public", 0))
}

// ---------- Страница команды ----------

func (s *Server) handleTeamLoginForm(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	// Уже залогинен в эту игру — сразу на свою страницу.
	if sess := s.session(r); sess != nil && sess.Role == "team" && sess.GameID == g.ID {
		http.Redirect(w, r, fmt.Sprintf("/g/%d/team/%d", g.ID, sess.TeamID), http.StatusSeeOther)
		return
	}
	s.render(w, "team_login.html", map[string]any{"Game": g, "Error": ""})
}

func (s *Server) handleTeamLogin(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	login := r.FormValue("login")
	password := r.FormValue("password")
	teams, err := s.store.GetTeams(g.ID)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	for _, t := range teams {
		if t.Login == login && t.Password == password {
			s.setSession(w, &Session{Role: "team", GameID: g.ID, TeamID: t.ID})
			s.logger.Printf("INFO вход команды %q (игра %d) — успех", login, g.ID)
			http.Redirect(w, r, fmt.Sprintf("/g/%d/team/%d", g.ID, t.ID), http.StatusSeeOther)
			return
		}
	}
	s.logger.Printf("WARN вход команды %q (игра %d) — неверные логин/пароль", login, g.ID)
	s.render(w, "team_login.html", map[string]any{"Game": g, "Error": "Неверные логин или пароль"})
}

// teamAccess проверяет доступ к странице/API команды: своя командная сессия
// или админ. Чужая командная сессия — 403.
func (s *Server) teamAccess(w http.ResponseWriter, r *http.Request, g *store.Game) *store.Team {
	teamID, err := strconv.ParseInt(r.PathValue("teamId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	team, err := s.store.GetTeam(teamID)
	if err != nil || team.GameID != g.ID {
		http.NotFound(w, r)
		return nil
	}
	sess := s.session(r)
	if sess == nil {
		http.Redirect(w, r, fmt.Sprintf("/g/%d/team", g.ID), http.StatusSeeOther)
		return nil
	}
	if sess.Role == "admin" {
		return team
	}
	if sess.Role == "team" && sess.GameID == g.ID && sess.TeamID == team.ID {
		return team
	}
	http.Error(w, "доступ запрещён", http.StatusForbidden)
	return nil
}

func (s *Server) handleTeamPage(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	team := s.teamAccess(w, r, g)
	if team == nil {
		return
	}
	s.render(w, "board.html", map[string]any{
		"Game":       g,
		"Team":       team,
		"Mode":       "team",
		"StateURL":   fmt.Sprintf("/api/g/%d/team/%d/state", g.ID, team.ID),
		"RefreshSec": s.pageRefresh.Seconds(),
		"CSRF":       s.csrfToken(r),
	})
}

// handleTeamBuy — самостоятельная покупка задачи командой. Задача адресуется
// номером ячейки (а не task_id), чтобы до покупки команда не могла опознать
// задачу по идентификатору. В отличие от админа, команде покупка «в минус»
// блокируется, а не подтверждается.
func (s *Server) handleTeamBuy(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	// Для API-запроса редирект на форму логина не годится: без сессии — 401
	// (иначе fetch молча проследует за редиректом и примет HTML за успех).
	if s.session(r) == nil {
		jsonErr(w, http.StatusUnauthorized, "требуется вход команды")
		return
	}
	team := s.teamAccess(w, r, g)
	if team == nil {
		return
	}
	if !s.checkCSRF(r) {
		jsonErr(w, http.StatusForbidden, "неверный CSRF-токен")
		return
	}
	// Покупки сериализуются (в т. ч. с админскими событиями): два
	// одновременных клика не купят одну задачу дважды и не уведут баланс
	// в минус.
	s.buyMu.Lock()
	defer s.buyMu.Unlock()

	// Игру перечитываем под мьютексом: параллельное продление/сокращение
	// могло поменять длительность после разбора пути.
	g, err := s.store.GetGame(g.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сервера")
		return
	}
	if g.Status(time.Now()) != "running" {
		jsonErr(w, http.StatusConflict, "покупки доступны только во время игры")
		return
	}
	cell, err := strconv.Atoi(r.FormValue("cell"))
	if err != nil || cell < 1 || cell > g.N*g.N {
		jsonErr(w, http.StatusBadRequest, "некорректная ячейка")
		return
	}
	order, err := s.store.GetTaskOrder(team.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сервера")
		return
	}
	taskID, ok := order[cell]
	if !ok {
		jsonErr(w, http.StatusConflict, "задача ячейки ещё не назначена")
		return
	}
	snap, err := s.snapshot(g)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сервера")
		return
	}
	ts := snap.Result.Teams[team.ID]
	if ts == nil {
		jsonErr(w, http.StatusInternalServerError, "состояние команды не найдено")
		return
	}
	st := ts.Tasks[taskID]
	if st == nil || st.State != game.StateHidden {
		jsonErr(w, http.StatusConflict, "задача уже куплена")
		return
	}
	var lvl *store.Level
	for i := range snap.Tasks {
		if snap.Tasks[i].ID == taskID {
			for j := range snap.Levels {
				if snap.Levels[j].Level == snap.Tasks[i].Level {
					lvl = &snap.Levels[j]
				}
			}
		}
	}
	if lvl == nil {
		jsonErr(w, http.StatusInternalServerError, "уровень задачи не найден")
		return
	}
	if ts.Amount < lvl.TaskCost {
		jsonErr(w, http.StatusConflict, "Недостаточно средств")
		return
	}
	if ts.Speed < lvl.Load {
		jsonErr(w, http.StatusConflict, "Недостаточно производительности")
		return
	}
	if _, err := s.store.AddEvent(&store.Event{
		GameID: g.ID, TeamID: team.ID, TaskID: &taskID,
		Type: "buy_task", At: time.Now().UTC().Truncate(time.Second),
		Source: "manual", Enabled: true, Comment: "куплено командой",
	}); err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сохранения")
		return
	}
	s.logger.Printf("INFO команда %q купила задачу (игра %d, ячейка %d, task %d, −%d)",
		team.Name, g.ID, cell, taskID, lvl.TaskCost)
	writeJSON(w, map[string]any{"ok": true})
}

// handleTeamAnswer — отправка ответа командой в математическом режиме.
// Задача адресуется номером ячейки; ответ сравнивается с эталонным
// (нормализация пробелов и регистра). Верный ответ засчитывает задачу
// (событие solve), неверный не меняет состояние.
func (s *Server) handleTeamAnswer(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	if s.session(r) == nil {
		jsonErr(w, http.StatusUnauthorized, "требуется вход команды")
		return
	}
	team := s.teamAccess(w, r, g)
	if team == nil {
		return
	}
	if !s.checkCSRF(r) {
		jsonErr(w, http.StatusForbidden, "неверный CSRF-токен")
		return
	}
	// Сериализуем с покупками и админскими событиями: между проверкой
	// состояния и записью solve никто не должен вклиниться.
	s.buyMu.Lock()
	defer s.buyMu.Unlock()

	g, err := s.store.GetGame(g.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сервера")
		return
	}
	if g.Mode != store.ModeManual {
		jsonErr(w, http.StatusConflict, "приём ответов доступен только в математическом режиме")
		return
	}
	if g.Status(time.Now()) != "running" {
		jsonErr(w, http.StatusConflict, "ответы принимаются только во время игры")
		return
	}
	cell, err := strconv.Atoi(r.FormValue("cell"))
	if err != nil || cell < 1 || cell > g.N*g.N {
		jsonErr(w, http.StatusBadRequest, "некорректная ячейка")
		return
	}
	order, err := s.store.GetTaskOrder(team.ID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сервера")
		return
	}
	taskID, ok := order[cell]
	if !ok {
		jsonErr(w, http.StatusConflict, "задача ячейки ещё не назначена")
		return
	}
	snap, err := s.snapshot(g)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сервера")
		return
	}
	ts := snap.Result.Teams[team.ID]
	if ts == nil {
		jsonErr(w, http.StatusInternalServerError, "состояние команды не найдено")
		return
	}
	st := ts.Tasks[taskID]
	if st == nil {
		jsonErr(w, http.StatusInternalServerError, "состояние задачи не найдено")
		return
	}
	if st.State == game.StatePassed {
		jsonErr(w, http.StatusConflict, "задача уже решена")
		return
	}
	if st.State != game.StateBought {
		jsonErr(w, http.StatusConflict, "сначала купите задачу")
		return
	}
	var task *store.Task
	for i := range snap.Tasks {
		if snap.Tasks[i].ID == taskID {
			task = &snap.Tasks[i]
			break
		}
	}
	if task == nil {
		jsonErr(w, http.StatusInternalServerError, "задача не найдена")
		return
	}
	if strings.TrimSpace(task.Answer) == "" {
		jsonErr(w, http.StatusConflict, "для этой задачи автопроверка не настроена — обратитесь к организатору")
		return
	}
	// Анти-брутфорс: после серии неверных ответов приём временно блокируется.
	strikeKey := fmt.Sprintf("%d:%d:%d", g.ID, team.ID, cell)
	now := time.Now()
	s.pruneAnswerStrikes(now)
	if st := s.answerStrikes[strikeKey]; now.Before(st.until) {
		wait := int(st.until.Sub(now).Seconds()) + 1
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, map[string]any{"ok": false, "wait": wait,
			"error": fmt.Sprintf("слишком много попыток, подождите %d с", wait)})
		return
	}
	submitted := r.FormValue("answer")
	if !answer.Equal(submitted, task.Answer) {
		st := s.answerStrikes[strikeKey]
		st.count++
		st.until = now.Add(answerCooldown(st.count))
		s.answerStrikes[strikeKey] = st
		s.logger.Printf("INFO команда %q — неверный ответ (игра %d, ячейка %d, task %d, попытка %d)",
			team.Name, g.ID, cell, taskID, st.count)
		writeJSON(w, map[string]any{"ok": true, "correct": false})
		return
	}
	delete(s.answerStrikes, strikeKey)
	if _, err := s.store.AddEvent(&store.Event{
		GameID: g.ID, TeamID: team.ID, TaskID: &taskID,
		Type: "solve", At: time.Now().UTC().Truncate(time.Second),
		Source: "manual", Enabled: true, Comment: "верный ответ команды",
	}); err != nil {
		jsonErr(w, http.StatusInternalServerError, "ошибка сохранения")
		return
	}
	s.logger.Printf("INFO команда %q — верный ответ, задача засчитана (игра %d, ячейка %d, task %d)",
		team.Name, g.ID, cell, taskID)
	writeJSON(w, map[string]any{"ok": true, "correct": true})
}

func (s *Server) handleTeamState(w http.ResponseWriter, r *http.Request) {
	g := s.gameFromPath(w, r)
	if g == nil {
		return
	}
	team := s.teamAccess(w, r, g)
	if team == nil {
		return
	}
	snap, err := s.snapshot(g)
	if err != nil {
		http.Error(w, "ошибка сервера", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.buildState(snap, "team", team.ID))
}
