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
	"strconv"
	"time"

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

	// Баннер опросчика: функция возвращает (текст последней ошибки, время).
	PollerError func() (string, time.Time)
	// Базовый URL информатикса для канонических ссылок.
	InformaticsBase string
}

type Config struct {
	Store           *store.Store
	Logger          *log.Logger
	Secret          []byte
	AdminCredsPath  string
	PageRefresh     time.Duration
	PollerError     func() (string, time.Time)
	InformaticsBase string
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		store:           cfg.Store,
		logger:          cfg.Logger,
		secret:          cfg.Secret,
		adminCreds:      cfg.AdminCredsPath,
		pageRefresh:     cfg.PageRefresh,
		cache:           newStateCache(),
		PollerError:     cfg.PollerError,
		InformaticsBase: cfg.InformaticsBase,
	}
	if s.pageRefresh <= 0 {
		s.pageRefresh = time.Second
	}
	if s.PollerError == nil {
		s.PollerError = func() (string, time.Time) { return "", time.Time{} }
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

	s.registerAdmin(mux)
	return mux
}

// ---------- Вспомогательное ----------

func (s *Server) render(w http.ResponseWriter, name string, data any) {
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
	})
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
