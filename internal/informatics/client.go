// Package informatics — клиент informatics.msk.ru и фоновый опросчик
// (раздел 7 ТЗ, перенос поведения из standings-edu).
package informatics

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credentials — сервисный аккаунт (data/credentials/informatics_credentials.json).
type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	BaseURL  string `json:"base_url"`
}

// LoadCredentials читает и валидирует файл сервисного аккаунта.
func LoadCredentials(path string) (*Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	c.Username = strings.TrimSpace(c.Username)
	c.Password = strings.TrimSpace(c.Password)
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = "https://informatics.msk.ru"
	}
	if c.Username == "" || c.Password == "" {
		return nil, fmt.Errorf("пустые логин/пароль сервисного аккаунта в %s", path)
	}
	return &c, nil
}

// Run — одна посылка пользователя на информатиксе.
type Run struct {
	ID           int64
	CreateTime   time.Time // UTC; при отсутствии/ошибке разбора — момент обнаружения
	TimeParsed   bool      // false, если create_time взят как «момент обнаружения»
	EjudgeStatus int
	ProblemID    int
}

// Solved — решённая посылка: ejudge_status ∈ {0, 8} (OK, ACCEPTED).
func (r *Run) Solved() bool { return r.EjudgeStatus == 0 || r.EjudgeStatus == 8 }

// Client — HTTP-клиент информатикса: Moodle-логин + API посылок.
type Client struct {
	creds  *Credentials
	http   *http.Client
	logger *log.Logger

	loginMu     sync.Mutex
	lastLoginAt time.Time
}

const sessionTTL = 30 * time.Minute

func NewClient(creds *Credentials, logger *log.Logger) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		creds:  creds,
		http:   &http.Client{Timeout: 20 * time.Second, Jar: jar},
		logger: logger,
	}
}

func (c *Client) BaseURL() string { return c.creds.BaseURL }

var loginTokenRe = regexp.MustCompile(`name="logintoken"\s+value="([^"]+)"`)

// login выполняет Moodle-логин (7.2). Вызывается под loginMu.
func (c *Client) login() error {
	loginURL := c.creds.BaseURL + "/login/index.php"
	resp, err := c.http.Get(loginURL)
	if err != nil {
		return fmt.Errorf("GET страницы логина: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("чтение страницы логина: %w", err)
	}
	m := loginTokenRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("informatics login token not found")
	}
	form := url.Values{
		"anchor":           {""},
		"logintoken":       {string(m[1])},
		"username":         {c.creds.Username},
		"password":         {c.creds.Password},
		"rememberusername": {"0"},
	}
	req, err := http.NewRequest(http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", loginURL)
	resp, err = c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST логина: %w", err)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("чтение ответа логина: %w", err)
	}
	s := string(body)
	if strings.Contains(s, "logout.php") {
		c.lastLoginAt = time.Now()
		return nil
	}
	if strings.Contains(s, `name="logintoken"`) || strings.Contains(s, "loginerrors") {
		return fmt.Errorf("bad credentials or blocked account")
	}
	// Неопознанный ответ — считаем ошибкой логина.
	return fmt.Errorf("неопознанный ответ логина информатикса")
}

// ensureLogin логинится, если сессия старше 30 минут или force=true.
func (c *Client) ensureLogin(force bool) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if !force && time.Since(c.lastLoginAt) < sessionTTL {
		return nil
	}
	return c.login()
}

// apiRun — сырой элемент ответа filter-runs.
type apiRun struct {
	ID           int64           `json:"id"`
	CreateTime   json.RawMessage `json:"create_time"`
	EjudgeStatus *flexInt        `json:"ejudge_status"`
	Problem      struct {
		ID int `json:"id"`
	} `json:"problem"`
}

type apiResponse struct {
	Result   string   `json:"result"`
	Message  string   `json:"message"`
	Data     []apiRun `json:"data"`
	Metadata struct {
		PageCount int `json:"page_count"`
		Count     int `json:"count"`
	} `json:"metadata"`
}

// flexInt принимает число, строку с числом, пустую строку и null.
type flexInt struct {
	Value int
	OK    bool
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		// нечисловое => «нет значения», не ошибка
		return nil
	}
	f.Value = v
	f.OK = true
	return nil
}

// FetchRunsPage загружает одну страницу посылок пользователя (7.3), новые сверху.
// При «Not authorized» перелогинивается и повторяет запрос один раз.
func (c *Client) FetchRunsPage(userID int, page int) ([]Run, int, error) {
	if err := c.ensureLogin(false); err != nil {
		return nil, 0, err
	}
	runs, pageCount, err, notAuth := c.fetchRunsPageOnce(userID, page)
	if notAuth {
		if err := c.ensureLogin(true); err != nil {
			return nil, 0, err
		}
		runs, pageCount, err, notAuth = c.fetchRunsPageOnce(userID, page)
		if notAuth {
			return nil, 0, fmt.Errorf("informatics API: Not authorized после перелогина")
		}
	}
	return runs, pageCount, err
}

func (c *Client) fetchRunsPageOnce(userID, page int) (runs []Run, pageCount int, err error, notAuthorized bool) {
	u := fmt.Sprintf("%s/py/problem/0/filter-runs?problem_id=0&user_id=%d&count=1000&page=%d"+
		"&from_timestamp=-1&to_timestamp=-1&lang_id=-1&status_id=-1&statement_id=0&with_comment=",
		c.creds.BaseURL, userID, page)
	resp, err := c.http.Get(u)
	if err != nil {
		return nil, 0, fmt.Errorf("запрос filter-runs: %w", err), false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("чтение ответа filter-runs: %w", err), false
	}
	if resp.StatusCode != http.StatusOK {
		snippet := body
		if len(snippet) > 1024 {
			snippet = snippet[:1024]
		}
		return nil, 0, fmt.Errorf("filter-runs HTTP %d: %s", resp.StatusCode, snippet), false
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, 0, fmt.Errorf("разбор JSON filter-runs: %w", err), false
	}
	if ar.Result == "error" {
		if strings.Contains(strings.ToLower(ar.Message), "not authorized") {
			return nil, 0, nil, true
		}
		return nil, 0, fmt.Errorf("filter-runs error: %s", ar.Message), false
	}
	now := time.Now().UTC()
	for _, r := range ar.Data {
		run := Run{ID: r.ID, ProblemID: r.Problem.ID, EjudgeStatus: -1}
		if r.EjudgeStatus != nil && r.EjudgeStatus.OK {
			run.EjudgeStatus = r.EjudgeStatus.Value
		}
		if t, ok := ParseCreateTime(r.CreateTime); ok {
			run.CreateTime = t
			run.TimeParsed = true
		} else {
			run.CreateTime = now
			if c.logger != nil {
				c.logger.Printf("WARN опросчик: не разобрано create_time посылки %d (%s), взят момент обнаружения", r.ID, string(r.CreateTime))
			}
		}
		runs = append(runs, run)
	}
	return runs, ar.Metadata.PageCount, nil, false
}

// msk — часовой пояс информатикса (строки даты-времени приходят по Москве).
var msk = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return loc
	}
	return time.FixedZone("MSK", 3*3600)
}()

// ParseCreateTime разбирает поле create_time: строка даты-времени по Москве,
// RFC3339 или unix-время (число/строка). Результат — UTC.
func ParseCreateTime(raw json.RawMessage) (time.Time, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}, false
	}
	if !strings.HasPrefix(s, `"`) {
		// число — unix-время (секунды)
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
			sec := int64(v)
			return time.Unix(sec, 0).UTC(), true
		}
		return time.Time{}, false
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return time.Time{}, false
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return time.Time{}, false
	}
	// unix-время строкой
	if v, err := strconv.ParseInt(str, 10, 64); err == nil && v > 1e8 {
		return time.Unix(v, 0).UTC(), true
	}
	// RFC3339 (с зоной)
	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t.UTC(), true
	}
	// строки без зоны — трактуем как московское время
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"02.01.2006 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, str, msk); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
