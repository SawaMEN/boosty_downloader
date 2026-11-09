package main

import (
	"github.com/SawaMEN/boosty_downloader/internal/server"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	cfg := server.FromEnv()
	if len(os.Args) > 1 && os.Args[1] == "reset-admin-password" {
		password, err := io.ReadAll(io.LimitReader(os.Stdin, 2049))
		if err != nil || len(password) > 2048 {
			log.Fatal("не удалось прочитать новый пароль")
		}
		if err = server.ResetAdminPassword(cfg.DataDir, cfg.AdminUser, cfg.AdminPassword, strings.TrimSpace(string(password))); err != nil {
			log.Fatal(err)
		}
		log.Print("Пароль администратора обновлён, старые сессии завершены")
		return
	}
	app, err := server.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	cert, key := os.Getenv("BD_TLS_CERT"), os.Getenv("BD_TLS_KEY")
	if cert == "" || key == "" {
		log.Fatal("требуются BD_TLS_CERT и BD_TLS_KEY: внешняя панель доступна только по HTTPS")
	}
	srv := &http.Server{Addr: cfg.Address, Handler: app.Handler(), ReadHeaderTimeout: 12 * time.Second, IdleTimeout: 90 * time.Second}
	log.Printf("Boosty Downloader listening on %s (HTTPS)", cfg.Address)
	log.Fatal(srv.ListenAndServeTLS(cert, key))
}
