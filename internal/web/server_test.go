package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"candyfactory/internal/store"
)

// testServer поднимает приложение с временным каталогом данных.
func testServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	credsPath := filepath.Join(dir, "admin_credentials.json")
	os.WriteFile(credsPath, []byte(`{"login":"admin","password":"secret"}`), 0o600)
	secret := make([]byte, 32)
	srv, err := NewServer(Config{
		Store: st, Logger: log.New(io.Discard, "", 0), Secret: secret,
		AdminCredsPath: credsPath, ThemePath: filepath.Join(dir, "theme.txt"),
		PageRefresh: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, dir
}

// adminClient — HTTP-клиент с cookie-сессией админа и CSRF-токеном.
func adminClient(t *testing.T, ts *httptest.Server) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"login": {"admin"}, "password": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin/games" {
		t.Fatalf("логин админа не удался: %s / %s", resp.Request.URL, body)
	}
	// CSRF-токен со страницы списка игр.
	s := string(body)
	i := strings.Index(s, `name="csrf" value="`)
	if i < 0 {
		t.Fatal("CSRF-токен не найден на странице")
	}
	s = s[i+len(`name="csrf" value="`):]
	return c, s[:strings.Index(s, `"`)]
}

// createGame создаёт игру n=3 с 9 задачами и 3 командами через форму админки.
func createGame(t *testing.T, ts *httptest.Server, c *http.Client, csrf string) int64 {
	t.Helper()
	form := url.Values{
		"csrf": {csrf}, "title": {"Тестовый турнир"}, "n": {"3"},
		"start_amount": {"20000"}, "start_speed": {"15"}, "duration_min": {"85"},
		// зеркало mccme и #1 на конце принимаются и нормализуются
		"tasks_1":       {"https://informatics.msk.ru/mod/statements/view.php?chapterid=101\nhttps://informatics.mccme.ru/mod/statements/view.php?chapterid=102#1\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=103"},
		"tasks_2":       {"https://informatics.msk.ru/mod/statements/view.php?chapterid=201\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=202\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=203"},
		"tasks_3":       {"https://informatics.msk.ru/mod/statements/view.php?chapterid=301\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=302\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=303"},
		"team_name":     {"Альфа", "Бета", "Гамма"},
		"team_user_id":  {"1001", "1002", "1003"},
		"team_login":    {"alpha", "beta", "gamma"},
		"team_password": {"pa", "pb", "pc"},
	}
	for lvl := 1; lvl <= 3; lvl++ {
		form.Set(fmt.Sprintf("task_cost_%d", lvl), "12000")
		form.Set(fmt.Sprintf("test_cost_%d", lvl), []string{"3000", "7000", "10000"}[lvl-1])
		form.Set(fmt.Sprintf("load_%d", lvl), "2")
		form.Set(fmt.Sprintf("amount_bonus_%d", lvl), []string{"12000", "25000", "50000"}[lvl-1])
		form.Set(fmt.Sprintf("speed_bonus_%d", lvl), []string{"4", "7", "11"}[lvl-1])
	}
	resp, err := c.PostForm(ts.URL+"/admin/games/new", form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Request.URL.Path, "/admin/g/") {
		t.Fatalf("игра не создана, страница: %s\n%s", resp.Request.URL.Path, body)
	}
	var id int64
	fmt.Sscanf(strings.TrimPrefix(resp.Request.URL.Path, "/admin/g/"), "%d", &id)
	return id
}

func startGame(t *testing.T, ts *httptest.Server, c *http.Client, csrf string, gameID int64) {
	t.Helper()
	resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/start", ts.URL, gameID), url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("старт игры: HTTP %d", resp.StatusCode)
	}
}

func getJSON(t *testing.T, c *http.Client, url string, v any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// Критерии 1–2: вход в админку (неправильный пароль — отказ), создание игры,
// ошибочная ссылка и дубль — внятные ошибки формы.
func TestAdminLoginAndGameCreation(t *testing.T) {
	ts, _, _ := testServer(t)

	// Неправильный пароль — отказ.
	jar, _ := cookiejar.New(nil)
	bad := &http.Client{Jar: jar}
	resp, _ := bad.PostForm(ts.URL+"/admin/login", url.Values{"login": {"admin"}, "password": {"wrong"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Неверные логин или пароль") {
		t.Errorf("неправильный пароль должен давать отказ")
	}
	// Мутация без сессии — 403.
	resp, _ = http.PostForm(ts.URL+"/admin/g/1/start", url.Values{})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("мутация без сессии: HTTP %d, ожидался 403", resp.StatusCode)
	}

	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	if gameID == 0 {
		t.Fatal("нет id созданной игры")
	}

	// Ошибочная ссылка и дубль chapterid — ошибки формы с указанием строки.
	form := url.Values{
		"csrf": {csrf}, "title": {"X"}, "n": {"2"},
		"start_amount": {"1"}, "start_speed": {"1"}, "duration_min": {"10"},
		"tasks_1":       {"https://example.com/task?chapterid=1\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=5"},
		"tasks_2":       {"https://informatics.msk.ru/mod/statements/view.php?chapterid=5\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=6"},
		"team_name":     {"А", "Б"},
		"team_user_id":  {"1", "2"},
		"team_login":    {"a", "b"},
		"team_password": {"1", "2"},
	}
	for lvl := 1; lvl <= 2; lvl++ {
		for _, f := range []string{"task_cost", "test_cost", "load", "amount_bonus", "speed_bonus"} {
			form.Set(fmt.Sprintf("%s_%d", f, lvl), "1")
		}
	}
	resp, _ = c.PostForm(ts.URL+"/admin/games/new", form)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(body)
	if !strings.Contains(s, "Уровень 1, строка 1") || !strings.Contains(s, "недопустимый хост") {
		t.Errorf("нет внятной ошибки о недопустимой ссылке:\n%s", s[:min(len(s), 2000)])
	}
	if !strings.Contains(s, "дубликат задачи chapterid=5") {
		t.Errorf("нет ошибки о дубликате chapterid")
	}
}

// Критерий 3: после старта таблицы двух команд содержат одинаковые множества
// задач в каждой строке, но (как правило) в разном порядке; порядок стабилен.
func TestShuffleStableAndPerTeam(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	var adminState struct {
		State struct {
			Teams []struct {
				ID    int64 `json:"id"`
				Cells []struct {
					Cell      int `json:"cell"`
					ChapterID int `json:"chapter_id"`
				} `json:"cells"`
			} `json:"teams"`
		} `json:"state"`
	}
	getJSON(t, c, fmt.Sprintf("%s/admin/api/g/%d/state", ts.URL, gameID), &adminState)
	teams := adminState.State.Teams
	if len(teams) != 3 {
		t.Fatalf("команд %d, ожидалось 3", len(teams))
	}
	// Множества задач в каждой строке одинаковые.
	rowSet := func(ti, row int) map[int]bool {
		set := map[int]bool{}
		for c := row * 3; c < row*3+3; c++ {
			set[teams[ti].Cells[c].ChapterID] = true
		}
		return set
	}
	for row := 0; row < 3; row++ {
		base := rowSet(0, row)
		for ti := 1; ti < 3; ti++ {
			other := rowSet(ti, row)
			for k := range base {
				if !other[k] {
					t.Errorf("строка %d: у команд разные множества задач", row+1)
				}
			}
		}
	}
	// Порядок стабилен после «рестарта» (повторного чтения из БД).
	teamsDB, _ := st.GetTeams(gameID)
	before, _ := st.GetTaskOrder(teamsDB[0].ID)
	if err := st.EnsureTaskOrder(gameID); err != nil { // идемпотентный повторный вызов
		t.Fatal(err)
	}
	after, _ := st.GetTaskOrder(teamsDB[0].ID)
	for cell, taskID := range before {
		if after[cell] != taskID {
			t.Errorf("перестановка изменилась после повторного EnsureTaskOrder")
		}
	}
}

// Критерий 4: публичный API — без ссылок вообще; командный — с ссылками.
func TestPublicStateHasNoLinks(t *testing.T) {
	ts, _, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	resp, _ := http.Get(fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID))
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Поле mode легитимно содержит слово "informatics" — проверяем именно
	// следы ссылок и идентификаторов задач.
	if strings.Contains(string(raw), "informatics.msk") || strings.Contains(string(raw), "chapterid") ||
		strings.Contains(string(raw), `"url"`) || strings.Contains(string(raw), "statements") {
		t.Errorf("публичный JSON содержит следы ссылок:\n%s", raw)
	}
	// Публичная HTML-страница тоже без ссылок на задачи.
	resp, _ = http.Get(fmt.Sprintf("%s/g/%d", ts.URL, gameID))
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(page), "statements/view.php") {
		t.Errorf("публичная страница содержит ссылки на задачи")
	}

	// Командный вход и командный API со ссылками.
	jar, _ := cookiejar.New(nil)
	tc := &http.Client{Jar: jar}
	resp, err := tc.PostForm(fmt.Sprintf("%s/g/%d/team", ts.URL, gameID),
		url.Values{"login": {"alpha"}, "password": {"pa"}})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.Path, "/team/") {
		t.Fatalf("вход команды не удался: %s", resp.Request.URL.Path)
	}
	teamURL := resp.Request.URL.Path // /g/{id}/team/{teamId}
	var teamState struct {
		Teams []struct {
			ID    int64 `json:"id"`
			Cells []struct {
				URL string `json:"url"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, tc, ts.URL+"/api"+teamURL+"/state", &teamState)
	withURL := 0
	for _, tm := range teamState.Teams {
		for _, cell := range tm.Cells {
			if cell.URL != "" {
				withURL++
			}
		}
	}
	// Ссылка появляется только после покупки задачи: до покупок их нет ни у кого.
	if withURL != 0 {
		t.Errorf("до покупок в командном API не должно быть ссылок, найдено %d", withURL)
	}
	// Чужая командная сессия — 403.
	parts := strings.Split(strings.Trim(teamURL, "/"), "/")
	ownID := parts[len(parts)-1]
	otherID := "999"
	for _, tm := range teamState.Teams {
		if fmt.Sprint(tm.ID) != ownID {
			otherID = fmt.Sprint(tm.ID)
			break
		}
	}
	resp, _ = tc.Get(fmt.Sprintf("%s/g/%d/team/%s", ts.URL, gameID, otherID))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("чужая страница команды: HTTP %d, ожидался 403", resp.StatusCode)
	}
}

// Критерий 5: покупка через админ-табло; предупреждение при нехватке средств,
// сохранение только после подтверждения.
func TestPurchaseAndWarning(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	teams, _ := st.GetTeams(gameID)
	tasks, _ := st.GetTasks(gameID)
	var lvl1 store.Task
	for _, task := range tasks {
		if task.Level == 1 {
			lvl1 = task
			break
		}
	}
	// Покупка задачи уровня 1: −12000 запасов, −2 скорости, ячейка жёлтая.
	postEvent := func(fields url.Values) map[string]any {
		fields.Set("csrf", csrf)
		resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/event", ts.URL, gameID), fields)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	out := postEvent(url.Values{
		"team_id": {fmt.Sprint(teams[0].ID)}, "task_id": {fmt.Sprint(lvl1.ID)}, "type": {"buy_task"},
	})
	if out["ok"] != true {
		t.Fatalf("покупка не сохранена: %v", out)
	}
	var pub struct {
		Teams []struct {
			ID     int64 `json:"id"`
			Amount int64 `json:"amount"`
			Speed  int64 `json:"speed"`
			Cells  []struct {
				State string `json:"state"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	team0 := pub.Teams[0]
	if team0.Speed != 13 {
		t.Errorf("скорость после покупки %d, ожидалось 13", team0.Speed)
	}
	if team0.Amount > 20000-12000+15*10 || team0.Amount < 20000-12000 {
		t.Errorf("запасы после покупки %d — вне ожидаемого диапазона", team0.Amount)
	}
	bought := 0
	for _, cell := range team0.Cells {
		if cell.State == "bought" {
			bought++
		}
	}
	if bought != 1 {
		t.Errorf("жёлтых ячеек %d, ожидалась 1", bought)
	}
	// Ячейка «куплена» есть только у купившей команды.
	for _, tm := range pub.Teams[1:] {
		for _, cell := range tm.Cells {
			if cell.State != "hidden" {
				t.Errorf("у другой команды ячейка не hidden")
			}
		}
	}

	// Скупаем задачи до нехватки средств: предупреждение, потом confirmed=1.
	var warned bool
	for _, task := range tasks {
		if task.ID == lvl1.ID {
			continue
		}
		out := postEvent(url.Values{
			"team_id": {fmt.Sprint(teams[0].ID)}, "task_id": {fmt.Sprint(task.ID)}, "type": {"buy_task"},
		})
		if w, ok := out["warning"].(string); ok {
			if w != "Недостаточно средств" && w != "Недостаточно производительности" {
				t.Errorf("неожиданный текст предупреждения: %q", w)
			}
			warned = true
			// Без подтверждения событие не сохранилось.
			events, _ := st.GetEvents(gameID)
			n := len(events)
			out2 := postEvent(url.Values{
				"team_id": {fmt.Sprint(teams[0].ID)}, "task_id": {fmt.Sprint(task.ID)},
				"type": {"buy_task"}, "confirmed": {"1"},
			})
			if out2["ok"] != true {
				t.Errorf("подтверждённая покупка не сохранена: %v", out2)
			}
			events, _ = st.GetEvents(gameID)
			if len(events) != n+1 {
				t.Errorf("после подтверждения должно стать %d событий, стало %d", n+1, len(events))
			}
			break
		}
	}
	if !warned {
		t.Errorf("предупреждение о нехватке средств так и не появилось")
	}
}

// Критерий 7: смещение покупки на 10 минут назад увеличивает запасы на
// 10*60*Load; удалённое событие исчезает; отключение/включение работает.
func TestJournalEditing(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)

	// Стартуем игру «в прошлом», чтобы было место для сдвига назад.
	past := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	if err := st.SetGameStartAt(gameID, &past); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureTaskOrder(gameID); err != nil {
		t.Fatal(err)
	}
	teams, _ := st.GetTeams(gameID)
	tasks, _ := st.GetTasks(gameID)
	var lvl1 store.Task
	for _, task := range tasks {
		if task.Level == 1 {
			lvl1 = task
			break
		}
	}
	buyAt := past.Add(20 * time.Minute)
	taskID := lvl1.ID
	eventID, err := st.AddEvent(&store.Event{
		GameID: gameID, TeamID: teams[0].ID, TaskID: &taskID,
		Type: "buy_task", At: buyAt, Source: "manual", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	amountOf := func() int64 {
		var pub struct {
			Teams []struct {
				Amount int64 `json:"amount"`
			} `json:"teams"`
		}
		getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
		return pub.Teams[0].Amount
	}
	before := amountOf()

	// Сдвигаем покупку на 10 минут назад через админку.
	newAt := buyAt.Add(-10 * time.Minute)
	resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/event/%d/update", ts.URL, gameID, eventID), url.Values{
		"csrf":    {csrf},
		"team_id": {fmt.Sprint(teams[0].ID)},
		"task_id": {fmt.Sprint(lvl1.ID)},
		"type":    {"buy_task"},
		"at":      {newAt.Local().Format("2006-01-02T15:04:05")},
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	after := amountOf()
	// Критерий приёмки 7: смещение покупки меняет текущие запасы ровно на
	// 10*60*Load_r — недоначисленная скорость. Более ранняя покупка означает,
	// что скорость снижена на Load дольше, поэтому запасы меньше на 1200.
	// Допуск в 2 секунды начисления (скорость 13) — между двумя запросами
	// состояния могла пройти секунда игрового времени.
	wantDiff := int64(-10 * 60 * 2) // 10 минут * Load 2
	diff := after - before
	if diff < wantDiff-2*13 || diff > wantDiff+2*13 {
		t.Errorf("после сдвига покупки на 10 минут назад запасы изменились на %d, ожидалось ~%d", diff, wantDiff)
	}

	// Отключение события возвращает состояние hidden.
	resp, _ = c.PostForm(fmt.Sprintf("%s/admin/g/%d/event/%d/toggle", ts.URL, gameID, eventID), url.Values{"csrf": {csrf}})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var pub struct {
		Teams []struct {
			Speed int64 `json:"speed"`
		} `json:"teams"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	if pub.Teams[0].Speed != 15 {
		t.Errorf("после отключения покупки скорость %d, ожидалось 15", pub.Teams[0].Speed)
	}

	// Удаление ручного события.
	resp, _ = c.PostForm(fmt.Sprintf("%s/admin/g/%d/event/%d/delete", ts.URL, gameID, eventID), url.Values{"csrf": {csrf}})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	events, _ := st.GetEvents(gameID)
	if len(events) != 0 {
		t.Errorf("событие не удалилось: %d записей", len(events))
	}
}

// Финиш (критерий 10): по истечении времени начисление останавливается,
// статус finished.
func TestFinishStopsAccrual(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	past := time.Now().UTC().Add(-2 * time.Hour)
	st.SetGameStartAt(gameID, &past)

	var pub struct {
		Status string `json:"status"`
		Teams  []struct {
			Amount int64 `json:"amount"`
		} `json:"teams"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	if pub.Status != "finished" {
		t.Errorf("статус %s, ожидался finished", pub.Status)
	}
	want := int64(20000 + 15*5100)
	if pub.Teams[0].Amount != want {
		t.Errorf("запасы после финиша %d, ожидалось %d", pub.Teams[0].Amount, want)
	}
	_ = csrf
}

// teamClient логинит команду и возвращает клиент, URL страницы и CSRF-токен.
func teamClient(t *testing.T, ts *httptest.Server, gameID int64, login, password string) (*http.Client, string, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(fmt.Sprintf("%s/g/%d/team", ts.URL, gameID),
		url.Values{"login": {login}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.Path, "/team/") {
		t.Fatalf("вход команды не удался: %s", resp.Request.URL.Path)
	}
	s := string(body)
	i := strings.Index(s, `csrf: "`)
	if i < 0 {
		t.Fatalf("CSRF-токен не найден на странице команды")
	}
	s = s[i+len(`csrf: "`):]
	return c, resp.Request.URL.Path, s[:strings.Index(s, `"`)]
}

// Самостоятельная покупка задачи командой: успех, появление ссылки, запрет
// повторной покупки, блокировка при нехватке средств, запрет вне running.
func TestTeamSelfBuy(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	tc, teamPath, teamCSRF := teamClient(t, ts, gameID, "alpha", "pa")
	buyURL := ts.URL + "/api" + teamPath + "/buy"
	buy := func(cl *http.Client, token string, cell int) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, buyURL,
			strings.NewReader(url.Values{"cell": {fmt.Sprint(cell)}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", token)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Успешная покупка ячейки 1.
	code, out := buy(tc, teamCSRF, 1)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("покупка ячейки 1: HTTP %d %v", code, out)
	}
	var state struct {
		Teams []struct {
			ID     int64 `json:"id"`
			Amount int64 `json:"amount"`
			Speed  int64 `json:"speed"`
			Cells  []struct {
				State string `json:"state"`
				URL   string `json:"url"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, tc, ts.URL+"/api"+teamPath+"/state", &state)
	var own *struct {
		ID     int64 `json:"id"`
		Amount int64 `json:"amount"`
		Speed  int64 `json:"speed"`
		Cells  []struct {
			State string `json:"state"`
			URL   string `json:"url"`
		} `json:"cells"`
	}
	for i := range state.Teams {
		if strings.HasSuffix(teamPath, fmt.Sprint(state.Teams[i].ID)) {
			own = &state.Teams[i]
		}
	}
	if own == nil {
		t.Fatal("своя команда не найдена в состоянии")
	}
	if own.Speed != 13 {
		t.Errorf("скорость после покупки %d, ожидалось 13", own.Speed)
	}
	if own.Cells[0].State != "bought" || own.Cells[0].URL == "" {
		t.Errorf("ячейка 1 после покупки: state=%s url=%q — ожидалось bought со ссылкой", own.Cells[0].State, own.Cells[0].URL)
	}

	// Повторная покупка той же ячейки — отказ.
	code, out = buy(tc, teamCSRF, 1)
	if code != http.StatusConflict {
		t.Errorf("повторная покупка: HTTP %d %v, ожидался 409", code, out)
	}
	// Некорректная ячейка.
	if code, _ := buy(tc, teamCSRF, 99); code != http.StatusBadRequest {
		t.Errorf("покупка ячейки 99: HTTP %d, ожидался 400", code)
	}
	// Без CSRF-токена — отказ.
	if code, _ := buy(tc, "", 2); code != http.StatusForbidden {
		t.Errorf("покупка без CSRF: HTTP %d, ожидался 403", code)
	}
	// Без сессии — 401.
	anon := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, buyURL, strings.NewReader("cell=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ := anon.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("покупка без сессии: HTTP %d, ожидался 401", resp.StatusCode)
	}

	// Скупаем до нехватки средств: в отличие от админа, команде — отказ.
	sawInsufficient := false
	for cell := 2; cell <= 9; cell++ {
		code, out := buy(tc, teamCSRF, cell)
		if code == http.StatusConflict {
			if msg, _ := out["error"].(string); strings.Contains(msg, "Недостаточно") {
				sawInsufficient = true
				break
			}
		}
	}
	if !sawInsufficient {
		t.Errorf("блокировка «Недостаточно средств» так и не сработала")
	}

	// В не-running игре покупка запрещена.
	g2 := createGame(t, ts, c, csrf) // draft
	tc2, teamPath2, teamCSRF2 := teamClient(t, ts, g2, "alpha", "pa")
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath2+"/buy",
		strings.NewReader("cell=1"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("X-CSRF-Token", teamCSRF2)
	resp2, _ := tc2.Do(req2)
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("покупка в draft-игре: HTTP %d, ожидался 409", resp2.StatusCode)
	}
	_ = st
}

// Продление на произвольное время.
func TestExtendArbitrary(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	extend := func(minutes string) int {
		resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/extend", ts.URL, gameID),
			url.Values{"csrf": {csrf}, "minutes": {minutes}})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := extend("30"); code != http.StatusOK {
		t.Fatalf("продление на 30 минут: HTTP %d", code)
	}
	g, _ := st.GetGame(gameID)
	if g.DurationSec != 85*60+30*60 {
		t.Errorf("duration = %d, ожидалось %d", g.DurationSec, 85*60+30*60)
	}
	if code := extend("-10"); code != http.StatusOK {
		t.Fatalf("сокращение на 10 минут: HTTP %d", code)
	}
	g, _ = st.GetGame(gameID)
	if g.DurationSec != 85*60+20*60 {
		t.Errorf("после сокращения duration = %d, ожидалось %d", g.DurationSec, 85*60+20*60)
	}
	// Нулевое и сверхотрицательное значения — отказ.
	if code := extend("0"); code != http.StatusBadRequest {
		t.Errorf("продление на 0: HTTP %d, ожидался 400", code)
	}
	if code := extend("-100000"); code != http.StatusBadRequest {
		t.Errorf("сокращение ниже минуты: HTTP %d, ожидался 400", code)
	}
}

// editForm собирает форму редактирования из текущей конфигурации игры.
func editForm(t *testing.T, st *store.Store, gameID int64, csrf string) url.Values {
	t.Helper()
	g, err := st.GetGame(gameID)
	if err != nil {
		t.Fatal(err)
	}
	levels, _ := st.GetLevels(gameID)
	tasks, _ := st.GetTasks(gameID)
	teams, _ := st.GetTeams(gameID)
	form := url.Values{
		"csrf": {csrf}, "title": {g.Title}, "n": {fmt.Sprint(g.N)},
		"start_amount": {fmt.Sprint(g.StartAmount)},
		"start_speed":  {fmt.Sprint(g.StartSpeed)},
		"duration_min": {fmt.Sprint(g.DurationSec / 60)},
	}
	for _, l := range levels {
		form.Set(fmt.Sprintf("task_cost_%d", l.Level), fmt.Sprint(l.TaskCost))
		form.Set(fmt.Sprintf("test_cost_%d", l.Level), fmt.Sprint(l.TestCost))
		form.Set(fmt.Sprintf("load_%d", l.Level), fmt.Sprint(l.Load))
		form.Set(fmt.Sprintf("amount_bonus_%d", l.Level), fmt.Sprint(l.AmountBonus))
		form.Set(fmt.Sprintf("speed_bonus_%d", l.Level), fmt.Sprint(l.SpeedBonus))
	}
	if g.Mode == store.ModeManual {
		// Мат-режим: поля условие+ответ на каждую задачу.
		for _, task := range tasks {
			form.Set(fmt.Sprintf("stmt_%d_%d", task.Level, task.Ord), task.Statement)
			form.Set(fmt.Sprintf("ans_%d_%d", task.Level, task.Ord), task.Answer)
		}
	} else {
		rows := map[int][]string{}
		for _, task := range tasks {
			rows[task.Level] = append(rows[task.Level], task.URL)
		}
		for lvl, urls := range rows {
			form.Set(fmt.Sprintf("tasks_%d", lvl), strings.Join(urls, "\n"))
		}
	}
	for _, tm := range teams {
		form.Add("team_id", fmt.Sprint(tm.ID))
		form.Add("team_name", tm.Name)
		form.Add("team_user_id", fmt.Sprint(tm.InformaticsUserID))
		form.Add("team_login", tm.Login)
		form.Add("team_password", tm.Password)
	}
	return form
}

func postEdit(t *testing.T, ts *httptest.Server, c *http.Client, gameID int64, form url.Values) (string, string) {
	t.Helper()
	resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/edit", ts.URL, gameID), form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.Request.URL.Path, string(body)
}

// Редактирование конфигурации не меняет id команд и задач: ссылки на страницы
// команд, сессии и события остаются валидными.
func TestEditPreservesTeamAndTaskIDs(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	teamsBefore, _ := st.GetTeams(gameID)
	tasksBefore, _ := st.GetTasks(gameID)
	orderBefore, _ := st.GetTaskOrders(gameID)

	// Команда логинится до редактирования.
	tc, teamPath, _ := teamClient(t, ts, gameID, "alpha", "pa")

	// Правим: название игры, цену уровня 1, имя и пароль первой команды.
	form := editForm(t, st, gameID, csrf)
	form.Set("title", "Переименованный турнир")
	form.Set("task_cost_1", "9000")
	form["team_name"][0] = "Альфа-Прим"
	form["team_password"][0] = "newpa"
	path, body := postEdit(t, ts, c, gameID, form)
	if !strings.HasPrefix(path, "/admin/g/") || strings.Contains(body, "Ошибки формы") {
		t.Fatalf("редактирование не сохранилось: %s\n%.500s", path, body)
	}

	teamsAfter, _ := st.GetTeams(gameID)
	tasksAfter, _ := st.GetTasks(gameID)
	orderAfter, _ := st.GetTaskOrders(gameID)
	if len(teamsAfter) != len(teamsBefore) {
		t.Fatalf("число команд изменилось: %d -> %d", len(teamsBefore), len(teamsAfter))
	}
	for i := range teamsBefore {
		if teamsAfter[i].ID != teamsBefore[i].ID {
			t.Errorf("id команды %d изменился: %d -> %d", i, teamsBefore[i].ID, teamsAfter[i].ID)
		}
	}
	if teamsAfter[0].Name != "Альфа-Прим" || teamsAfter[0].Password != "newpa" {
		t.Errorf("правки команды не применились: %+v", teamsAfter[0])
	}
	for i := range tasksBefore {
		if tasksAfter[i].ID != tasksBefore[i].ID {
			t.Errorf("id задачи %d изменился: %d -> %d", i, tasksBefore[i].ID, tasksAfter[i].ID)
		}
	}
	// Перестановки не пересоздались.
	for teamID, cells := range orderBefore {
		for cell, taskID := range cells {
			if orderAfter[teamID][cell] != taskID {
				t.Errorf("перестановка команды %d изменилась в ячейке %d", teamID, cell)
			}
		}
	}
	// Старая сессия команды продолжает работать (ссылка не «поехала»).
	resp, _ := tc.Get(ts.URL + teamPath)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("страница команды после редактирования: HTTP %d", resp.StatusCode)
	}
	// Новая цена задач видна в состоянии.
	var pub struct {
		Levels []struct {
			TaskCost int64 `json:"task_cost"`
		} `json:"levels"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	if pub.Levels[0].TaskCost != 9000 {
		t.Errorf("цена уровня 1 = %d, ожидалось 9000", pub.Levels[0].TaskCost)
	}
}

// Редактирование во время идущей игры: события выживают, замена ссылки «на
// месте» сохраняет id задачи, структурные изменения отклоняются.
func TestEditDuringRunning(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	teams, _ := st.GetTeams(gameID)
	tasks, _ := st.GetTasks(gameID)
	// Событие на первую задачу первой команды.
	taskID := tasks[0].ID
	if _, err := st.AddEvent(&store.Event{GameID: gameID, TeamID: teams[0].ID, TaskID: &taskID,
		Type: "buy_task", At: time.Now().UTC(), Source: "manual", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Замена chapterid первой задачи «на месте».
	form := editForm(t, st, gameID, csrf)
	rows := strings.Split(form.Get("tasks_1"), "\n")
	rows[0] = "https://informatics.msk.ru/mod/statements/view.php?chapterid=90101"
	form.Set("tasks_1", strings.Join(rows, "\n"))
	path, body := postEdit(t, ts, c, gameID, form)
	if strings.Contains(body, "Ошибки формы") {
		t.Fatalf("замена ссылки на месте должна проходить: %s\n%.500s", path, body)
	}
	tasksAfter, _ := st.GetTasks(gameID)
	if tasksAfter[0].ID != taskID || tasksAfter[0].ChapterID != 90101 {
		t.Errorf("замена на месте: id %d->%d, chapterid=%d (ожидалось то же id и 90101)",
			taskID, tasksAfter[0].ID, tasksAfter[0].ChapterID)
	}
	events, _ := st.GetEvents(gameID)
	if len(events) != 1 || *events[0].TaskID != taskID {
		t.Errorf("событие потерялось после редактирования: %+v", events)
	}
	// Ячейка осталась купленной.
	var pub struct {
		Teams []struct {
			ID    int64 `json:"id"`
			Cells []struct {
				State string `json:"state"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	bought := 0
	for _, tm := range pub.Teams {
		for _, cell := range tm.Cells {
			if cell.State == "bought" {
				bought++
			}
		}
	}
	if bought != 1 {
		t.Errorf("после редактирования куплено %d ячеек, ожидалась 1", bought)
	}

	// Смена n после старта — ошибка формы.
	form = editForm(t, st, gameID, csrf)
	form.Set("n", "2")
	form.Set("tasks_1", "https://informatics.msk.ru/mod/statements/view.php?chapterid=101\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=102")
	form.Set("tasks_2", "https://informatics.msk.ru/mod/statements/view.php?chapterid=201\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=202")
	form.Del("tasks_3")
	_, body = postEdit(t, ts, c, gameID, form)
	if !strings.Contains(body, "нельзя менять число уровней") {
		t.Errorf("смена n после старта должна отклоняться с внятной ошибкой")
	}

	// Удаление команды с событиями — ошибка.
	form = editForm(t, st, gameID, csrf)
	form["team_id"] = form["team_id"][1:]
	form["team_name"] = form["team_name"][1:]
	form["team_user_id"] = form["team_user_id"][1:]
	form["team_login"] = form["team_login"][1:]
	form["team_password"] = form["team_password"][1:]
	_, body = postEdit(t, ts, c, gameID, form)
	if !strings.Contains(body, "нельзя удалить команду") {
		t.Errorf("удаление команды с событиями должно отклоняться")
	}
	teamsAfter, _ := st.GetTeams(gameID)
	if len(teamsAfter) != 3 {
		t.Errorf("команда всё-таки удалилась: %d", len(teamsAfter))
	}

	// Добавление команды посреди игры: получает перестановку и ячейки.
	form = editForm(t, st, gameID, csrf)
	form.Add("team_id", "")
	form.Add("team_name", "Дельта")
	form.Add("team_user_id", "1004")
	form.Add("team_login", "delta")
	form.Add("team_password", "pd")
	_, body = postEdit(t, ts, c, gameID, form)
	if strings.Contains(body, "Ошибки формы") {
		t.Fatalf("добавление команды не прошло:\n%.500s", body)
	}
	teamsAfter, _ = st.GetTeams(gameID)
	if len(teamsAfter) != 4 {
		t.Fatalf("команд %d, ожидалось 4", len(teamsAfter))
	}
	// Состояние (оно же материализует перестановку новой команды).
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	if len(pub.Teams) != 4 {
		t.Fatalf("в состоянии %d команд, ожидалось 4", len(pub.Teams))
	}
	orders, _ := st.GetTaskOrders(gameID)
	newTeam := teamsAfter[3]
	if len(orders[newTeam.ID]) != 9 {
		t.Errorf("у новой команды %d ячеек перестановки, ожидалось 9", len(orders[newTeam.ID]))
	}
	// Перестановки старых команд не тронуты (стабильность).
	if len(orders[teams[0].ID]) != 9 {
		t.Errorf("перестановка старой команды повреждена")
	}
}

// Кнопка «+5 минут до старта».
func TestDelayStart(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)

	delay := func() int {
		resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/delay-start", ts.URL, gameID),
			url.Values{"csrf": {csrf}})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// Без планового старта: назначается now+5.
	if code := delay(); code != http.StatusOK {
		t.Fatalf("delay-start: HTTP %d", code)
	}
	g, _ := st.GetGame(gameID)
	if g.StartAt == nil {
		t.Fatal("start_at не назначен")
	}
	d := time.Until(*g.StartAt)
	if d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("start_at через %v, ожидалось ~5 минут", d)
	}
	first := *g.StartAt
	// Повторное нажатие: +5 от планового.
	if code := delay(); code != http.StatusOK {
		t.Fatalf("повторный delay-start: HTTP %d", code)
	}
	g, _ = st.GetGame(gameID)
	if got := g.StartAt.Sub(first); got != 5*time.Minute {
		t.Errorf("повторный сдвиг = %v, ожидалось 5 минут", got)
	}
	// Для стартовавшей игры — 409.
	startGame(t, ts, c, csrf, gameID)
	if code := delay(); code != http.StatusConflict {
		t.Errorf("delay-start после старта: HTTP %d, ожидался 409", code)
	}
}

// Смена оформления сайта: применяется ко всем страницам, переживает рестарт,
// мусорные значения отклоняются, без прав админа недоступна.
func TestSiteTheme(t *testing.T) {
	ts, _, dir := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createGame(t, ts, c, csrf)

	pageTheme := func(cl *http.Client, url string) string {
		resp, err := cl.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		i := strings.Index(string(body), `data-theme="`)
		if i < 0 {
			t.Fatalf("на странице %s нет data-theme", url)
		}
		s := string(body)[i+len(`data-theme="`):]
		return s[:strings.Index(s, `"`)]
	}
	// По умолчанию — candy, на публичной странице тоже.
	if th := pageTheme(http.DefaultClient, fmt.Sprintf("%s/g/%d", ts.URL, gameID)); th != "candy" {
		t.Errorf("тема по умолчанию %q, ожидалась candy", th)
	}
	// Переключение.
	resp, err := c.PostForm(ts.URL+"/admin/theme", url.Values{"csrf": {csrf}, "theme": {"hamster"}})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for _, u := range []string{
		fmt.Sprintf("%s/g/%d", ts.URL, gameID), // публичное табло
		ts.URL + "/",                           // главная
	} {
		if th := pageTheme(http.DefaultClient, u); th != "hamster" {
			t.Errorf("после переключения %s имеет тему %q", u, th)
		}
	}
	// Файл темы записан (переживёт рестарт).
	b, err := os.ReadFile(filepath.Join(dir, "theme.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "hamster" {
		t.Errorf("файл темы: %q, %v", b, err)
	}
	// Мусорное значение — 400, тема не меняется.
	resp, _ = c.PostForm(ts.URL+"/admin/theme", url.Values{"csrf": {csrf}, "theme": {"disco"}})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("мусорная тема: HTTP %d, ожидался 400", resp.StatusCode)
	}
	// Без сессии — 403.
	resp, _ = http.PostForm(ts.URL+"/admin/theme", url.Values{"theme": {"neuro"}})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("смена темы без админа: HTTP %d, ожидался 403", resp.StatusCode)
	}
	if th := pageTheme(http.DefaultClient, ts.URL+"/"); th != "hamster" {
		t.Errorf("тема сбилась после неудачных попыток: %q", th)
	}
}

// createManualGame создаёт игру в ручном (математическом) режиме: без ссылок
// и informatics user_id.
func createManualGame(t *testing.T, ts *httptest.Server, c *http.Client, csrf string) int64 {
	t.Helper()
	form := url.Values{
		"csrf": {csrf}, "title": {"Матбой"}, "mode": {"manual"}, "n": {"2"},
		"start_amount": {"20000"}, "start_speed": {"15"}, "duration_min": {"85"},
		"team_id":       {"", ""},
		"team_name":     {"Синусы", "Косинусы"},
		"team_user_id":  {"", ""},
		"team_login":    {"sin", "cos"},
		"team_password": {"p1", "p2"},
	}
	for lvl := 1; lvl <= 2; lvl++ {
		form.Set(fmt.Sprintf("task_cost_%d", lvl), "1000")
		form.Set(fmt.Sprintf("test_cost_%d", lvl), "500")
		form.Set(fmt.Sprintf("load_%d", lvl), "1")
		form.Set(fmt.Sprintf("amount_bonus_%d", lvl), "2000")
		form.Set(fmt.Sprintf("speed_bonus_%d", lvl), "1")
	}
	resp, err := c.PostForm(ts.URL+"/admin/games/new", form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Request.URL.Path, "/admin/g/") {
		t.Fatalf("ручная игра не создана: %s\n%.700s", resp.Request.URL.Path, body)
	}
	var id int64
	fmt.Sscanf(strings.TrimPrefix(resp.Request.URL.Path, "/admin/g/"), "%d", &id)
	return id
}

// Ручной (математический) режим: без ссылок, без user_id, self-buy запрещён,
// полный цикл купить→решить через админку работает.
func TestManualMode(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGame(t, ts, c, csrf)

	// Задачи-плейсхолдеры: 4 штуки, отрицательные chapterid, без URL.
	tasks, _ := st.GetTasks(gameID)
	if len(tasks) != 4 {
		t.Fatalf("задач %d, ожидалось 4", len(tasks))
	}
	for _, task := range tasks {
		if task.ChapterID >= 0 || task.URL != "" {
			t.Errorf("плейсхолдер: chapterid=%d url=%q — ожидались отрицательный id и пустой URL", task.ChapterID, task.URL)
		}
	}
	g, _ := st.GetGame(gameID)
	if g.Mode != store.ModeManual {
		t.Fatalf("mode = %q", g.Mode)
	}

	startGame(t, ts, c, csrf, gameID)

	// Публичный и командный state: mode=manual, никаких url/chapterid.
	resp, _ := http.Get(fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID))
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"mode":"manual"`) {
		t.Errorf("в публичном state нет mode=manual")
	}
	tc, teamPath, teamCSRF := teamClient(t, ts, gameID, "sin", "p1")
	resp, _ = tc.Get(ts.URL + "/api" + teamPath + "/state")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(raw), `"url"`) || strings.Contains(string(raw), "chapter_id") {
		t.Errorf("командный state ручной игры содержит ссылки/chapterid:\n%s", raw)
	}

	// Самостоятельная покупка в математическом режиме теперь разрешена.
	buy := func(cl *http.Client, token, path string, cell int) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+path+"/buy",
			strings.NewReader(url.Values{"cell": {fmt.Sprint(cell)}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", token)
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, body := buy(tc, teamCSRF, teamPath, 1); code != http.StatusOK {
		t.Fatalf("self-buy в математическом режиме: HTTP %d %s, ожидался 200", code, body)
	}

	// Купленную командой задачу решает организатор (буквой solve).
	teams, _ := st.GetTeams(gameID)
	order, _ := st.GetTaskOrder(teams[0].ID)
	boughtTaskID := order[1]
	resp, err := c.PostForm(fmt.Sprintf("%s/admin/g/%d/event", ts.URL, gameID), url.Values{
		"csrf": {csrf}, "team_id": {fmt.Sprint(teams[0].ID)},
		"task_id": {fmt.Sprint(boughtTaskID)}, "type": {"solve"}, "confirmed": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var pub struct {
		Teams []struct {
			ID    int64 `json:"id"`
			Speed int64 `json:"speed"`
			Cells []struct {
				State string `json:"state"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	passed := 0
	for _, tm := range pub.Teams {
		for _, cell := range tm.Cells {
			if cell.State == "passed" {
				passed++
			}
		}
	}
	if passed != 1 {
		t.Errorf("решённых ячеек %d, ожидалась 1", passed)
	}
	// Скорость: 15 - 1 (load при покупке) + 1 (load назад) + 1 (бонус) = 16.
	for _, tm := range pub.Teams {
		if tm.ID == teams[0].ID && tm.Speed != 16 {
			t.Errorf("скорость после решения %d, ожидалось 16", tm.Speed)
		}
	}

	// Админский state содержит уровень и вариант, но не chapterid/url.
	var adminState struct {
		State struct {
			Teams []struct {
				Cells []struct {
					TaskID    int64  `json:"task_id"`
					Level     int    `json:"level"`
					Ord       int    `json:"ord"`
					ChapterID int    `json:"chapter_id"`
					URL       string `json:"url"`
				} `json:"cells"`
			} `json:"teams"`
		} `json:"state"`
	}
	getJSON(t, c, fmt.Sprintf("%s/admin/api/g/%d/state", ts.URL, gameID), &adminState)
	cell := adminState.State.Teams[0].Cells[0]
	if cell.TaskID == 0 || cell.Level == 0 || cell.Ord == 0 {
		t.Errorf("админская ячейка без task_id/level/ord: %+v", cell)
	}
	if cell.ChapterID != 0 || cell.URL != "" {
		t.Errorf("админская ячейка ручной игры содержит chapterid/url: %+v", cell)
	}

	// Метки задач в админке: «уровень, вариант», без отрицательных чисел.
	resp, _ = c.Get(fmt.Sprintf("%s/admin/g/%d", ts.URL, gameID))
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "уровень 1, вариант") {
		t.Errorf("на странице админки нет меток «уровень, вариант»")
	}
	if strings.Contains(string(page), "-1001") {
		t.Errorf("на странице админки виден синтетический chapterid")
	}

	// Смена режима после старта запрещена.
	form := editForm(t, st, gameID, csrf)
	form.Set("mode", "informatics")
	form.Set("tasks_1", "https://informatics.msk.ru/mod/statements/view.php?chapterid=1\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=2")
	form.Set("tasks_2", "https://informatics.msk.ru/mod/statements/view.php?chapterid=3\nhttps://informatics.msk.ru/mod/statements/view.php?chapterid=4")
	_, body2 := postEdit(t, ts, c, gameID, form)
	if !strings.Contains(body2, "нельзя менять режим") {
		t.Errorf("смена режима после старта должна отклоняться")
	}
}

// Отсутствие поля mode в форме (curl/устаревшая форма) не должно молча
// переводить черновик из ручного режима в informatics.
func TestManualModeMissingFieldKeepsMode(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGame(t, ts, c, csrf) // черновик, режим manual

	form := editForm(t, st, gameID, csrf)
	form.Del("mode") // поле отсутствует
	form.Set("title", "Без поля режима")
	form.Set("tasks_1", "")
	form.Set("tasks_2", "")
	path, body := postEdit(t, ts, c, gameID, form)
	if strings.Contains(body, "Ошибки формы") {
		t.Fatalf("сохранение не прошло: %s\n%.400s", path, body)
	}
	g, _ := st.GetGame(gameID)
	if g.Mode != store.ModeManual {
		t.Errorf("режим сменился на %q при отсутствии поля mode, ожидался manual", g.Mode)
	}
	// Плейсхолдеры не заменены ссылками.
	tasks, _ := st.GetTasks(gameID)
	for _, task := range tasks {
		if task.ChapterID >= 0 {
			t.Errorf("плейсхолдер заменён: chapterid=%d", task.ChapterID)
		}
	}
}

// createManualGameWithAnswers создаёт математическую игру с эталонными
// ответами (по n на уровень).
func createManualGameWithAnswers(t *testing.T, ts *httptest.Server, c *http.Client, csrf string) int64 {
	t.Helper()
	form := url.Values{
		"csrf": {csrf}, "title": {"Матбой с ответами"}, "mode": {"manual"}, "n": {"2"},
		"start_amount": {"20000"}, "start_speed": {"15"}, "duration_min": {"85"},
		"team_id":       {"", ""},
		"team_name":     {"Синусы", "Косинусы"},
		"team_user_id":  {"", ""},
		"team_login":    {"sin", "cos"},
		"team_password": {"p1", "p2"},
		// уровень 1: варианты «42» и «Привет Мир»; уровень 2: «3.14» и пусто.
		// Условия: у 1.1 — текст, у 1.2 — ссылка, остальные без условия.
		"ans_1_1":  {"42"},
		"ans_1_2":  {"Привет Мир"},
		"ans_2_1":  {"3.14"},
		"ans_2_2":  {""},
		"stmt_1_1": {"Найдите ответ на главный вопрос"},
		"stmt_1_2": {"https://example.org/task2.pdf"},
	}
	for lvl := 1; lvl <= 2; lvl++ {
		form.Set(fmt.Sprintf("task_cost_%d", lvl), "1000")
		form.Set(fmt.Sprintf("test_cost_%d", lvl), "500")
		form.Set(fmt.Sprintf("load_%d", lvl), "1")
		form.Set(fmt.Sprintf("amount_bonus_%d", lvl), "2000")
		form.Set(fmt.Sprintf("speed_bonus_%d", lvl), "1")
	}
	resp, err := c.PostForm(ts.URL+"/admin/games/new", form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Request.URL.Path, "/admin/g/") {
		t.Fatalf("игра с ответами не создана: %s\n%.700s", resp.Request.URL.Path, body)
	}
	var id int64
	fmt.Sscanf(strings.TrimPrefix(resp.Request.URL.Path, "/admin/g/"), "%d", &id)
	return id
}

// Приём ответов в математическом режиме: верный засчитывает, неверный нет,
// эталон нормализуется, ответы не утекают, has_answer в состоянии.
func TestManualAnswers(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGameWithAnswers(t, ts, c, csrf)

	// Ответы сохранены нормализованными по вариантам.
	tasks, _ := st.GetTasks(gameID)
	byLvlOrd := map[[2]int]store.Task{}
	for _, tk := range tasks {
		byLvlOrd[[2]int{tk.Level, tk.Ord}] = tk
	}
	if byLvlOrd[[2]int{1, 1}].Answer != "42" || byLvlOrd[[2]int{1, 2}].Answer != "Привет Мир" {
		t.Fatalf("ответы уровня 1 не сохранены: %+v", tasks)
	}
	if byLvlOrd[[2]int{2, 1}].Answer != "3.14" || byLvlOrd[[2]int{2, 2}].Answer != "" {
		t.Fatalf("ответы уровня 2 не сохранены: %+v", tasks)
	}

	startGame(t, ts, c, csrf, gameID)
	teams, _ := st.GetTeams(gameID)
	tc, teamPath, teamCSRF := teamClient(t, ts, gameID, "sin", "p1")

	// Отличимые значения ответов не должны утекать в командный state — ни до,
	// ни после покупки (когда у ячейки появляются детали). «42» не проверяем:
	// оно совпадает с секундами в таймстампах.
	assertNoAnswerLeak := func(where string) {
		resp, _ := tc.Get(ts.URL + "/api" + teamPath + "/state")
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, secret := range []string{"Привет", "3.14", `"answer"`} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("эталонный ответ %q утёк в состояние (%s):\n%s", secret, where, raw)
			}
		}
	}
	assertNoAnswerLeak("до покупки")

	answer := func(path, token string, cell int, ans string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+path+"/answer",
			strings.NewReader(url.Values{"cell": {fmt.Sprint(cell)}, "answer": {ans}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", token)
		resp, err := tc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp.StatusCode, out
	}

	// Найдём ячейку команды sin, где стоит задача уровня 1, вариант 1 (ответ «42»).
	order, _ := st.GetTaskOrder(teams[0].ID)
	want := byLvlOrd[[2]int{1, 1}].ID
	targetCell := 0
	for cell, tid := range order {
		if tid == want {
			targetCell = cell
		}
	}
	if targetCell == 0 {
		t.Fatal("не нашли ячейку с задачей уровня 1 вариант 1")
	}

	// Ответ до покупки — отказ.
	if code, out := answer(teamPath, teamCSRF, targetCell, "42"); code != http.StatusConflict {
		t.Errorf("ответ до покупки: HTTP %d %v, ожидался 409", code, out)
	}
	// Покупаем ячейку.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath+"/buy",
		strings.NewReader(url.Values{"cell": {fmt.Sprint(targetCell)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", teamCSRF)
	resp, _ := tc.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// После покупки у ячейки появляются детали — эталон всё равно не утёк.
	assertNoAnswerLeak("после покупки")

	// Неверный ответ: 200, correct=false, задача не решена.
	code, out := answer(teamPath, teamCSRF, targetCell, "41")
	if code != http.StatusOK || out["correct"] != false {
		t.Errorf("неверный ответ: HTTP %d %v, ожидалось 200 correct=false", code, out)
	}
	// Верный ответ с «грязью» (регистр/пробелы) — засчитан.
	code, out = answer(teamPath, teamCSRF, targetCell, "  42 ")
	if code != http.StatusOK || out["correct"] != true {
		t.Errorf("верный ответ: HTTP %d %v, ожидалось 200 correct=true", code, out)
	}
	// Задача стала решённой; повторный ответ — «уже решена».
	var pub struct {
		Teams []struct {
			ID    int64 `json:"id"`
			Cells []struct {
				Cell  int    `json:"cell"`
				State string `json:"state"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, c, fmt.Sprintf("%s/api/g/%d/state", ts.URL, gameID), &pub)
	solved := false
	for _, tm := range pub.Teams {
		if tm.ID != teams[0].ID {
			continue
		}
		for _, cell := range tm.Cells {
			if cell.Cell == targetCell && cell.State == "passed" {
				solved = true
			}
		}
	}
	if !solved {
		t.Errorf("после верного ответа задача не засчитана")
	}
	if code, _ := answer(teamPath, teamCSRF, targetCell, "42"); code != http.StatusConflict {
		t.Errorf("ответ по решённой задаче: HTTP %d, ожидался 409", code)
	}

	// has_answer виден у своей купленной задачи с эталоном.
	var teamState struct {
		Teams []struct {
			ID    int64 `json:"id"`
			Cells []struct {
				Cell      int  `json:"cell"`
				HasAnswer bool `json:"has_answer"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, tc, ts.URL+"/api"+teamPath+"/state", &teamState)
	// Купим задачу уровня 2 вариант 2 (без эталона) и проверим has_answer=false.
	noAnsTask := byLvlOrd[[2]int{2, 2}].ID
	noAnsCell := 0
	for cell, tid := range order {
		if tid == noAnsTask {
			noAnsCell = cell
		}
	}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath+"/buy",
		strings.NewReader(url.Values{"cell": {fmt.Sprint(noAnsCell)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", teamCSRF)
	resp, _ = tc.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if code, out := answer(teamPath, teamCSRF, noAnsCell, "что угодно"); code != http.StatusConflict {
		t.Errorf("ответ по задаче без эталона: HTTP %d %v, ожидался 409", code, out)
	}
	getJSON(t, tc, ts.URL+"/api"+teamPath+"/state", &teamState)
	for _, tm := range teamState.Teams {
		if tm.ID != teams[0].ID {
			continue
		}
		for _, cell := range tm.Cells {
			if cell.Cell == noAnsCell && cell.HasAnswer {
				t.Errorf("has_answer=true у задачи без эталона")
			}
		}
	}

	// В informatics-игре приём ответов недоступен.
	gid2 := createGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gid2)
	tc2, teamPath2, teamCSRF2 := teamClient(t, ts, gid2, "alpha", "pa")
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath2+"/answer",
		strings.NewReader("cell=1&answer=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", teamCSRF2)
	resp, _ = tc2.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("приём ответа в informatics-игре: HTTP %d, ожидался 409", resp.StatusCode)
	}
}

func TestAnswerCooldown(t *testing.T) {
	cases := map[int]time.Duration{
		0: 0, 1: 0, 2: 2 * time.Second, 3: 4 * time.Second,
		4: 8 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 10: 30 * time.Second,
	}
	for count, want := range cases {
		if got := answerCooldown(count); got != want {
			t.Errorf("answerCooldown(%d) = %v, ожидалось %v", count, got, want)
		}
	}
}

// Анти-брутфорс: серия неверных ответов включает нарастающую задержку (429),
// верный ответ её сбрасывает.
func TestAnswerBruteforceCooldown(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGameWithAnswers(t, ts, c, csrf) // level1 var1 = «42»
	startGame(t, ts, c, csrf, gameID)
	teams, _ := st.GetTeams(gameID)
	tasks, _ := st.GetTasks(gameID)
	var want int64
	for _, tk := range tasks {
		if tk.Level == 1 && tk.Ord == 1 {
			want = tk.ID
		}
	}
	order, _ := st.GetTaskOrder(teams[0].ID)
	targetCell := 0
	for cell, tid := range order {
		if tid == want {
			targetCell = cell
		}
	}
	tc, teamPath, teamCSRF := teamClient(t, ts, gameID, "sin", "p1")
	// Покупаем.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath+"/buy",
		strings.NewReader(url.Values{"cell": {fmt.Sprint(targetCell)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", teamCSRF)
	resp, _ := tc.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	ans := func(a string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath+"/answer",
			strings.NewReader(url.Values{"cell": {fmt.Sprint(targetCell)}, "answer": {a}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", teamCSRF)
		resp, err := tc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp.StatusCode, out
	}
	// Первая ошибка бесплатна.
	if code, out := ans("0"); code != http.StatusOK || out["correct"] != false {
		t.Fatalf("1-я ошибка: HTTP %d %v", code, out)
	}
	// Вторая ошибка ставит задержку; третья попытка сразу — 429.
	if code, _ := ans("1"); code != http.StatusOK {
		t.Fatalf("2-я ошибка: HTTP %d", code)
	}
	code, out := ans("2")
	if code != http.StatusTooManyRequests {
		t.Errorf("3-я попытка сразу после задержки: HTTP %d, ожидался 429 (%v)", code, out)
	}
	if _, ok := out["wait"]; !ok {
		t.Errorf("в ответе 429 нет поля wait: %v", out)
	}
	// Даже верный ответ во время кулдауна блокируется (429) — задача не решена.
	if code, _ := ans("42"); code != http.StatusTooManyRequests {
		t.Errorf("верный ответ во время кулдауна: HTTP %d, ожидался 429", code)
	}
	// Сброс кулдауна после верного ответа покрыт в TestManualAnswers
	// (одна бесплатная ошибка → верный ответ проходит без задержки).
}

// Условие математической задачи показывается команде после покупки: текст —
// в поле statement, ссылка — в url; до покупки условие скрыто и не утекает.
func TestManualStatement(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGameWithAnswers(t, ts, c, csrf) // 1.1 текст, 1.2 ссылка
	startGame(t, ts, c, csrf, gameID)
	teams, _ := st.GetTeams(gameID)
	tasks, _ := st.GetTasks(gameID)
	byLvlOrd := map[[2]int]store.Task{}
	for _, tk := range tasks {
		byLvlOrd[[2]int{tk.Level, tk.Ord}] = tk
	}
	// Условия сохранены.
	if byLvlOrd[[2]int{1, 1}].Statement != "Найдите ответ на главный вопрос" {
		t.Fatalf("условие 1.1 не сохранено: %q", byLvlOrd[[2]int{1, 1}].Statement)
	}
	if byLvlOrd[[2]int{1, 2}].Statement != "https://example.org/task2.pdf" {
		t.Fatalf("условие 1.2 (ссылка) не сохранено: %q", byLvlOrd[[2]int{1, 2}].Statement)
	}

	tc, teamPath, teamCSRF := teamClient(t, ts, gameID, "sin", "p1")
	order, _ := st.GetTaskOrder(teams[0].ID)
	cellOf := func(lvl, ord int) int {
		for cell, tid := range order {
			if tid == byLvlOrd[[2]int{lvl, ord}].ID {
				return cell
			}
		}
		return 0
	}
	textCell := cellOf(1, 1) // условие текстом
	linkCell := cellOf(1, 2) // условие ссылкой

	teamState := func() string {
		resp, _ := tc.Get(ts.URL + "/api" + teamPath + "/state")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	// До покупки условие не утекает.
	if s := teamState(); strings.Contains(s, "Найдите ответ") || strings.Contains(s, "example.org") {
		t.Fatalf("условие утекло до покупки:\n%s", s)
	}
	buy := func(cell int) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api"+teamPath+"/buy",
			strings.NewReader(url.Values{"cell": {fmt.Sprint(cell)}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", teamCSRF)
		resp, _ := tc.Do(req)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	buy(textCell)
	buy(linkCell)

	var state struct {
		Teams []struct {
			ID    int64 `json:"id"`
			Cells []struct {
				Cell      int    `json:"cell"`
				Statement string `json:"statement"`
				URL       string `json:"url"`
			} `json:"cells"`
		} `json:"teams"`
	}
	getJSON(t, tc, ts.URL+"/api"+teamPath+"/state", &state)
	var own *struct {
		ID    int64 `json:"id"`
		Cells []struct {
			Cell      int    `json:"cell"`
			Statement string `json:"statement"`
			URL       string `json:"url"`
		} `json:"cells"`
	}
	for i := range state.Teams {
		if state.Teams[i].ID == teams[0].ID {
			own = &state.Teams[i]
		}
	}
	for _, cell := range own.Cells {
		if cell.Cell == textCell {
			if cell.Statement != "Найдите ответ на главный вопрос" || cell.URL != "" {
				t.Errorf("условие-текст: statement=%q url=%q", cell.Statement, cell.URL)
			}
		}
		if cell.Cell == linkCell {
			if cell.URL != "https://example.org/task2.pdf" || cell.Statement != "" {
				t.Errorf("условие-ссылка: url=%q statement=%q", cell.URL, cell.Statement)
			}
		}
	}

	// В состоянии другой команды условия наших ячеек не видны (проверяем,
	// что statement/url есть только у своих купленных).
	other := state.Teams[1]
	if own.ID == other.ID {
		other = state.Teams[0]
	}
	for _, cell := range other.Cells {
		if cell.Statement != "" || cell.URL != "" {
			t.Errorf("условие чужой команды видно: %+v", cell)
		}
	}

	// Правка условия купленной задачи посреди игры долетает до состояния.
	form := editForm(t, st, gameID, csrf)
	form.Set("mode", "manual")
	form.Set("stmt_1_1", "исправленное условие")
	if _, body := postEdit(t, ts, c, gameID, form); strings.Contains(body, "Ошибки формы") {
		t.Fatalf("правка условия не прошла:\n%.300s", body)
	}
	getJSON(t, tc, ts.URL+"/api"+teamPath+"/state", &state)
	for i := range state.Teams {
		if state.Teams[i].ID != teams[0].ID {
			continue
		}
		for _, cell := range state.Teams[i].Cells {
			if cell.Cell == textCell && cell.Statement != "исправленное условие" {
				t.Errorf("правка условия купленной задачи не долетела: %q", cell.Statement)
			}
		}
	}
}

// Редактирование ответов: форма реконструирует эталоны из БД, правка одного
// не стирает остальные (в т. ч. пустые варианты сохраняют позицию).
func TestManualAnswersEditRoundTrip(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGameWithAnswers(t, ts, c, csrf) // 42/Привет Мир, 3.14/(пусто)

	// Форма редактирования восстанавливает поля условие+ответ по вариантам.
	resp, _ := c.Get(fmt.Sprintf("%s/admin/g/%d/edit", ts.URL, gameID))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{
		`name="ans_1_1" value="42"`,
		`name="ans_1_2" value="Привет Мир"`,
		`name="ans_2_1" value="3.14"`,
		`name="stmt_1_1"`, `Найдите ответ на главный вопрос`,
		`name="stmt_1_2"`, `https://example.org/task2.pdf`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("в форме редактирования нет %q", want)
		}
	}

	// Меняем только ответ и условие варианта 1.2, остальное сохраняется.
	form := editForm(t, st, gameID, csrf)
	form.Set("mode", "manual")
	form.Set("ans_1_2", "Пока")
	form.Set("stmt_1_2", "новое условие текстом")
	path, respBody := postEdit(t, ts, c, gameID, form)
	if strings.Contains(respBody, "Ошибки формы") {
		t.Fatalf("правка не прошла: %s\n%.400s", path, respBody)
	}
	tasks, _ := st.GetTasks(gameID)
	gotAns := map[[2]int]string{}
	gotStmt := map[[2]int]string{}
	for _, tk := range tasks {
		gotAns[[2]int{tk.Level, tk.Ord}] = tk.Answer
		gotStmt[[2]int{tk.Level, tk.Ord}] = tk.Statement
	}
	wantAns := map[[2]int]string{{1, 1}: "42", {1, 2}: "Пока", {2, 1}: "3.14", {2, 2}: ""}
	for k, v := range wantAns {
		if gotAns[k] != v {
			t.Errorf("ответ уровень %d вариант %d = %q, ожидалось %q", k[0], k[1], gotAns[k], v)
		}
	}
	if gotStmt[[2]int{1, 1}] != "Найдите ответ на главный вопрос" {
		t.Errorf("условие 1.1 не сохранилось: %q", gotStmt[[2]int{1, 1}])
	}
	if gotStmt[[2]int{1, 2}] != "новое условие текстом" {
		t.Errorf("условие 1.2 не обновилось: %q", gotStmt[[2]int{1, 2}])
	}
}

// Редактирование ручной игры (в т. ч. после старта) не требует ссылок и
// сохраняет плейсхолдеры.
func TestManualModeEdit(t *testing.T) {
	ts, st, _ := testServer(t)
	c, csrf := adminClient(t, ts)
	gameID := createManualGame(t, ts, c, csrf)
	startGame(t, ts, c, csrf, gameID)

	tasksBefore, _ := st.GetTasks(gameID)
	form := editForm(t, st, gameID, csrf)
	form.Set("mode", "manual")
	form.Set("title", "Матбой-финал")
	form.Set("task_cost_1", "2000")
	// tasks_1/tasks_2 в форме пустые (editForm собрал пустые URL) — ок.
	form.Set("tasks_1", "")
	form.Set("tasks_2", "")
	path, body := postEdit(t, ts, c, gameID, form)
	if strings.Contains(body, "Ошибки формы") {
		t.Fatalf("редактирование ручной игры не прошло: %s\n%.500s", path, body)
	}
	tasksAfter, _ := st.GetTasks(gameID)
	for i := range tasksBefore {
		if tasksAfter[i].ID != tasksBefore[i].ID || tasksAfter[i].ChapterID != tasksBefore[i].ChapterID {
			t.Errorf("плейсхолдер %d изменился: %+v -> %+v", i, tasksBefore[i], tasksAfter[i])
		}
	}
	g, _ := st.GetGame(gameID)
	if g.Title != "Матбой-финал" {
		t.Errorf("правка названия не применилась")
	}
}

// Неизвестные id — 404.
func TestNotFound(t *testing.T) {
	ts, _, _ := testServer(t)
	for _, u := range []string{"/g/999", "/api/g/999/state", "/g/abc"} {
		resp, _ := http.Get(ts.URL + u)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: HTTP %d, ожидался 404", u, resp.StatusCode)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
