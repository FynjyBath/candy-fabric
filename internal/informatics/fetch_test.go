package informatics

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// fakeSource — страницы посылок, новые сверху.
type fakeSource struct {
	pages map[int][][]Run // userID -> страницы
	calls int
}

func (f *fakeSource) FetchRunsPage(userID, page int) ([]Run, int, error) {
	f.calls++
	pp := f.pages[userID]
	if page > len(pp) {
		return nil, len(pp), nil
	}
	return pp[page-1], len(pp), nil
}

func run(id int64) Run { return Run{ID: id, EjudgeStatus: 0, ProblemID: 1} }

func TestFetchNewRunsFirstPollFetchesAllPages(t *testing.T) {
	cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
	src := &fakeSource{pages: map[int][][]Run{
		7: {{run(50), run(40)}, {run(30), run(20)}, {run(10)}},
	}}
	runs, newMax, err := FetchNewRuns(src, cache, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 5 {
		t.Errorf("первый опрос: %d посылок, ожидалось 5 (все страницы)", len(runs))
	}
	if err := CommitMaxRunID(cache, 7, newMax); err != nil {
		t.Fatal(err)
	}
	if got, _ := cache.MaxRunID(7); got != 50 {
		t.Errorf("max_run_id = %d, ожидалось 50", got)
	}
	if src.calls != 3 {
		t.Errorf("вызовов страниц %d, ожидалось 3", src.calls)
	}
}

func TestFetchNewRunsStopsAtOldRun(t *testing.T) {
	cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
	cache.SetMaxRunID(7, 30)
	src := &fakeSource{pages: map[int][][]Run{
		7: {{run(50), run(40), run(30), run(20)}, {run(10)}},
	}}
	runs, newMax, err := FetchNewRuns(src, cache, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 50 || runs[1].ID != 40 {
		t.Errorf("ожидались посылки 50 и 40, получено %+v", runs)
	}
	if src.calls != 1 {
		t.Errorf("вызовов страниц %d — при встрече старой посылки надо остановиться на 1-й", src.calls)
	}
	// Водяной знак двигается только явным коммитом (после успешной обработки).
	if got, _ := cache.MaxRunID(7); got != 30 {
		t.Errorf("до коммита max_run_id = %d, ожидалось 30", got)
	}
	if err := CommitMaxRunID(cache, 7, newMax); err != nil {
		t.Fatal(err)
	}
	if got, _ := cache.MaxRunID(7); got != 50 {
		t.Errorf("max_run_id = %d, ожидалось 50", got)
	}
}

func TestFetchNewRunsContinuesPagesWhenNoOld(t *testing.T) {
	cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
	cache.SetMaxRunID(7, 5)
	src := &fakeSource{pages: map[int][][]Run{
		7: {{run(50), run(40)}, {run(30), run(5)}},
	}}
	runs, _, err := FetchNewRuns(src, cache, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Errorf("ожидалось 3 новых посылки, получено %d", len(runs))
	}
	if src.calls != 2 {
		t.Errorf("вызовов страниц %d, ожидалось 2", src.calls)
	}
}

func TestFetchNewRunsNoNew(t *testing.T) {
	cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
	cache.SetMaxRunID(7, 50)
	src := &fakeSource{pages: map[int][][]Run{
		7: {{run(50), run(40)}},
	}}
	runs, newMax, err := FetchNewRuns(src, cache, 7)
	if err != nil {
		t.Fatal(err)
	}
	if newMax != 50 {
		t.Errorf("newMax = %d, ожидалось 50", newMax)
	}
	if len(runs) != 0 {
		t.Errorf("новых посылок быть не должно, получено %d", len(runs))
	}
}

func TestCachePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	c1 := OpenCache(path)
	if err := c1.SetMaxRunID(285757, 1234567); err != nil {
		t.Fatal(err)
	}
	c2 := OpenCache(path)
	if got, ok := c2.MaxRunID(285757); !ok || got != 1234567 {
		t.Errorf("после перечтения кеша max_run_id = %d (ok=%v), ожидалось 1234567", got, ok)
	}
	// битый файл — пустой кеш, не ошибка
	c3 := OpenCache(filepath.Join(t.TempDir(), "missing.json"))
	if _, ok := c3.MaxRunID(1); ok {
		t.Errorf("пустой кеш не должен знать аккаунтов")
	}
}

func TestParseCreateTime(t *testing.T) {
	mskT := time.Date(2026, 7, 3, 12, 30, 0, 0, msk).UTC()
	cases := []struct {
		raw  string
		want time.Time
		ok   bool
	}{
		{`"2026-07-03 12:30:00"`, mskT, true},
		{`"2026-07-03T12:30:00"`, mskT, true},
		{`"2026-07-03T09:30:00Z"`, time.Date(2026, 7, 3, 9, 30, 0, 0, time.UTC), true},
		{`1767436200`, time.Unix(1767436200, 0).UTC(), true},
		{`"1767436200"`, time.Unix(1767436200, 0).UTC(), true},
		{`null`, time.Time{}, false},
		{`""`, time.Time{}, false},
		{`"мусор"`, time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := ParseCreateTime(json.RawMessage(c.raw))
		if ok != c.ok {
			t.Errorf("ParseCreateTime(%s): ok=%v, ожидалось %v", c.raw, ok, c.ok)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("ParseCreateTime(%s) = %v, ожидалось %v", c.raw, got, c.want)
		}
	}
}

func TestFlexInt(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		ok   bool
	}{
		{`0`, 0, true}, {`8`, 8, true}, {`"100"`, 100, true},
		{`""`, 0, false}, {`null`, 0, false}, {`"abc"`, 0, false},
	}
	for _, c := range cases {
		var f flexInt
		if err := json.Unmarshal([]byte(c.raw), &f); err != nil {
			t.Errorf("flexInt(%s): ошибка %v", c.raw, err)
			continue
		}
		if f.OK != c.ok || (c.ok && f.Value != c.want) {
			t.Errorf("flexInt(%s) = %+v, ожидалось %d/%v", c.raw, f, c.want, c.ok)
		}
	}
}

// Посылка без вердикта удерживает водяной знак: её поздний OK будет выкачан.
func TestFetchNewRunsHoldsBackPendingRun(t *testing.T) {
	cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
	cache.SetMaxRunID(7, 10)
	now := time.Now().UTC()
	pending := Run{ID: 40, EjudgeStatus: -1, ProblemID: 1, CreateTime: now}
	solved := Run{ID: 50, EjudgeStatus: 0, ProblemID: 1, CreateTime: now}
	src := &fakeSource{pages: map[int][][]Run{7: {{solved, pending}}}}
	runs, newMax, err := FetchNewRuns(src, cache, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Errorf("посылок %d, ожидалось 2", len(runs))
	}
	if newMax != 39 {
		t.Errorf("newMax = %d — водяной знак должен остановиться перед посылкой без вердикта (39)", newMax)
	}
	// Старая посылка без вердикта водяной знак не держит.
	old := Run{ID: 60, EjudgeStatus: -1, ProblemID: 1, CreateTime: now.Add(-time.Hour)}
	src = &fakeSource{pages: map[int][][]Run{7: {{old}}}}
	_, newMax, err = FetchNewRuns(src, cache, 7)
	if err != nil {
		t.Fatal(err)
	}
	if newMax != 60 {
		t.Errorf("newMax = %d, ожидалось 60 (старый «без вердикта» не удерживает)", newMax)
	}
}

// Промежуточные вердикты (тестируется / компилируется / ждёт проверки) тоже
// удерживают водяной знак — иначе их поздний OK не будет выкачан.
func TestFetchNewRunsHoldsBackTransientVerdicts(t *testing.T) {
	now := time.Now().UTC()
	for _, status := range []int{
		statusRunning, statusCompiling, statusCompiled, statusAvailable,
		statusPending, statusPendingReview, statusSummoned,
	} {
		cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
		cache.SetMaxRunID(7, 10)
		pending := Run{ID: 40, EjudgeStatus: status, ProblemID: 1, CreateTime: now}
		src := &fakeSource{pages: map[int][][]Run{7: {{pending}}}}
		_, newMax, err := FetchNewRuns(src, cache, 7)
		if err != nil {
			t.Fatal(err)
		}
		if newMax != 39 {
			t.Errorf("статус %d: newMax = %d, ожидалось 39 (промежуточный вердикт удерживает знак)", status, newMax)
		}
	}
	// Терминальные вердикты (WA/CE/…) знак не удерживают.
	for _, status := range []int{0, 1, 2, 3, 5, 8, 17} {
		cache := OpenCache(filepath.Join(t.TempDir(), "state.json"))
		cache.SetMaxRunID(7, 10)
		r := Run{ID: 40, EjudgeStatus: status, ProblemID: 1, CreateTime: now}
		src := &fakeSource{pages: map[int][][]Run{7: {{r}}}}
		_, newMax, err := FetchNewRuns(src, cache, 7)
		if err != nil {
			t.Fatal(err)
		}
		if newMax != 40 {
			t.Errorf("терминальный статус %d: newMax = %d, ожидалось 40 (не удерживает)", status, newMax)
		}
	}
}

func TestRunPending(t *testing.T) {
	pending := []int{-1, 11, 16, 23, 96, 97, 98, 99}
	terminal := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 13, 14, 15, 17, 18}
	for _, s := range pending {
		if !(&Run{EjudgeStatus: s}).Pending() {
			t.Errorf("статус %d должен быть промежуточным", s)
		}
	}
	for _, s := range terminal {
		if (&Run{EjudgeStatus: s}).Pending() {
			t.Errorf("статус %d должен быть терминальным", s)
		}
	}
}

func TestMain(m *testing.M) {
	pageDelay = time.Millisecond // не замедлять тесты постраничной паузой
	m.Run()
}
