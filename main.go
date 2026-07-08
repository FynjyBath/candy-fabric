// Конфетная фабрика — онлайн-монитор командного турнира по решению задач
// с автоматическим учётом решений через informatics.msk.ru.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"candyfactory/internal/informatics"
	"candyfactory/internal/store"
	"candyfactory/internal/web"
)

func main() {
	listen := flag.String("listen", ":8080", "адрес HTTP-сервера")
	dataDir := flag.String("data-dir", "./data", "каталог данных")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "период опроса информатикса")
	pageRefresh := flag.Duration("page-refresh", time.Second, "период автообновления страниц")
	flag.Parse()

	// Переменные окружения перекрывают дефолты (флаги — окружение — дефолт).
	if v := os.Getenv("CANDY_LISTEN"); v != "" && !flagPassed("listen") {
		*listen = v
	}
	if v := os.Getenv("CANDY_DATA_DIR"); v != "" && !flagPassed("data-dir") {
		*dataDir = v
	}

	if err := run(*listen, *dataDir, *pollInterval, *pageRefresh); err != nil {
		fmt.Fprintln(os.Stderr, "фатальная ошибка:", err)
		os.Exit(1)
	}
}

func flagPassed(name string) bool {
	passed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

func run(listen, dataDir string, pollInterval, pageRefresh time.Duration) error {
	for _, d := range []string{dataDir, filepath.Join(dataDir, "credentials"), filepath.Join(dataDir, "cache")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// Лог: data/server.log + stdout (9.3).
	logFile, err := os.OpenFile(filepath.Join(dataDir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "", log.LstdFlags)

	secret, err := web.LoadOrCreateSecret(filepath.Join(dataDir, "secret"))
	if err != nil {
		return fmt.Errorf("серверный секрет: %w", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "app.db"))
	if err != nil {
		return fmt.Errorf("открытие БД: %w", err)
	}
	defer st.Close()

	adminCredsPath := filepath.Join(dataDir, "credentials", "admin_credentials.json")
	if _, err := os.Stat(adminCredsPath); os.IsNotExist(err) {
		if err := os.WriteFile(adminCredsPath, []byte("{ \"login\": \"admin\", \"password\": \"change_me\" }\n"), 0o600); err != nil {
			return err
		}
		logger.Printf("WARN создан %s с паролем по умолчанию — поменяйте его", adminCredsPath)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Опросчик: пустые/отсутствующие креды информатикса — фатальная ошибка
	// старта опросчика, но не веб-сервера (7.1): в админке — баннер.
	pollerErr := func() (string, time.Time) { return "", time.Time{} }
	pollerStatus := func() (time.Time, int, int) { return time.Time{}, 0, 0 }
	informaticsBase := "https://informatics.msk.ru"
	credsPath := filepath.Join(dataDir, "credentials", "informatics_credentials.json")
	creds, err := informatics.LoadCredentials(credsPath)
	if err != nil {
		startedAt := time.Now()
		msg := fmt.Sprintf("сервисный аккаунт информатикса не настроен (%v)", err)
		logger.Printf("ERROR %s — опросчик не запущен", msg)
		pollerErr = func() (string, time.Time) { return msg, startedAt }
	} else {
		informaticsBase = creds.BaseURL
		client := informatics.NewClient(creds, logger)
		poller := &informatics.Poller{
			Store:        st,
			Client:       client,
			Cache:        informatics.OpenCache(filepath.Join(dataDir, "cache", "informatics_state.json")),
			Logger:       logger,
			PollInterval: pollInterval,
			AccountPause: time.Second,
		}
		pollerErr = poller.LastError
		pollerStatus = poller.Status
		go poller.Run(ctx)
		logger.Printf("INFO опросчик запущен: %s, период %s", creds.BaseURL, pollInterval)
	}

	srv, err := web.NewServer(web.Config{
		Store:           st,
		Logger:          logger,
		Secret:          secret,
		AdminCredsPath:  adminCredsPath,
		ThemePath:       filepath.Join(dataDir, "theme.txt"),
		PageRefresh:     pageRefresh,
		PollerError:     pollerErr,
		PollerStatus:    pollerStatus,
		InformaticsBase: informaticsBase,
	})
	if err != nil {
		return fmt.Errorf("инициализация веб-сервера: %w", err)
	}

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		httpSrv.Shutdown(shutdownCtx)
	}()
	logger.Printf("INFO сервер слушает %s (данные: %s)", listen, dataDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	logger.Printf("INFO сервер остановлен")
	return nil
}
