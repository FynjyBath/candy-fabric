// Package links — нормализация ссылок на задачи информатикса (раздел 6 ТЗ).
package links

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Допустимые хосты (зеркала считаются эквивалентными).
var allowedHosts = map[string]bool{
	"informatics.msk.ru":       true,
	"www.informatics.msk.ru":   true,
	"informatics.mccme.ru":     true,
	"www.informatics.mccme.ru": true,
}

// Normalize разбирает ссылку на задачу и возвращает chapterid.
// Правила: допустимые хосты-зеркала, путь /mod/statements/view.php (без учёта
// регистра), параметр chapterid — положительное целое; фрагмент (#1) и прочие
// параметры игнорируются.
func Normalize(raw string) (chapterID int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("пустая ссылка")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("не удалось разобрать ссылку: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, fmt.Errorf("ожидается ссылка http(s)")
	}
	host := strings.ToLower(u.Hostname())
	if !allowedHosts[host] {
		return 0, fmt.Errorf("недопустимый хост %q (ожидается informatics.msk.ru или informatics.mccme.ru)", u.Hostname())
	}
	if !strings.EqualFold(strings.TrimRight(u.Path, "/"), "/mod/statements/view.php") {
		return 0, fmt.Errorf("недопустимый путь %q (ожидается /mod/statements/view.php)", u.Path)
	}
	vals := u.Query()
	s := vals.Get("chapterid")
	if s == "" {
		// параметры без учёта регистра ключа
		for k, v := range vals {
			if strings.EqualFold(k, "chapterid") && len(v) > 0 {
				s = v[0]
				break
			}
		}
	}
	if s == "" {
		return 0, fmt.Errorf("нет параметра chapterid")
	}
	id, err := strconv.Atoi(s)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("chapterid должен быть положительным целым, получено %q", s)
	}
	return id, nil
}

// CanonicalURL — канонический URL задачи для показа на страницах команд.
// baseURL берётся из informatics_credentials.json.
func CanonicalURL(baseURL string, chapterID int) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://informatics.msk.ru"
	}
	return fmt.Sprintf("%s/mod/statements/view.php?chapterid=%d", base, chapterID)
}
