package game

import (
	"testing"
	"time"

	"candyfactory/internal/store"
)

func mkGame(start time.Time) *store.Game {
	return &store.Game{
		ID: 1, Title: "t", N: 3,
		StartAmount: 20000, StartSpeed: 15, DurationSec: 5100,
		StartAt: &start,
	}
}

var testLevels = []store.Level{
	{Level: 1, TaskCost: 12000, TestCost: 3000, Load: 2, AmountBonus: 12000, SpeedBonus: 4},
	{Level: 2, TaskCost: 12000, TestCost: 7000, Load: 2, AmountBonus: 25000, SpeedBonus: 7},
	{Level: 3, TaskCost: 12000, TestCost: 10000, Load: 2, AmountBonus: 50000, SpeedBonus: 11},
}

var testTasks = []store.Task{
	{ID: 101, GameID: 1, Level: 1, Ord: 1, ChapterID: 11},
	{ID: 102, GameID: 1, Level: 2, Ord: 1, ChapterID: 22},
	{ID: 103, GameID: 1, Level: 3, Ord: 1, ChapterID: 33},
}

var testTeams = []store.Team{{ID: 1, GameID: 1, Name: "A"}, {ID: 2, GameID: 1, Name: "B"}}

func ev(id int64, teamID int64, taskID int64, typ string, at time.Time, enabled bool) *store.Event {
	return &store.Event{ID: id, GameID: 1, TeamID: teamID, TaskID: &taskID, Type: typ, At: at, Source: "manual", Enabled: enabled}
}

func TestProductionOnly(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	res := Compute(g, testLevels, testTasks, testTeams, nil, t0.Add(100*time.Second))
	want := int64(20000 + 15*100)
	if res.Teams[1].Amount != want {
		t.Errorf("amount = %d, ожидалось %d", res.Teams[1].Amount, want)
	}
	if res.Teams[1].Speed != 15 {
		t.Errorf("speed = %d, ожидалось 15", res.Teams[1].Speed)
	}
}

func TestBeforeStartAndAfterEnd(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	// До старта — стартовые значения.
	res := Compute(g, testLevels, testTasks, testTeams, nil, t0.Add(-time.Hour))
	if res.Teams[1].Amount != 20000 {
		t.Errorf("до старта amount = %d", res.Teams[1].Amount)
	}
	// После конца начисление останавливается: T = t_end.
	res = Compute(g, testLevels, testTasks, testTeams, nil, t0.Add(10*time.Hour))
	want := int64(20000 + 15*5100)
	if res.Teams[1].Amount != want {
		t.Errorf("после конца amount = %d, ожидалось %d", res.Teams[1].Amount, want)
	}
}

func TestBuySolveEffects(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	events := []*store.Event{
		ev(1, 1, 101, "buy_task", t0.Add(10*time.Second), true),
		ev(2, 1, 101, "buy_test", t0.Add(20*time.Second), true),
		ev(3, 1, 101, "solve", t0.Add(30*time.Second), true),
	}
	res := Compute(g, testLevels, testTasks, testTeams, events, t0.Add(40*time.Second))
	// Событие с at <= t0+k применяется до начисления за секунду k, поэтому:
	// секунды 1..9 по 15; покупка (−12000, скорость 13); секунды 10..19 по 13;
	// тест (−3000); секунды 20..29 по 13; solve (+12000, скорость 13+4+2=19);
	// секунды 30..40 по 19.
	wantAmount := int64(20000 + 9*15 - 12000 + 10*13 - 3000 + 10*13 + 12000 + 11*19)
	ts := res.Teams[1]
	if ts.Amount != wantAmount {
		t.Errorf("amount = %d, ожидалось %d", ts.Amount, wantAmount)
	}
	if ts.Speed != 19 {
		t.Errorf("speed = %d, ожидалось 19", ts.Speed)
	}
	if st := ts.Tasks[101]; st.State != StatePassed || st.Tests != 1 {
		t.Errorf("задача 101: %+v, ожидалось passed/1 тест", st)
	}
	// Время сдачи = время события solve.
	if st := ts.Tasks[101]; !st.SolvedAt.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("SolvedAt = %v, ожидалось %v", st.SolvedAt, t0.Add(30*time.Second))
	}
	// Вторая команда не затронута.
	if res.Teams[2].Amount != 20000+40*15 {
		t.Errorf("команда 2 amount = %d", res.Teams[2].Amount)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("неожиданные предупреждения: %+v", res.Warnings)
	}
}

// Критерий приёмки 7: смещение покупки на 10 минут назад увеличивает текущие
// запасы ровно на 10*60*Load_r (недоначисленная скорость).
func TestBackdatedBuyShiftsAmountByLoad(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	now := t0.Add(40 * time.Minute)
	at1 := t0.Add(30 * time.Minute)
	at2 := t0.Add(20 * time.Minute)
	ev1 := []*store.Event{ev(1, 1, 101, "buy_task", at1, true)}
	ev2 := []*store.Event{ev(1, 1, 101, "buy_task", at2, true)}
	a1 := Compute(g, testLevels, testTasks, testTeams, ev1, now).Teams[1].Amount
	a2 := Compute(g, testLevels, testTasks, testTeams, ev2, now).Teams[1].Amount
	wantDiff := int64(10 * 60 * 2) // Load_1 = 2
	if a1-a2 != wantDiff {
		t.Errorf("разница запасов = %d, ожидалось %d", a1-a2, wantDiff)
	}
}

func TestDisabledEventIgnored(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	events := []*store.Event{ev(1, 1, 101, "buy_task", t0.Add(10*time.Second), false)}
	res := Compute(g, testLevels, testTasks, testTeams, events, t0.Add(20*time.Second))
	if res.Teams[1].Amount != 20000+20*15 {
		t.Errorf("отключённое событие повлияло на расчёт: %d", res.Teams[1].Amount)
	}
	if res.Teams[1].Tasks[101].State != StateHidden {
		t.Errorf("отключённое событие поменяло состояние задачи")
	}
}

// События с at < t0 трактуются как произошедшие в t0 (эффект до 1-й секунды),
// и валидатор их подсвечивает.
func TestEventBeforeStart(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	events := []*store.Event{ev(1, 1, 101, "buy_task", t0.Add(-time.Hour), true)}
	res := Compute(g, testLevels, testTasks, testTeams, events, t0.Add(10*time.Second))
	// Покупка применяется до первого начисления: скорость 13 все 10 секунд.
	want := int64(20000 - 12000 + 10*13)
	if res.Teams[1].Amount != want {
		t.Errorf("amount = %d, ожидалось %d", res.Teams[1].Amount, want)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("ожидалось предупреждение о событии раньше t0")
	}
}

// Событие «не по правилам» эффект всё равно применяет, но подсвечивается.
func TestOutOfOrderEventAppliesWithWarning(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	events := []*store.Event{ev(1, 1, 101, "solve", t0.Add(10*time.Second), true)} // solve без покупки
	res := Compute(g, testLevels, testTasks, testTeams, events, t0.Add(20*time.Second))
	ts := res.Teams[1]
	if ts.Tasks[101].State != StatePassed {
		t.Errorf("состояние = %s, ожидалось passed", ts.Tasks[101].State)
	}
	if ts.Speed != 15+4+2 {
		t.Errorf("speed = %d, ожидалось 21", ts.Speed)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("ожидалось предупреждение solve-без-покупки")
	}
}

func TestNegativeAmountWarning(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	g := mkGame(t0)
	g.StartAmount = 1000
	events := []*store.Event{ev(1, 1, 101, "buy_task", t0.Add(5*time.Second), true)}
	res := Compute(g, testLevels, testTasks, testTeams, events, t0.Add(10*time.Second))
	found := false
	for _, w := range res.Warnings {
		if w.EventID == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("ожидалось предупреждение об отрицательных запасах: %+v", res.Warnings)
	}
}

func TestTaskStateAt(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	events := []*store.Event{
		ev(1, 1, 101, "buy_task", t0.Add(10*time.Second), true),
		ev(2, 1, 101, "solve", t0.Add(30*time.Second), true),
		ev(3, 1, 102, "buy_task", t0.Add(40*time.Second), false), // отключено
	}
	cases := []struct {
		taskID int64
		at     time.Time
		want   string
	}{
		{101, t0.Add(5 * time.Second), StateHidden},
		{101, t0.Add(15 * time.Second), StateBought},
		{101, t0.Add(35 * time.Second), StatePassed},
		{102, t0.Add(50 * time.Second), StateHidden},
	}
	for _, c := range cases {
		if got := TaskStateAt(events, 1, c.taskID, c.at); got != c.want {
			t.Errorf("TaskStateAt(task %d, %v) = %s, ожидалось %s", c.taskID, c.at, got, c.want)
		}
	}
}
