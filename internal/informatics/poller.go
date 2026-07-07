package informatics

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"candyfactory/internal/game"
	"candyfactory/internal/store"
)

// Poller — фоновый опросчик (7.5): один цикл с периодом pollInterval обходит
// аккаунты команд всех игр в статусе running (плюс finished менее 15 минут
// назад), инкрементально выкачивает посылки и матчит их в события/аномалии.
type Poller struct {
	Store        *store.Store
	Client       *Client
	Cache        *Cache
	Logger       *log.Logger
	PollInterval time.Duration
	AccountPause time.Duration // пауза между аккаунтами, >= 1 c

	mu          sync.Mutex
	lastError   string
	lastErrAt   time.Time
	cycleHadErr bool
}

// FinishedGrace — сколько опрашивать после конца игры (посылки «на флажке»).
const FinishedGrace = 15 * time.Minute

// LastError — последняя ошибка опросчика для баннера в админке.
func (p *Poller) LastError() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError, p.lastErrAt
}

func (p *Poller) setError(err error) {
	p.mu.Lock()
	p.lastError = err.Error()
	p.lastErrAt = time.Now()
	p.cycleHadErr = true
	p.mu.Unlock()
	p.Logger.Printf("ERROR опросчик: %v", err)
}

// finishCycle сбрасывает баннер ошибки, если цикл прошёл без ошибок:
// связь восстановилась — баннер в админке гаснет.
func (p *Poller) finishCycle() {
	p.mu.Lock()
	if !p.cycleHadErr {
		p.lastError = ""
		p.lastErrAt = time.Time{}
	}
	p.cycleHadErr = false
	p.mu.Unlock()
}

// Run запускает цикл опроса до отмены контекста.
func (p *Poller) Run(ctx context.Context) {
	interval := p.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	pause := p.AccountPause
	if pause < time.Second {
		pause = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		p.pollOnce(ctx, pause)
		p.finishCycle()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollOnce — один цикл обхода. Ошибка одного аккаунта не прерывает остальные.
func (p *Poller) pollOnce(ctx context.Context, pause time.Duration) {
	games, err := p.activeGames()
	if err != nil {
		p.setError(fmt.Errorf("список активных игр: %w", err))
		return
	}
	// Аккаунт может участвовать в нескольких играх — собираем множество.
	type gameTeam struct {
		g    *store.Game
		team store.Team
	}
	accounts := map[int][]gameTeam{}
	for _, g := range games {
		teams, err := p.Store.GetTeams(g.ID)
		if err != nil {
			p.setError(fmt.Errorf("команды игры %d: %w", g.ID, err))
			continue
		}
		for _, t := range teams {
			accounts[t.InformaticsUserID] = append(accounts[t.InformaticsUserID], gameTeam{g, t})
		}
	}
	if len(accounts) == 0 {
		return
	}
	first := true
	for userID, gts := range accounts {
		if ctx.Err() != nil {
			return
		}
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
		}
		first = false
		runs, newMax, err := FetchNewRuns(p.Client, p.Cache, userID)
		if err != nil {
			p.setError(fmt.Errorf("аккаунт %d: %w", userID, err))
			continue // один упавший аккаунт не прерывает обход
		}
		matchedOK := true
		for _, r := range runs {
			if !r.Solved() {
				continue // остальные вердикты — просто попытки, не учитываются
			}
			for _, gt := range gts {
				if err := p.matchRun(gt.g, gt.team, r); err != nil {
					matchedOK = false
					p.setError(fmt.Errorf("матчинг посылки %d (игра %d): %w", r.ID, gt.g.ID, err))
				}
			}
		}
		// Водяной знак двигаем только после успешной обработки: при сбое
		// матчинга посылки будут выкачаны снова (дубли исключает run_id в БД).
		if matchedOK {
			if err := CommitMaxRunID(p.Cache, userID, newMax); err != nil {
				p.setError(fmt.Errorf("сохранение кеша аккаунта %d: %w", userID, err))
			}
		}
	}
}

// activeGames — игры в статусе running плюс finished менее 15 минут назад.
func (p *Poller) activeGames() ([]*store.Game, error) {
	all, err := p.Store.ListGames(false)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []*store.Game
	for _, g := range all {
		// Ручной (математический) режим: информатикс не при чём,
		// аккаунтов нет — игру не опрашиваем вовсе.
		if g.Mode == store.ModeManual {
			continue
		}
		switch g.Status(now) {
		case "running":
			out = append(out, g)
		case "finished":
			if end := g.EndAt(); end != nil && now.Sub(*end) < FinishedGrace {
				out = append(out, g)
			}
		}
	}
	return out, nil
}

// matchRun матчит решённую посылку в конкретной игре (7.5 п.3–4):
// по problem.id ищется задача игры, дальше — событие solve или аномалия.
func (p *Poller) matchRun(g *store.Game, team store.Team, r Run) error {
	tasks, err := p.Store.GetTasks(g.ID)
	if err != nil {
		return err
	}
	var task *store.Task
	for i := range tasks {
		if tasks[i].ChapterID == r.ProblemID {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		return nil // посылка по чужой задаче — игнорировать молча
	}
	// Дедупликация: запись с тем же run_id (событие или аномалия) — пропустить.
	if has, err := p.Store.HasRunRecord(g.ID, r.ID); err != nil {
		return err
	} else if has {
		return nil
	}
	// Уже есть включённое solve по паре (команда, задача) — пропустить.
	if has, err := p.Store.HasEnabledSolve(g.ID, team.ID, task.ID); err != nil {
		return err
	} else if has {
		return nil
	}

	tRun := r.CreateTime
	t0 := *g.StartAt // активная игра всегда имеет start_at
	tEnd := *g.EndAt()

	reason := ""
	if tRun.Before(t0) || tRun.After(tEnd) {
		reason = "out_of_time"
	} else {
		events, err := p.Store.GetEvents(g.ID)
		if err != nil {
			return err
		}
		switch game.TaskStateAt(events, team.ID, task.ID, tRun) {
		case game.StateBought:
			// всё по правилам — событие solve
		case game.StatePassed:
			reason = "already_passed"
		default:
			reason = "not_bought"
		}
	}
	if reason == "" {
		runID := r.ID
		_, err := p.Store.AddEvent(&store.Event{
			GameID: g.ID, TeamID: team.ID, TaskID: &task.ID,
			Type: "solve", At: tRun, Source: "auto", RunID: &runID, Enabled: true,
		})
		if err != nil {
			return err
		}
		p.Logger.Printf("INFO опросчик: событие solve (игра %d, команда %q, chapterid %d, run %d, %s)",
			g.ID, team.Name, task.ChapterID, r.ID, tRun.Format(time.RFC3339))
		return nil
	}
	err = p.Store.AddAnomaly(&store.Anomaly{
		GameID: g.ID, TeamID: team.ID, TaskID: task.ID,
		RunID: r.ID, RunAt: tRun, Reason: reason,
	})
	if err != nil {
		return err
	}
	p.Logger.Printf("WARN опросчик: аномалия %s (игра %d, команда %q, chapterid %d, run %d, %s)",
		reason, g.ID, team.Name, task.ChapterID, r.ID, tRun.Format(time.RFC3339))
	return nil
}
