// Package store — SQLite-хранилище (раздел 4 ТЗ).
package store

import (
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS games (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	title TEXT NOT NULL,
	status_archived INTEGER NOT NULL DEFAULT 0,
	n INTEGER NOT NULL,
	start_amount INTEGER NOT NULL,
	start_speed INTEGER NOT NULL,
	duration_sec INTEGER NOT NULL,
	start_at TEXT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS game_levels (
	game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
	level INTEGER NOT NULL,
	task_cost INTEGER NOT NULL,
	test_cost INTEGER NOT NULL,
	load INTEGER NOT NULL,
	amount_bonus INTEGER NOT NULL,
	speed_bonus INTEGER NOT NULL,
	PRIMARY KEY (game_id, level)
);

CREATE TABLE IF NOT EXISTS game_tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
	level INTEGER NOT NULL,
	ord INTEGER NOT NULL,
	chapter_id INTEGER NOT NULL,
	url TEXT NOT NULL,
	UNIQUE (game_id, chapter_id)
);

CREATE TABLE IF NOT EXISTS teams (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	informatics_user_id INTEGER NOT NULL,
	login TEXT NOT NULL,
	password TEXT NOT NULL,
	UNIQUE (game_id, login)
);

CREATE TABLE IF NOT EXISTS team_task_order (
	team_id INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	cell INTEGER NOT NULL,
	task_id INTEGER NOT NULL REFERENCES game_tasks(id),
	PRIMARY KEY (team_id, cell)
);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
	team_id INTEGER NOT NULL REFERENCES teams(id),
	task_id INTEGER NULL REFERENCES game_tasks(id),
	type TEXT NOT NULL,
	at TEXT NOT NULL,
	source TEXT NOT NULL,
	run_id INTEGER NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	comment TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (game_id, run_id)
);

CREATE TABLE IF NOT EXISTS anomalies (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
	team_id INTEGER NOT NULL REFERENCES teams(id),
	task_id INTEGER NOT NULL REFERENCES game_tasks(id),
	run_id INTEGER NOT NULL,
	run_at TEXT NOT NULL,
	reason TEXT NOT NULL,
	resolved INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	UNIQUE (game_id, run_id)
);
`

// Store — обёртка над SQLite с версией данных для инвалидации кеша расчёта.
type Store struct {
	DB *sql.DB

	mu        sync.Mutex
	versions  map[int64]int64 // game_id -> версия журнала/конфигурации
	orderDone map[int64]bool  // game_id -> перестановки уже материализованы
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite: одна пишущая транзакция за раз, ограничиваем пул.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("миграция схемы: %w", err)
	}
	return &Store{DB: db, versions: map[int64]int64{}, orderDone: map[int64]bool{}}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// Bump увеличивает версию данных игры (для ленивого пересчёта состояния).
func (s *Store) Bump(gameID int64) {
	s.mu.Lock()
	s.versions[gameID]++
	s.mu.Unlock()
}

// Version возвращает текущую версию данных игры.
func (s *Store) Version(gameID int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versions[gameID]
}

// ---------- Модели ----------

type Game struct {
	ID          int64
	Title       string
	Archived    bool
	N           int
	StartAmount int64
	StartSpeed  int64
	DurationSec int64
	StartAt     *time.Time
	CreatedAt   time.Time
}

// Status — вычислимая функция времени (раздел 3.4).
func (g *Game) Status(now time.Time) string {
	if g.Archived {
		return "archived"
	}
	if g.StartAt == nil || now.Before(*g.StartAt) {
		return "draft"
	}
	if now.Before(g.StartAt.Add(time.Duration(g.DurationSec) * time.Second)) {
		return "running"
	}
	return "finished"
}

func (g *Game) EndAt() *time.Time {
	if g.StartAt == nil {
		return nil
	}
	e := g.StartAt.Add(time.Duration(g.DurationSec) * time.Second)
	return &e
}

type Level struct {
	Level       int
	TaskCost    int64
	TestCost    int64
	Load        int64
	AmountBonus int64
	SpeedBonus  int64
}

type Task struct {
	ID        int64
	GameID    int64
	Level     int
	Ord       int
	ChapterID int
	URL       string
}

type Team struct {
	ID                int64
	GameID            int64
	Name              string
	InformaticsUserID int
	Login             string
	Password          string
}

type Event struct {
	ID        int64
	GameID    int64
	TeamID    int64
	TaskID    *int64
	Type      string // buy_task | buy_test | solve
	At        time.Time
	Source    string // manual | auto
	RunID     *int64
	Enabled   bool
	Comment   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Anomaly struct {
	ID        int64
	GameID    int64
	TeamID    int64
	TaskID    int64
	RunID     int64
	RunAt     time.Time
	Reason    string // not_bought | already_passed | out_of_time
	Resolved  bool
	CreatedAt time.Time
}

const timeFmt = time.RFC3339

func fmtTime(t time.Time) string { return t.UTC().Format(timeFmt) }
func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeFmt, s)
	return t.UTC()
}

// ---------- Игры ----------

func (s *Store) CreateGame(g *Game, levels []Level, tasksByLevel [][]TaskInput, teams []TeamInput) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO games(title, status_archived, n, start_amount, start_speed, duration_sec, start_at, created_at)
		VALUES(?,0,?,?,?,?,?,?)`,
		g.Title, g.N, g.StartAmount, g.StartSpeed, g.DurationSec, fmtTimePtr(g.StartAt), fmtTime(time.Now()))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if err := insertGameConfig(tx, id, levels, tasksByLevel, teams); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.Bump(id)
	return id, nil
}

type TaskInput struct {
	ChapterID int
	URL       string
}

type TeamInput struct {
	Name              string
	InformaticsUserID int
	Login             string
	Password          string
}

func insertGameConfig(tx *sql.Tx, gameID int64, levels []Level, tasksByLevel [][]TaskInput, teams []TeamInput) error {
	for _, l := range levels {
		if _, err := tx.Exec(`INSERT INTO game_levels(game_id, level, task_cost, test_cost, load, amount_bonus, speed_bonus)
			VALUES(?,?,?,?,?,?,?)`, gameID, l.Level, l.TaskCost, l.TestCost, l.Load, l.AmountBonus, l.SpeedBonus); err != nil {
			return err
		}
	}
	for li, row := range tasksByLevel {
		for oi, t := range row {
			if _, err := tx.Exec(`INSERT INTO game_tasks(game_id, level, ord, chapter_id, url) VALUES(?,?,?,?,?)`,
				gameID, li+1, oi+1, t.ChapterID, t.URL); err != nil {
				return err
			}
		}
	}
	for _, t := range teams {
		if _, err := tx.Exec(`INSERT INTO teams(game_id, name, informatics_user_id, login, password) VALUES(?,?,?,?,?)`,
			gameID, t.Name, t.InformaticsUserID, t.Login, t.Password); err != nil {
			return err
		}
	}
	return nil
}

// UpdateGameConfig полностью перезаписывает конфигурацию игры в статусе draft.
func (s *Store) UpdateGameConfig(g *Game, levels []Level, tasksByLevel [][]TaskInput, teams []TeamInput) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE games SET title=?, n=?, start_amount=?, start_speed=?, duration_sec=?, start_at=? WHERE id=?`,
		g.Title, g.N, g.StartAmount, g.StartSpeed, g.DurationSec, fmtTimePtr(g.StartAt), g.ID); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM team_task_order WHERE team_id IN (SELECT id FROM teams WHERE game_id=?)`,
		`DELETE FROM game_levels WHERE game_id=?`,
		`DELETE FROM game_tasks WHERE game_id=?`,
		`DELETE FROM teams WHERE game_id=?`,
	} {
		if _, err := tx.Exec(q, g.ID); err != nil {
			return err
		}
	}
	if err := insertGameConfig(tx, g.ID, levels, tasksByLevel, teams); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.orderDone[g.ID] = false // конфигурация переписана — перестановки удалены
	s.mu.Unlock()
	s.Bump(g.ID)
	return nil
}

func scanGame(row interface{ Scan(...any) error }) (*Game, error) {
	var g Game
	var archived int
	var startAt sql.NullString
	var createdAt string
	if err := row.Scan(&g.ID, &g.Title, &archived, &g.N, &g.StartAmount, &g.StartSpeed, &g.DurationSec, &startAt, &createdAt); err != nil {
		return nil, err
	}
	g.Archived = archived != 0
	if startAt.Valid {
		t := parseTime(startAt.String)
		g.StartAt = &t
	}
	g.CreatedAt = parseTime(createdAt)
	return &g, nil
}

const gameCols = `id, title, status_archived, n, start_amount, start_speed, duration_sec, start_at, created_at`

func (s *Store) GetGame(id int64) (*Game, error) {
	return scanGame(s.DB.QueryRow(`SELECT `+gameCols+` FROM games WHERE id=?`, id))
}

func (s *Store) ListGames(includeArchived bool) ([]*Game, error) {
	q := `SELECT ` + gameCols + ` FROM games`
	if !includeArchived {
		q += ` WHERE status_archived=0`
	}
	q += ` ORDER BY id DESC`
	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Game
	for rows.Next() {
		g, err := scanGame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) SetGameStartAt(id int64, at *time.Time) error {
	_, err := s.DB.Exec(`UPDATE games SET start_at=? WHERE id=?`, fmtTimePtr(at), id)
	s.Bump(id)
	return err
}

func (s *Store) SetGameDuration(id int64, sec int64) error {
	_, err := s.DB.Exec(`UPDATE games SET duration_sec=? WHERE id=?`, sec, id)
	s.Bump(id)
	return err
}

// ExtendGameDuration атомарно продлевает игру (два одновременных «+15 минут»
// не потеряют друг друга).
func (s *Store) ExtendGameDuration(id int64, deltaSec int64) error {
	_, err := s.DB.Exec(`UPDATE games SET duration_sec = duration_sec + ? WHERE id=?`, deltaSec, id)
	s.Bump(id)
	return err
}

// TeamCounts — число команд по играм одним запросом (для списка игр).
func (s *Store) TeamCounts() (map[int64]int, error) {
	rows, err := s.DB.Query(`SELECT game_id, COUNT(*) FROM teams GROUP BY game_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var gameID int64
		var cnt int
		if err := rows.Scan(&gameID, &cnt); err != nil {
			return nil, err
		}
		out[gameID] = cnt
	}
	return out, rows.Err()
}

func (s *Store) ArchiveGame(id int64) error {
	_, err := s.DB.Exec(`UPDATE games SET status_archived=1 WHERE id=?`, id)
	s.Bump(id)
	return err
}

// ---------- Конфигурация игры ----------

func (s *Store) GetLevels(gameID int64) ([]Level, error) {
	rows, err := s.DB.Query(`SELECT level, task_cost, test_cost, load, amount_bonus, speed_bonus
		FROM game_levels WHERE game_id=? ORDER BY level`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Level
	for rows.Next() {
		var l Level
		if err := rows.Scan(&l.Level, &l.TaskCost, &l.TestCost, &l.Load, &l.AmountBonus, &l.SpeedBonus); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetTasks(gameID int64) ([]Task, error) {
	rows, err := s.DB.Query(`SELECT id, game_id, level, ord, chapter_id, url
		FROM game_tasks WHERE game_id=? ORDER BY level, ord`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.GameID, &t.Level, &t.Ord, &t.ChapterID, &t.URL); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTeams(gameID int64) ([]Team, error) {
	rows, err := s.DB.Query(`SELECT id, game_id, name, informatics_user_id, login, password
		FROM teams WHERE game_id=? ORDER BY id`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.GameID, &t.Name, &t.InformaticsUserID, &t.Login, &t.Password); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTeam(teamID int64) (*Team, error) {
	var t Team
	err := s.DB.QueryRow(`SELECT id, game_id, name, informatics_user_id, login, password FROM teams WHERE id=?`, teamID).
		Scan(&t.ID, &t.GameID, &t.Name, &t.InformaticsUserID, &t.Login, &t.Password)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) SetTeamPassword(teamID int64, password string) error {
	_, err := s.DB.Exec(`UPDATE teams SET password=? WHERE id=?`, password, teamID)
	if err == nil {
		if t, e := s.GetTeam(teamID); e == nil {
			s.Bump(t.GameID)
		}
	}
	return err
}

// ---------- Перестановки ----------

// EnsureTaskOrder материализует случайные перестановки задач для всех команд
// игры, если их ещё нет. Идемпотентно и безопасно для конкурентных вызовов
// (проверка и вставка — в одной транзакции; повторная проверка под мьютексом
// плюс кеш «уже готово», чтобы не гонять COUNT каждую секунду).
func (s *Store) EnsureTaskOrder(gameID int64) error {
	s.mu.Lock()
	if s.orderDone[gameID] {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	teams, err := s.GetTeams(gameID)
	if err != nil {
		return err
	}
	tasks, err := s.GetTasks(gameID)
	if err != nil {
		return err
	}
	byLevel := map[int][]Task{}
	maxLevel := 0
	for _, t := range tasks {
		byLevel[t.Level] = append(byLevel[t.Level], t)
		if t.Level > maxLevel {
			maxLevel = t.Level
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Проверка внутри транзакции: пул из одного соединения сериализует
	// транзакции, поэтому второй конкурентный вызов увидит уже вставленные
	// строки и выйдет, не ломая перестановку.
	var cnt int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM team_task_order
		WHERE team_id IN (SELECT id FROM teams WHERE game_id=?)`, gameID).Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		s.mu.Lock()
		s.orderDone[gameID] = true
		s.mu.Unlock()
		return nil
	}
	for _, team := range teams {
		for lvl := 1; lvl <= maxLevel; lvl++ {
			row := byLevel[lvl]
			n := len(row)
			perm := rand.Perm(n)
			for i, p := range perm {
				cell := (lvl-1)*n + i + 1
				if _, err := tx.Exec(`INSERT INTO team_task_order(team_id, cell, task_id) VALUES(?,?,?)`,
					team.ID, cell, row[p].ID); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.orderDone[gameID] = true
	s.mu.Unlock()
	s.Bump(gameID)
	return nil
}

// GetTaskOrders возвращает перестановки всех команд игры одним запросом:
// team_id -> (cell -> task_id).
func (s *Store) GetTaskOrders(gameID int64) (map[int64]map[int]int64, error) {
	rows, err := s.DB.Query(`SELECT o.team_id, o.cell, o.task_id
		FROM team_task_order o JOIN teams t ON t.id = o.team_id
		WHERE t.game_id = ?`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[int]int64{}
	for rows.Next() {
		var teamID, taskID int64
		var cell int
		if err := rows.Scan(&teamID, &cell, &taskID); err != nil {
			return nil, err
		}
		if out[teamID] == nil {
			out[teamID] = map[int]int64{}
		}
		out[teamID][cell] = taskID
	}
	return out, rows.Err()
}

// GetTaskOrder возвращает для команды отображение cell -> task_id.
func (s *Store) GetTaskOrder(teamID int64) (map[int]int64, error) {
	rows, err := s.DB.Query(`SELECT cell, task_id FROM team_task_order WHERE team_id=?`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var cell int
		var taskID int64
		if err := rows.Scan(&cell, &taskID); err != nil {
			return nil, err
		}
		out[cell] = taskID
	}
	return out, rows.Err()
}

// ---------- События ----------

const eventCols = `id, game_id, team_id, task_id, type, at, source, run_id, enabled, comment, created_at, updated_at`

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	var taskID, runID sql.NullInt64
	var at, createdAt, updatedAt string
	var enabled int
	if err := row.Scan(&e.ID, &e.GameID, &e.TeamID, &taskID, &e.Type, &at, &e.Source, &runID, &enabled, &e.Comment, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if taskID.Valid {
		e.TaskID = &taskID.Int64
	}
	if runID.Valid {
		e.RunID = &runID.Int64
	}
	e.At = parseTime(at)
	e.Enabled = enabled != 0
	e.CreatedAt = parseTime(createdAt)
	e.UpdatedAt = parseTime(updatedAt)
	return &e, nil
}

func (s *Store) GetEvents(gameID int64) ([]*Event, error) {
	rows, err := s.DB.Query(`SELECT `+eventCols+` FROM events WHERE game_id=? ORDER BY at, id`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetEvent(id int64) (*Event, error) {
	return scanEvent(s.DB.QueryRow(`SELECT `+eventCols+` FROM events WHERE id=?`, id))
}

func (s *Store) AddEvent(e *Event) (int64, error) {
	now := fmtTime(time.Now())
	var runID any
	if e.RunID != nil {
		runID = *e.RunID
	}
	var taskID any
	if e.TaskID != nil {
		taskID = *e.TaskID
	}
	res, err := s.DB.Exec(`INSERT INTO events(game_id, team_id, task_id, type, at, source, run_id, enabled, comment, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.GameID, e.TeamID, taskID, e.Type, fmtTime(e.At), e.Source, runID, boolToInt(e.Enabled), e.Comment, now, now)
	if err != nil {
		return 0, err
	}
	s.Bump(e.GameID)
	return res.LastInsertId()
}

// UpdateManualEvent меняет время/команду/задачу/тип/комментарий ручного события.
func (s *Store) UpdateManualEvent(e *Event) error {
	_, err := s.DB.Exec(`UPDATE events SET team_id=?, task_id=?, type=?, at=?, comment=?, updated_at=?
		WHERE id=? AND source='manual'`,
		e.TeamID, e.TaskID, e.Type, fmtTime(e.At), e.Comment, fmtTime(time.Now()), e.ID)
	s.Bump(e.GameID)
	return err
}

// UpdateEventAt меняет только время события (разрешено и для auto).
func (s *Store) UpdateEventAt(id, gameID int64, at time.Time) error {
	_, err := s.DB.Exec(`UPDATE events SET at=?, updated_at=? WHERE id=?`, fmtTime(at), fmtTime(time.Now()), id)
	s.Bump(gameID)
	return err
}

func (s *Store) SetEventEnabled(id, gameID int64, enabled bool) error {
	_, err := s.DB.Exec(`UPDATE events SET enabled=?, updated_at=? WHERE id=?`, boolToInt(enabled), fmtTime(time.Now()), id)
	s.Bump(gameID)
	return err
}

// DeleteManualEvent физически удаляет ручное событие.
func (s *Store) DeleteManualEvent(id, gameID int64) error {
	_, err := s.DB.Exec(`DELETE FROM events WHERE id=? AND source='manual'`, id)
	s.Bump(gameID)
	return err
}

// HasRunRecord — есть ли по run_id событие или аномалия в игре (дедупликация).
func (s *Store) HasRunRecord(gameID, runID int64) (bool, error) {
	var cnt int
	err := s.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM events WHERE game_id=? AND run_id=?) +
		(SELECT COUNT(*) FROM anomalies WHERE game_id=? AND run_id=?)`,
		gameID, runID, gameID, runID).Scan(&cnt)
	return cnt > 0, err
}

// HasEnabledSolve — есть ли включённое событие solve по паре (команда, задача).
func (s *Store) HasEnabledSolve(gameID, teamID, taskID int64) (bool, error) {
	var cnt int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM events
		WHERE game_id=? AND team_id=? AND task_id=? AND type='solve' AND enabled=1`,
		gameID, teamID, taskID).Scan(&cnt)
	return cnt > 0, err
}

// ---------- Аномалии ----------

func (s *Store) AddAnomaly(a *Anomaly) error {
	_, err := s.DB.Exec(`INSERT INTO anomalies(game_id, team_id, task_id, run_id, run_at, reason, resolved, created_at)
		VALUES(?,?,?,?,?,?,0,?)`,
		a.GameID, a.TeamID, a.TaskID, a.RunID, fmtTime(a.RunAt), a.Reason, fmtTime(time.Now()))
	s.Bump(a.GameID)
	return err
}

func (s *Store) GetAnomalies(gameID int64, onlyUnresolved bool) ([]*Anomaly, error) {
	q := `SELECT id, game_id, team_id, task_id, run_id, run_at, reason, resolved, created_at FROM anomalies WHERE game_id=?`
	if onlyUnresolved {
		q += ` AND resolved=0`
	}
	q += ` ORDER BY id`
	rows, err := s.DB.Query(q, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Anomaly
	for rows.Next() {
		var a Anomaly
		var runAt, createdAt string
		var resolved int
		if err := rows.Scan(&a.ID, &a.GameID, &a.TeamID, &a.TaskID, &a.RunID, &runAt, &a.Reason, &resolved, &createdAt); err != nil {
			return nil, err
		}
		a.RunAt = parseTime(runAt)
		a.Resolved = resolved != 0
		a.CreatedAt = parseTime(createdAt)
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *Store) GetAnomaly(id int64) (*Anomaly, error) {
	var a Anomaly
	var runAt, createdAt string
	var resolved int
	err := s.DB.QueryRow(`SELECT id, game_id, team_id, task_id, run_id, run_at, reason, resolved, created_at
		FROM anomalies WHERE id=?`, id).
		Scan(&a.ID, &a.GameID, &a.TeamID, &a.TaskID, &a.RunID, &runAt, &a.Reason, &resolved, &createdAt)
	if err != nil {
		return nil, err
	}
	a.RunAt = parseTime(runAt)
	a.Resolved = resolved != 0
	a.CreatedAt = parseTime(createdAt)
	return &a, nil
}

func (s *Store) ResolveAnomaly(id, gameID int64) error {
	_, err := s.DB.Exec(`UPDATE anomalies SET resolved=1 WHERE id=?`, id)
	s.Bump(gameID)
	return err
}

// AcceptAnomaly в одной транзакции создаёт ручное событие solve со временем
// посылки и помечает аномалию решённой.
func (s *Store) AcceptAnomaly(a *Anomaly) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := fmtTime(time.Now())
	// run_id не переносим в событие: уникальность (game_id, run_id) уже занята аномалией.
	if _, err := tx.Exec(`INSERT INTO events(game_id, team_id, task_id, type, at, source, run_id, enabled, comment, created_at, updated_at)
		VALUES(?,?,?,'solve',?,'manual',NULL,1,?,?,?)`,
		a.GameID, a.TeamID, a.TaskID, fmtTime(a.RunAt), fmt.Sprintf("принята аномалия #%d (run %d)", a.ID, a.RunID), now, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE anomalies SET resolved=1 WHERE id=?`, a.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Bump(a.GameID)
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
