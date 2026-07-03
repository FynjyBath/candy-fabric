package informatics

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockInformatics — минимальный мок Moodle-логина и API filter-runs.
type mockInformatics struct {
	mu          int // защита не нужна: httptest сервер + последовательные вызовы
	loginCount  int
	badPassword bool
	requireAuth bool
	authorized  bool
	runsJSON    string
}

func (m *mockInformatics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/index.php", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<form><input type="hidden" name="logintoken" value="tok123"></form>`)
	})
	mux.HandleFunc("POST /login/index.php", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		m.loginCount++
		if r.FormValue("logintoken") != "tok123" || m.badPassword {
			fmt.Fprint(w, `<div class="loginerrors">Invalid login</div><input name="logintoken" value="x">`)
			return
		}
		m.authorized = true
		fmt.Fprint(w, `<a href="https://example/login/logout.php">Выход</a>`)
	})
	mux.HandleFunc("GET /py/problem/0/filter-runs", func(w http.ResponseWriter, r *http.Request) {
		if m.requireAuth && !m.authorized {
			fmt.Fprint(w, `{"result":"error","message":"Not authorized"}`)
			return
		}
		fmt.Fprint(w, m.runsJSON)
	})
	return mux
}

func newMockClient(t *testing.T, m *mockInformatics) *Client {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	return NewClient(&Credentials{Username: "svc", Password: "pw", BaseURL: srv.URL}, log.New(io.Discard, "", 0))
}

const sampleRuns = `{
	"result": "success",
	"data": [
		{"id": 100, "create_time": "2026-07-03 12:30:00", "ejudge_status": 0, "problem": {"id": 111}},
		{"id": 99, "create_time": 1767436200, "ejudge_status": "8", "problem": {"id": 222}},
		{"id": 98, "create_time": null, "ejudge_status": null, "problem": {"id": 333}},
		{"id": 97, "ejudge_status": "", "problem": {"id": 444}}
	],
	"metadata": {"page_count": 1, "count": 4}
}`

func TestClientLoginAndFetch(t *testing.T) {
	m := &mockInformatics{runsJSON: sampleRuns}
	c := newMockClient(t, m)
	runs, pageCount, err := c.FetchRunsPage(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.loginCount != 1 {
		t.Errorf("логинов %d, ожидался 1", m.loginCount)
	}
	if pageCount != 1 || len(runs) != 4 {
		t.Fatalf("получено %d посылок (pageCount %d), ожидалось 4/1", len(runs), pageCount)
	}
	if !runs[0].Solved() || !runs[1].Solved() {
		t.Errorf("посылки 100 и 99 должны быть решёнными (статусы 0 и \"8\")")
	}
	if runs[2].Solved() || runs[3].Solved() {
		t.Errorf("посылки с ejudge_status null/\"\" не должны считаться решёнными")
	}
	if !runs[0].TimeParsed || !runs[1].TimeParsed {
		t.Errorf("create_time строкой и unix-числом должны разбираться")
	}
	if runs[2].TimeParsed || runs[2].CreateTime.IsZero() {
		t.Errorf("отсутствующее create_time — момент обнаружения: %+v", runs[2])
	}
	// Повторный запрос — без нового логина (сессия < 30 минут).
	if _, _, err := c.FetchRunsPage(7, 1); err != nil {
		t.Fatal(err)
	}
	if m.loginCount != 1 {
		t.Errorf("логинов %d — сессия должна переиспользоваться", m.loginCount)
	}
}

func TestClientBadCredentials(t *testing.T) {
	m := &mockInformatics{badPassword: true, runsJSON: sampleRuns}
	c := newMockClient(t, m)
	_, _, err := c.FetchRunsPage(7, 1)
	if err == nil {
		t.Fatal("ожидалась ошибка bad credentials")
	}
}

// «Not authorized» → перелогин и один повтор запроса.
func TestClientRetriesOnNotAuthorized(t *testing.T) {
	m := &mockInformatics{requireAuth: true, runsJSON: sampleRuns}
	c := newMockClient(t, m)
	// Первый ensureLogin залогинится, но сбросим авторизацию, имитируя
	// протухшую сессию на сервере при живом клиентском таймере.
	if err := c.ensureLogin(false); err != nil {
		t.Fatal(err)
	}
	m.authorized = false
	runs, _, err := c.FetchRunsPage(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 {
		t.Errorf("после перелогина ожидалось 4 посылки, получено %d", len(runs))
	}
	if m.loginCount != 2 {
		t.Errorf("логинов %d, ожидалось 2 (начальный + перелогин)", m.loginCount)
	}
}
