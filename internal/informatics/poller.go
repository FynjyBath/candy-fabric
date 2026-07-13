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
	lastPollAt  time.Time // время последнего завершённого цикла
	lastSolves  int       // засчитано решений в последнем цикле
	totalSolves int       // засчитано решений за всё время работы
}

// FinishedGrace — сколько опрашивать после конца игры (посылки «на флажке»).
const FinishedGrace = 15 * time.Minute

// pendingRepoll — если в цикле были ещё не оценённые посылки, следующий цикл
// делаем раньше: их OK нужно поймать без ожидания полного интервала.
const pendingRepoll = 12 * time.Second

// LastError — последняя ошибка опросчика для баннера в админке.
func (p *Poller) LastError() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError, p.lastErrAt
}

// Status — диагностика опросчика для админки: время последнего цикла и
// сколько решений засчитано (в последнем цикле и всего).
func (p *Poller) Status() (lastPollAt time.Time, lastSolves, totalSolves int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastPollAt, p.lastSolves, p.totalSolves
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

// Run запускает цикл опроса до отмены контекста. Если в цикле были ещё не
// оценённые посылки, следующий цикл делается раньше (pendingRepoll), чтобы их
// поздний OK засчитывался быстрее.
func (p *Poller) Run(ctx context.Context) {
	interval := p.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	pause := p.AccountPause
	if pause < time.Second {
		pause = time.Second
	}
	for {
		sawPending := p.pollOnce(ctx, pause)
		p.finishCycle()
		next := interval
		if sawPending && pendingRepoll < next {
			next = pendingRepoll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// pollOnce — один цикл обхода. Ошибка одного аккаунта не прерывает остальные.
// Возвращает sawPending: были ли посылки с ещё не окончательным вердиктом
// (тогда следующий цикл делается раньше).
func (p *Poller) pollOnce(ctx context.Context, pause time.Duration) (sawPending bool) {
	games, err := p.activeGames()
	if err != nil {
		p.setError(fmt.Errorf("список активных игр: %w", err))
		return false
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
		return false
	}
	solves, newRuns := 0, 0
	first := true
	for userID, gts := range accounts {
		if ctx.Err() != nil {
			return sawPending
		}
		if !first {
			select {
			case <-ctx.Done():
				return sawPending
			case <-time.After(pause):
			}
		}
		first = false
		runs, newMax, err := FetchNewRuns(p.Client, p.Cache, userID)
		if err != nil {
			p.setError(fmt.Errorf("аккаунт %d: %w", userID, err))
			continue // один упавший аккаунт не прерывает обход
		}
		newRuns += len(runs)
		matchedOK := true
		for _, r := range runs {
			if r.Pending() {
				sawPending = true // ещё тестируется — переопросим раньше
			}
			if !r.Solved() {
				continue // остальные вердикты — просто попытки, не учитываются
			}
			for _, gt := range gts {
				n, err := p.matchRun(gt.g, gt.team, r)
				if err != nil {
					matchedOK = false
					p.setError(fmt.Errorf("матчинг посылки %d (игра %d): %w", r.ID, gt.g.ID, err))
				}
				solves += n
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
	p.mu.Lock()
	p.lastPollAt = time.Now()
	p.lastSolves = solves
	p.totalSolves += solves
	p.mu.Unlock()
	if newRuns > 0 || solves > 0 {
		note := ""
		if sawPending {
			note = ", есть ещё не оценённые (переопрос раньше)"
		}
		p.Logger.Printf("INFO опросчик: аккаунтов %d, новых посылок %d, засчитано решений %d%s",
			len(accounts), newRuns, solves, note)
	}
	return sawPending
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

// CheckGame — разовая проверка одной игры против информатикса, работает в
// любом статусе (в т. ч. черновик). Для каждой команды выкачивает последние
// посылки и матчит их (создавая события/аномалии), не трогая водяной знак
// непрерывного опросчика — чтобы не пересечься с обходом других игр,
// делящих аккаунт. Позволяет после сохранения конфигурации сразу увидеть уже
// решённые задачи и аномалии, не дожидаясь старта. Запускать в горутине.
func (p *Poller) CheckGame(gameID int64) {
	g, err := p.Store.GetGame(gameID)
	if err != nil {
		p.setError(fmt.Errorf("проверка игры %d: %w", gameID, err))
		return
	}
	if g.Mode == store.ModeManual {
		return // ручной режим — информатикса нет
	}
	teams, err := p.Store.GetTeams(gameID)
	if err != nil {
		p.setError(fmt.Errorf("проверка игры %d: команды: %w", gameID, err))
		return
	}
	pause := p.AccountPause
	if pause < time.Second {
		pause = time.Second
	}
	solves, anomalies := 0, 0
	for i, team := range teams {
		if team.InformaticsUserID <= 0 {
			continue
		}
		if i > 0 {
			time.Sleep(pause)
		}
		// Страница 1 — новейшие 1000 посылок аккаунта: для аккаунта команды
		// этого достаточно, чтобы найти уже сделанные решения по задачам игры.
		runs, _, err := p.Client.FetchRunsPage(team.InformaticsUserID, 1)
		if err != nil {
			p.setError(fmt.Errorf("проверка игры %d: аккаунт %d: %w", gameID, team.InformaticsUserID, err))
			continue
		}
		for _, r := range runs {
			if !r.Solved() {
				continue
			}
			n, err := p.matchRun(g, team, r)
			if err != nil {
				p.setError(fmt.Errorf("проверка игры %d: матчинг посылки %d: %w", gameID, r.ID, err))
				continue
			}
			solves += n
			if n == 0 {
				anomalies++ // matchRun по решённой посылке без solve = аномалия/пропуск
			}
		}
	}
	p.Logger.Printf("INFO проверка игры %d завершена: засчитано решений %d, отмечено записей-аномалий %d",
		gameID, solves, anomalies)
}

// matchRun матчит решённую посылку в конкретной игре (7.5 п.3–4):
// по problem.id ищется задача игры, дальше — событие solve или аномалия.
// Возвращает 1, если создано событие solve, иначе 0.
func (p *Poller) matchRun(g *store.Game, team store.Team, r Run) (int, error) {
	tasks, err := p.Store.GetTasks(g.ID)
	if err != nil {
		return 0, err
	}
	var task *store.Task
	for i := range tasks {
		if tasks[i].ChapterID == r.ProblemID {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		return 0, nil // посылка по чужой задаче — игнорировать молча
	}
	// Дедупликация: запись с тем же run_id (событие или аномалия) — пропустить.
	if has, err := p.Store.HasRunRecord(g.ID, r.ID); err != nil {
		return 0, err
	} else if has {
		return 0, nil
	}
	// Уже есть включённое solve по паре (команда, задача) — пропустить.
	if has, err := p.Store.HasEnabledSolve(g.ID, team.ID, task.ID); err != nil {
		return 0, err
	} else if has {
		return 0, nil
	}

	tRun := r.CreateTime

	reason := ""
	if g.StartAt == nil {
		// Игра ещё не стартовала (проверка конфигурации): любое решение —
		// вне окна игры. Так организатор до старта видит уже решённые задачи.
		reason = "out_of_time"
	} else if t0, tEnd := *g.StartAt, *g.EndAt(); tRun.Before(t0) || tRun.After(tEnd) {
		reason = "out_of_time"
	} else {
		events, err := p.Store.GetEvents(g.ID)
		if err != nil {
			return 0, err
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
			return 0, err
		}
		p.Logger.Printf("INFO опросчик: засчитано решение (игра %d, команда %q, chapterid %d, run %d, время %s)",
			g.ID, team.Name, task.ChapterID, r.ID, tRun.Local().Format("02.01 15:04:05"))
		return 1, nil
	}
	// Аномалия видна только в админке; логируем контекст, чтобы причину
	// (особенно out_of_time — часто это несовпадение времени/пояса) было
	// сразу видно в логе.
	extra := ""
	if reason == "out_of_time" {
		if g.StartAt == nil {
			extra = ", игра ещё не стартовала (проверка конфигурации)"
		} else {
			extra = fmt.Sprintf(", окно игры %s..%s",
				g.StartAt.Local().Format("02.01 15:04:05"), g.EndAt().Local().Format("15:04:05"))
		}
	}
	if !r.TimeParsed {
		extra += " (create_time не разобрано — взят момент обнаружения)"
	}
	if err := p.Store.AddAnomaly(&store.Anomaly{
		GameID: g.ID, TeamID: team.ID, TaskID: task.ID,
		RunID: r.ID, RunAt: tRun, Reason: reason,
	}); err != nil {
		return 0, err
	}
	p.Logger.Printf("WARN опросчик: аномалия %s (игра %d, команда %q, chapterid %d, run %d, время посылки %s%s)",
		reason, g.ID, team.Name, task.ChapterID, r.ID, tRun.Local().Format("02.01 15:04:05"), extra)
	return 0, nil
}
