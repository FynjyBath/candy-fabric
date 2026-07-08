// Package game — детерминированный расчёт состояния игры из журнала событий
// (раздел 5.3 ТЗ). Состояние нигде не хранится: оно всегда функция
// конфигурации игры и списка включённых событий.
package game

import (
	"fmt"
	"math"
	"sort"
	"time"

	"candyfactory/internal/store"
)

// Состояния задачи.
const (
	StateHidden = "hidden"
	StateBought = "bought"
	StatePassed = "passed"
)

// TaskState — состояние пары (команда, задача).
type TaskState struct {
	State    string
	Tests    int
	SolvedAt time.Time // время события solve (нулевое, если задача не решена)
}

// TeamState — результат расчёта для одной команды.
type TeamState struct {
	TeamID int64
	Amount int64
	Speed  int64
	Tasks  map[int64]*TaskState // task_id -> состояние
}

// Warning — проблема валидации (не блокирует, подсвечивается в админке).
type Warning struct {
	EventID int64
	Text    string
}

// Result — итог расчёта состояния игры на момент времени.
type Result struct {
	At       time.Time
	Teams    map[int64]*TeamState
	Warnings []Warning
}

// Compute считает состояние игры на момент now по алгоритму 5.3:
// t0 = start_at, t_end = t0 + Duration, T = min(now, t_end); включённые события
// с at <= T применяются в порядке (at, id), между событиями начисление
// amount += speed за каждую целую секунду (эквивалентно циклу по секундам,
// но считается умножением).
func Compute(g *store.Game, levels []store.Level, tasks []store.Task, teams []store.Team, events []*store.Event, now time.Time) *Result {
	res := &Result{At: now, Teams: map[int64]*TeamState{}}

	levelByNum := map[int]store.Level{}
	for _, l := range levels {
		levelByNum[l.Level] = l
	}
	taskByID := map[int64]store.Task{}
	for _, t := range tasks {
		taskByID[t.ID] = t
	}
	for _, tm := range teams {
		ts := &TeamState{TeamID: tm.ID, Amount: g.StartAmount, Speed: g.StartSpeed, Tasks: map[int64]*TaskState{}}
		for _, t := range tasks {
			ts.Tasks[t.ID] = &TaskState{State: StateHidden}
		}
		res.Teams[tm.ID] = ts
	}

	if g.StartAt == nil {
		return res // игра не началась: стартовые значения, событий не применяем
	}
	t0 := *g.StartAt
	tEnd := t0.Add(time.Duration(g.DurationSec) * time.Second)
	T := now
	if T.After(tEnd) {
		T = tEnd
	}
	if T.Before(t0) {
		return res
	}
	// K — число целых секунд начисления.
	K := int64(math.Floor(T.Sub(t0).Seconds()))

	// Включённые события с at <= T, сортировка (at, id); события с at < t0
	// трактуются как произошедшие в t0 — в том числе при сортировке.
	var evs []*store.Event
	for _, e := range events {
		if !e.Enabled {
			continue
		}
		if e.At.After(T) {
			// Событие позже конца игры видно валидатору, хотя в расчёт не входит.
			if e.At.After(tEnd) {
				res.Warnings = append(res.Warnings, Warning{e.ID, "событие позже конца игры — не участвует в расчёте"})
			}
			continue
		}
		evs = append(evs, e)
	}
	clampT0 := func(t time.Time) time.Time {
		if t.Before(t0) {
			return t0
		}
		return t
	}
	sort.SliceStable(evs, func(i, j int) bool {
		ti, tj := clampT0(evs[i].At), clampT0(evs[j].At)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return evs[i].ID < evs[j].ID
	})

	// accrued[team] — сколько секунд уже начислено команде.
	accrued := map[int64]int64{}
	accrueTo := func(ts *TeamState, upto int64) {
		if upto > K {
			upto = K
		}
		if upto > accrued[ts.TeamID] {
			ts.Amount += ts.Speed * (upto - accrued[ts.TeamID])
			accrued[ts.TeamID] = upto
		}
	}

	for _, e := range evs {
		ts := res.Teams[e.TeamID]
		if ts == nil {
			continue
		}
		// Событие применяется перед начислением за секунду kApply,
		// где kApply — минимальное целое k >= 1 с at <= t0 + k.
		d := e.At.Sub(t0).Seconds()
		kApply := int64(math.Ceil(d))
		if kApply < 1 {
			kApply = 1
		}
		accrueTo(ts, kApply-1)
		applyEvent(res, ts, e, taskByID, levelByNum, t0, tEnd)
		if ts.Amount < 0 {
			res.Warnings = append(res.Warnings, Warning{e.ID, "после события запасы стали отрицательными"})
		}
		if ts.Speed < 0 {
			res.Warnings = append(res.Warnings, Warning{e.ID, "после события скорость стала отрицательной"})
		}
	}
	for _, ts := range res.Teams {
		accrueTo(ts, K)
	}
	return res
}

func applyEvent(res *Result, ts *TeamState, e *store.Event, taskByID map[int64]store.Task, levels map[int]store.Level, t0, tEnd time.Time) {
	warn := func(text string) {
		res.Warnings = append(res.Warnings, Warning{e.ID, text})
	}
	if e.At.Before(t0) {
		warn("событие раньше начала игры (трактуется как t0)")
	}
	if e.TaskID == nil {
		warn("событие без задачи — эффект не применён")
		return
	}
	task, ok := taskByID[*e.TaskID]
	if !ok {
		warn("задача события не найдена — эффект не применён")
		return
	}
	lvl, ok := levels[task.Level]
	if !ok {
		warn("уровень задачи не найден — эффект не применён")
		return
	}
	st := ts.Tasks[task.ID]
	switch e.Type {
	case "buy_task":
		if st.State != StateHidden {
			warn(fmt.Sprintf("buy_task по задаче в состоянии %s (ожидалось hidden)", st.State))
		}
		ts.Amount -= lvl.TaskCost
		ts.Speed -= lvl.Load
		st.State = StateBought
	case "buy_test":
		if st.State != StateBought {
			warn(fmt.Sprintf("buy_test по задаче в состоянии %s (ожидалось bought)", st.State))
		}
		ts.Amount -= lvl.TestCost
		st.Tests++
	case "solve":
		if st.State != StateBought {
			warn(fmt.Sprintf("solve по задаче в состоянии %s (ожидалось bought)", st.State))
		}
		ts.Amount += lvl.AmountBonus
		ts.Speed += lvl.SpeedBonus + lvl.Load
		st.State = StatePassed
		st.SolvedAt = e.At
	default:
		warn("неизвестный тип события " + e.Type)
	}
}

// TaskStateAt возвращает состояние конкретной пары (команда, задача) на момент
// времени at — используется опросчиком для матчинга посылок.
func TaskStateAt(events []*store.Event, teamID, taskID int64, at time.Time) string {
	state := StateHidden
	var relevant []*store.Event
	for _, e := range events {
		if !e.Enabled || e.TeamID != teamID || e.TaskID == nil || *e.TaskID != taskID {
			continue
		}
		if e.At.After(at) {
			continue
		}
		relevant = append(relevant, e)
	}
	sort.SliceStable(relevant, func(i, j int) bool {
		if !relevant[i].At.Equal(relevant[j].At) {
			return relevant[i].At.Before(relevant[j].At)
		}
		return relevant[i].ID < relevant[j].ID
	})
	for _, e := range relevant {
		switch e.Type {
		case "buy_task":
			state = StateBought
		case "solve":
			state = StatePassed
		}
	}
	return state
}
