package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Session — подписанная cookie (HMAC-SHA256 с серверным секретом), 24 ч.
type Session struct {
	Role   string `json:"role"` // admin | team
	GameID int64  `json:"game_id,omitempty"`
	TeamID int64  `json:"team_id,omitempty"`
	Exp    int64  `json:"exp"` // unix
}

const sessionCookie = "cf_session"
const sessionTTL = 24 * time.Hour

// LoadOrCreateSecret читает серверный секрет из data/secret, генерируя при
// первом запуске.
func LoadOrCreateSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

func (s *Server) signSession(sess *Session) string {
	payload, _ := json.Marshal(sess)
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(b64))
	return b64 + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) parseSession(value string) *Session {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(parts[0]))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil
	}
	var sess Session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return nil
	}
	if time.Now().Unix() > sess.Exp {
		return nil
	}
	return &sess
}

func (s *Server) setSession(w http.ResponseWriter, sess *Session) {
	sess.Exp = time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.signSession(sess),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
}

func (s *Server) session(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return s.parseSession(c.Value)
}

func (s *Server) isAdmin(r *http.Request) bool {
	sess := s.session(r)
	return sess != nil && sess.Role == "admin"
}

// csrfToken — токен, привязанный к текущей cookie сессии.
func (s *Server) csrfToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("csrf:" + c.Value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) checkCSRF(r *http.Request) bool {
	token := r.FormValue("csrf")
	if token == "" {
		token = r.Header.Get("X-CSRF-Token")
	}
	want := s.csrfToken(r)
	return want != "" && hmac.Equal([]byte(token), []byte(want))
}

// AdminCredentials читаются при каждой попытке входа (правка без рестарта).
type adminCredentials struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func loadAdminCredentials(path string) (*adminCredentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c adminCredentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	if c.Login == "" {
		return nil, fmt.Errorf("пустой логин в %s", path)
	}
	return &c, nil
}
