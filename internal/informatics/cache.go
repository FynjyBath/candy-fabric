package informatics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Cache — кеш опросчика (7.6): max_run_id по аккаунтам. Файл
// data/cache/informatics_state.json, запись атомарная (temp + rename),
// доступ под мьютексом. Сброс кеша не приводит к дублям событий —
// дедупликация по run_id в БД.
type Cache struct {
	mu   sync.Mutex
	path string
	data cacheFile
}

type cacheFile struct {
	Version  int                     `json:"version"`
	Accounts map[string]cacheAccount `json:"accounts"`
}

type cacheAccount struct {
	MaxRunID  int64  `json:"max_run_id"`
	UpdatedAt string `json:"updated_at"`
}

const cacheVersion = 1

func OpenCache(path string) *Cache {
	c := &Cache{path: path, data: cacheFile{Version: cacheVersion, Accounts: map[string]cacheAccount{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		return c // файла нет — пустой кеш
	}
	var f cacheFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != cacheVersion {
		return c // не парсится или версия не совпадает — начать с нуля
	}
	if f.Accounts == nil {
		f.Accounts = map[string]cacheAccount{}
	}
	c.data = f
	return c
}

// MaxRunID возвращает (max_run_id, известен ли аккаунт).
func (c *Cache) MaxRunID(userID int) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Accounts[strconv.Itoa(userID)]
	return a.MaxRunID, ok
}

// SetMaxRunID обновляет max_run_id аккаунта и атомарно сохраняет файл.
func (c *Cache) SetMaxRunID(userID int, maxRunID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data.Accounts[strconv.Itoa(userID)] = cacheAccount{
		MaxRunID:  maxRunID,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return c.saveLocked()
}

func (c *Cache) saveLocked() error {
	b, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".informatics_state-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
