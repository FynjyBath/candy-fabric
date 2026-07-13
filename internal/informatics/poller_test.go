package informatics

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"candyfactory/internal/store"
)

// testGame создаёт в хранилище игру с одной командой и одной задачей уровня 1.
func testGame(t *testing.T, st *store.Store, start time.Time) (*store.Game, store.Team, store.Task) {
	t.Helper()
	gid, err := st.CreateGame(
		&store.Game{Title: "Тест", N: 1, StartAmount: 20000, StartSpeed: 15, DurationSec: 5100, StartAt: &start},
		[]store.Level{{Level: 1, TaskCost: 12000, TestCost: 3000, Load: 2, AmountBonus: 12000, SpeedBonus: 4}},
		[][]store.TaskInput{{{ChapterID: 111, URL: "https://informatics.msk.ru/mod/statements/view.php?chapterid=111"}}},
		[]store.TeamInput{{Name: "К1", InformaticsUserID: 777, Login: "t1", Password: "p"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	g, err := st.GetGame(gid)
	if err != nil {
		t.Fatal(err)
	}
	teams, _ := st.GetTeams(gid)
	tasks, _ := st.GetTasks(gid)
	return g, teams[0], tasks[0]
}

func newPoller(t *testing.T, st *store.Store) *Poller {
	t.Helper()
	return &Poller{
		Store:  st,
		Cache:  OpenCache(filepath.Join(t.TempDir(), "cache.json")),
		Logger: log.New(io.Discard, "", 0),
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMatchRunCreatesSolve(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	g, team, task := testGame(t, st, start)
	// Задача куплена за 5 минут до посылки.
	buyAt := start.Add(2 * time.Minute)
	taskID := task.ID
	if _, err := st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID,
		Type: "buy_task", At: buyAt, Source: "manual", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	p := newPoller(t, st)
	runAt := start.Add(5 * time.Minute)
	if _, err := p.matchRun(g, team, Run{ID: 1001, CreateTime: runAt, EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	events, _ := st.GetEvents(g.ID)
	var solve *store.Event
	for _, e := range events {
		if e.Type == "solve" {
			solve = e
		}
	}
	if solve == nil {
		t.Fatal("событие solve не создано")
	}
	if solve.Source != "auto" || solve.RunID == nil || *solve.RunID != 1001 || !solve.At.Equal(runAt) {
		t.Errorf("некорректное auto-событие: %+v", solve)
	}
	// Повторный матчинг той же посылки — дубль не создаётся.
	if _, err := p.matchRun(g, team, Run{ID: 1001, CreateTime: runAt, EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	events, _ = st.GetEvents(g.ID)
	if len(events) != 2 {
		t.Errorf("после повторного матчинга %d событий, ожидалось 2", len(events))
	}
}

func TestMatchRunNotBoughtAnomaly(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	g, team, _ := testGame(t, st, start)
	p := newPoller(t, st)
	runAt := start.Add(5 * time.Minute)
	if _, err := p.matchRun(g, team, Run{ID: 2001, CreateTime: runAt, EjudgeStatus: 8, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	anomalies, _ := st.GetAnomalies(g.ID, true)
	if len(anomalies) != 1 || anomalies[0].Reason != "not_bought" {
		t.Fatalf("ожидалась аномалия not_bought, получено %+v", anomalies)
	}
	// Пока аномалия не решена, опросчик по этой посылке ничего не создаёт.
	if _, err := p.matchRun(g, team, Run{ID: 2001, CreateTime: runAt, EjudgeStatus: 8, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	anomalies, _ = st.GetAnomalies(g.ID, false)
	if len(anomalies) != 1 {
		t.Errorf("дубль аномалии: %d записей", len(anomalies))
	}
	// «Принять» — создаётся ручное solve со временем посылки.
	if err := st.AcceptAnomaly(anomalies[0]); err != nil {
		t.Fatal(err)
	}
	events, _ := st.GetEvents(g.ID)
	if len(events) != 1 || events[0].Type != "solve" || events[0].Source != "manual" || !events[0].At.Equal(runAt) {
		t.Errorf("после «Принять» ожидалось ручное solve со временем посылки, получено %+v", events)
	}
}

// По ТЗ 7.5: если по паре (команда, задача) уже есть включённое событие solve,
// новая посылка пропускается молча (ни события, ни аномалии).
func TestMatchRunSkippedWhenSolveExists(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	g, team, task := testGame(t, st, start)
	taskID := task.ID
	st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID, Type: "buy_task", At: start.Add(time.Minute), Source: "manual", Enabled: true})
	st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID, Type: "solve", At: start.Add(2 * time.Minute), Source: "manual", Enabled: true})
	p := newPoller(t, st)
	if _, err := p.matchRun(g, team, Run{ID: 3001, CreateTime: start.Add(5 * time.Minute), EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	anomalies, _ := st.GetAnomalies(g.ID, false)
	events, _ := st.GetEvents(g.ID)
	if len(anomalies) != 0 || len(events) != 2 {
		t.Errorf("повторная посылка по решённой задаче должна пропускаться молча: %d аномалий, %d событий", len(anomalies), len(events))
	}
}

func TestMatchRunOutOfTimeAnomaly(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour) // игра закончилась
	g, team, task := testGame(t, st, start)
	taskID := task.ID
	st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID, Type: "buy_task", At: start.Add(time.Minute), Source: "manual", Enabled: true})
	p := newPoller(t, st)
	// Посылка после конца игры (t_end = start + 5100 c).
	if _, err := p.matchRun(g, team, Run{ID: 4001, CreateTime: start.Add(2 * time.Hour), EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	anomalies, _ := st.GetAnomalies(g.ID, true)
	if len(anomalies) != 1 || anomalies[0].Reason != "out_of_time" {
		t.Errorf("ожидалась аномалия out_of_time, получено %+v", anomalies)
	}
}

// Посылка, отправленная до конца, но обнаруженная после конца игры, зачитывается
// (критерий приёмки 10) — важно именно время посылки.
func TestMatchRunAtFlagCounted(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-90 * time.Minute) // finished (5100 c = 85 мин), но в 15-минутном окне
	g, team, task := testGame(t, st, start)
	taskID := task.ID
	st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID, Type: "buy_task", At: start.Add(time.Minute), Source: "manual", Enabled: true})
	p := newPoller(t, st)
	runAt := start.Add(84 * time.Minute) // за минуту до конца
	if _, err := p.matchRun(g, team, Run{ID: 5001, CreateTime: runAt, EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	events, _ := st.GetEvents(g.ID)
	found := false
	for _, e := range events {
		if e.Type == "solve" && e.Source == "auto" {
			found = true
		}
	}
	if !found {
		t.Errorf("посылка «на флажке» не зачтена")
	}
}

// Отключённое auto-решение не возвращается опросчиком (критерий 7):
// уникальность run_id блокирует пересоздание.
func TestDisabledAutoSolveNotRecreated(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	g, team, task := testGame(t, st, start)
	taskID := task.ID
	st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID, Type: "buy_task", At: start.Add(time.Minute), Source: "manual", Enabled: true})
	p := newPoller(t, st)
	runAt := start.Add(5 * time.Minute)
	if _, err := p.matchRun(g, team, Run{ID: 6001, CreateTime: runAt, EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	events, _ := st.GetEvents(g.ID)
	var auto *store.Event
	for _, e := range events {
		if e.Source == "auto" {
			auto = e
		}
	}
	if err := st.SetEventEnabled(auto.ID, g.ID, false); err != nil {
		t.Fatal(err)
	}
	// Опросчик снова видит ту же посылку.
	if _, err := p.matchRun(g, team, Run{ID: 6001, CreateTime: runAt, EjudgeStatus: 0, ProblemID: 111}); err != nil {
		t.Fatal(err)
	}
	events, _ = st.GetEvents(g.ID)
	cnt := 0
	for _, e := range events {
		if e.Source == "auto" {
			cnt++
			if e.Enabled {
				t.Errorf("отключённое auto-решение снова включено")
			}
		}
	}
	if cnt != 1 {
		t.Errorf("auto-событий %d, ожидалось 1 (без дублей)", cnt)
	}
}

// Ручная (математическая) игра не опрашивается вовсе: аккаунтов нет,
// pollOnce завершает цикл, не притрагиваясь к клиенту (Client == nil).
func TestManualGameNotPolled(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	gid, err := st.CreateGame(
		&store.Game{Title: "Матбой", Mode: store.ModeManual, N: 1,
			StartAmount: 20000, StartSpeed: 15, DurationSec: 5100, StartAt: &start},
		[]store.Level{{Level: 1, TaskCost: 1000, TestCost: 500, Load: 1, AmountBonus: 2000, SpeedBonus: 1}},
		[][]store.TaskInput{{{ChapterID: -1001, URL: ""}}},
		[]store.TeamInput{{Name: "К", InformaticsUserID: 0, Login: "k", Password: "p"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoller(t, st)
	p.Client = nil // упадёт с паникой, если опросчик попробует выкачивать
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.pollOnce(ctx, time.Millisecond)
	if msg, _ := p.LastError(); msg != "" {
		t.Errorf("опрос ручной игры дал ошибку: %s", msg)
	}
	events, _ := st.GetEvents(gid)
	if len(events) != 0 {
		t.Errorf("у ручной игры появились события от опросчика")
	}
}

// CheckGame по черновику (игра не стартовала) находит уже решённые командой
// задачи и помечает их аномалиями out_of_time — до старта игры.
func TestCheckGameDraftFindsPreSolved(t *testing.T) {
	st := openStore(t)
	// Черновик: без start_at. Задача chapterid=111 (её решает sampleRuns).
	gid, err := st.CreateGame(
		&store.Game{Title: "Черновик", N: 1, StartAmount: 20000, StartSpeed: 15, DurationSec: 5100},
		[]store.Level{{Level: 1, TaskCost: 12000, TestCost: 3000, Load: 2, AmountBonus: 12000, SpeedBonus: 4}},
		[][]store.TaskInput{{{ChapterID: 111, URL: "https://informatics.msk.ru/mod/statements/view.php?chapterid=111"}}},
		[]store.TeamInput{{Name: "К", InformaticsUserID: 777, Login: "t", Password: "p"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockInformatics{runsJSON: sampleRuns} // run 100: problem 111, status 0 (решено)
	p := newPoller(t, st)
	p.Client = newMockClient(t, m)
	p.AccountPause = time.Millisecond

	p.CheckGame(gid)

	anomalies, _ := st.GetAnomalies(gid, true)
	if len(anomalies) != 1 || anomalies[0].Reason != "out_of_time" {
		t.Fatalf("ожидалась аномалия out_of_time по пред-решённой задаче, получено %+v", anomalies)
	}
	// Событий solve нет (игра не идёт).
	events, _ := st.GetEvents(gid)
	if len(events) != 0 {
		t.Errorf("у черновика не должно быть solve-событий, получено %d", len(events))
	}
	// Повторная проверка не дублирует аномалию (дедуп по run_id).
	p.CheckGame(gid)
	anomalies, _ = st.GetAnomalies(gid, false)
	if len(anomalies) != 1 {
		t.Errorf("повторная проверка продублировала аномалию: %d", len(anomalies))
	}
}

// CheckGame ручной игры не обращается к информатиксу (Client не трогается).
func TestCheckGameManualSkipped(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC()
	gid, err := st.CreateGame(
		&store.Game{Title: "Матбой", Mode: store.ModeManual, N: 1,
			StartAmount: 20000, StartSpeed: 15, DurationSec: 5100, StartAt: &start},
		[]store.Level{{Level: 1, TaskCost: 1000, TestCost: 500, Load: 1, AmountBonus: 2000, SpeedBonus: 1}},
		[][]store.TaskInput{{{ChapterID: -1001}}},
		[]store.TeamInput{{Name: "К", Login: "k", Password: "p"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoller(t, st)
	p.Client = nil // паника, если CheckGame полезет в информатикс
	p.CheckGame(gid)
	if a, _ := st.GetAnomalies(gid, false); len(a) != 0 {
		t.Errorf("ручная игра не должна давать аномалий")
	}
}

// matchRun возвращает 1 при засчитанном решении и 0 при аномалии/пропуске.
func TestMatchRunReturnsSolveCount(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	g, team, task := testGame(t, st, start)
	taskID := task.ID
	st.AddEvent(&store.Event{GameID: g.ID, TeamID: team.ID, TaskID: &taskID,
		Type: "buy_task", At: start.Add(time.Minute), Source: "manual", Enabled: true})
	p := newPoller(t, st)
	// Куплена → решение засчитано (1).
	if n, err := p.matchRun(g, team, Run{ID: 8001, CreateTime: start.Add(5 * time.Minute), EjudgeStatus: 0, ProblemID: 111}); err != nil || n != 1 {
		t.Errorf("solve: n=%d err=%v, ожидалось 1", n, err)
	}
	// Чужая задача → 0.
	if n, _ := p.matchRun(g, team, Run{ID: 8002, CreateTime: start.Add(6 * time.Minute), EjudgeStatus: 0, ProblemID: 99999}); n != 0 {
		t.Errorf("чужая задача: n=%d, ожидалось 0", n)
	}
	// Вне времени → аномалия, 0.
	if n, _ := p.matchRun(g, team, Run{ID: 8003, CreateTime: start.Add(-time.Hour), EjudgeStatus: 0, ProblemID: 111}); n != 0 {
		t.Errorf("out_of_time: n=%d, ожидалось 0", n)
	}
}

// Посылка по чужой задаче игнорируется молча.
func TestForeignProblemIgnored(t *testing.T) {
	st := openStore(t)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	g, team, _ := testGame(t, st, start)
	p := newPoller(t, st)
	if _, err := p.matchRun(g, team, Run{ID: 7001, CreateTime: start.Add(time.Minute), EjudgeStatus: 0, ProblemID: 99999}); err != nil {
		t.Fatal(err)
	}
	events, _ := st.GetEvents(g.ID)
	anomalies, _ := st.GetAnomalies(g.ID, false)
	if len(events) != 0 || len(anomalies) != 0 {
		t.Errorf("чужая задача породила записи: %d событий, %d аномалий", len(events), len(anomalies))
	}
}
