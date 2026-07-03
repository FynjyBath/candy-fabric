package web

import (
	"sync"
	"time"

	"candyfactory/internal/game"
	"candyfactory/internal/store"
)

// stateCache — кеш расчёта состояния в памяти процесса (5.3): пересчёт при
// изменении журнала/конфигурации (версия) и не реже раза в секунду.
type stateCache struct {
	mu      sync.Mutex
	entries map[int64]*cacheEntry
}

type cacheEntry struct {
	computedAt time.Time
	version    int64
	snapshot   *gameSnapshot
}

// gameSnapshot — всё, что нужно страницам: конфигурация + результат расчёта.
type gameSnapshot struct {
	Game   *store.Game
	Levels []store.Level
	Tasks  []store.Task
	Teams  []store.Team
	Events []*store.Event
	Orders map[int64]map[int]int64 // team_id -> cell -> task_id
	Result *game.Result
	Status string
	Now    time.Time
}

func newStateCache() *stateCache {
	return &stateCache{entries: map[int64]*cacheEntry{}}
}

// snapshot возвращает актуальный расчёт состояния игры, лениво пересчитывая,
// если с прошлого расчёта прошло >= 1 c или изменились данные.
func (s *Server) snapshot(g *store.Game) (*gameSnapshot, error) {
	now := time.Now().UTC()
	ver := s.store.Version(g.ID)
	s.cache.mu.Lock()
	e := s.cache.entries[g.ID]
	if e != nil && e.version == ver && now.Sub(e.computedAt) < time.Second {
		snap := e.snapshot
		s.cache.mu.Unlock()
		return snap, nil
	}
	s.cache.mu.Unlock()

	// Перестановки материализуются при переходе в running (в т. ч. по
	// плановому времени старта) — идемпотентно.
	status := g.Status(now)
	if status == "running" || status == "finished" {
		if err := s.store.EnsureTaskOrder(g.ID); err != nil {
			return nil, err
		}
		ver = s.store.Version(g.ID)
	}

	levels, err := s.store.GetLevels(g.ID)
	if err != nil {
		return nil, err
	}
	tasks, err := s.store.GetTasks(g.ID)
	if err != nil {
		return nil, err
	}
	teams, err := s.store.GetTeams(g.ID)
	if err != nil {
		return nil, err
	}
	events, err := s.store.GetEvents(g.ID)
	if err != nil {
		return nil, err
	}
	orders, err := s.store.GetTaskOrders(g.ID)
	if err != nil {
		return nil, err
	}
	snap := &gameSnapshot{
		Game: g, Levels: levels, Tasks: tasks, Teams: teams, Events: events,
		Orders: orders,
		Result: game.Compute(g, levels, tasks, teams, events, now),
		Status: status,
		Now:    now,
	}
	s.cache.mu.Lock()
	s.cache.entries[g.ID] = &cacheEntry{computedAt: now, version: ver, snapshot: snap}
	s.cache.mu.Unlock()
	return snap, nil
}

// ---------- JSON состояния (8.4) ----------

type stateJSON struct {
	ServerTime     string      `json:"server_time"`
	Status         string      `json:"status"`
	StartAt        *string     `json:"start_at"`
	EndAt          *string     `json:"end_at"`
	N              int         `json:"n"`
	Levels         []levelJSON `json:"levels"`
	Teams          []teamJSON  `json:"teams"`
	PageRefreshSec float64     `json:"page_refresh_sec"`
}

type levelJSON struct {
	TaskCost    int64 `json:"task_cost"`
	TestCost    int64 `json:"test_cost"`
	Load        int64 `json:"load"`
	AmountBonus int64 `json:"amount_bonus"`
	SpeedBonus  int64 `json:"speed_bonus"`
}

type teamJSON struct {
	ID     int64      `json:"id"`
	Name   string     `json:"name"`
	Amount int64      `json:"amount"`
	Speed  int64      `json:"speed"`
	Cells  []cellJSON `json:"cells"`
}

type cellJSON struct {
	Cell  int    `json:"cell"`
	State string `json:"state"`
	Tests int    `json:"tests"`
	// Только в командном/админском варианте:
	URL       string `json:"url,omitempty"`
	ChapterID int    `json:"chapter_id,omitempty"`
	TaskID    int64  `json:"task_id,omitempty"`
	Level     int    `json:"level,omitempty"`
}

// buildState собирает JSON состояния. include: "public" — без ссылок вообще;
// "team" — ссылки и порядок только для команды forTeamID; "admin" — всё для
// всех команд.
func (s *Server) buildState(snap *gameSnapshot, include string, forTeamID int64) *stateJSON {
	g := snap.Game
	out := &stateJSON{
		ServerTime:     snap.Now.Format(time.RFC3339),
		Status:         snap.Status,
		N:              g.N,
		PageRefreshSec: s.pageRefresh.Seconds(),
	}
	if g.StartAt != nil {
		v := g.StartAt.UTC().Format(time.RFC3339)
		out.StartAt = &v
		e := g.EndAt().UTC().Format(time.RFC3339)
		out.EndAt = &e
	}
	for _, l := range snap.Levels {
		out.Levels = append(out.Levels, levelJSON{l.TaskCost, l.TestCost, l.Load, l.AmountBonus, l.SpeedBonus})
	}
	taskByID := map[int64]store.Task{}
	for _, t := range snap.Tasks {
		taskByID[t.ID] = t
	}
	n2 := g.N * g.N
	for _, tm := range snap.Teams {
		ts := snap.Result.Teams[tm.ID]
		tj := teamJSON{ID: tm.ID, Name: tm.Name, Amount: ts.Amount, Speed: ts.Speed}
		order := snap.Orders[tm.ID]
		for cell := 1; cell <= n2; cell++ {
			cj := cellJSON{Cell: cell, State: game.StateHidden}
			if taskID, ok := order[cell]; ok {
				if st := ts.Tasks[taskID]; st != nil {
					cj.State = st.State
					cj.Tests = st.Tests
				}
				withDetails := include == "admin" || (include == "team" && tm.ID == forTeamID)
				if withDetails {
					if task, ok := taskByID[taskID]; ok {
						cj.URL = task.URL
						cj.ChapterID = task.ChapterID
						cj.TaskID = task.ID
						cj.Level = task.Level
					}
				}
			}
			tj.Cells = append(tj.Cells, cj)
		}
		out.Teams = append(out.Teams, tj)
	}
	return out
}
