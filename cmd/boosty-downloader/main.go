package main

import (
	"github.com/SawaMEN/boosty_downloader/internal/server"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg := server.FromEnv()
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
