package main

import (
	"context"
	"crypto/tls"
	"errors"
	"os/signal"
	"syscall"
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
	if err:=serve(cfg);err!=nil{log.Fatal(err)}
}

func serve(cfg server.Config)error{
	cert,key:=os.Getenv("BD_TLS_CERT"),os.Getenv("BD_TLS_KEY")
	if cert==""||key==""{return errors.New("требуются BD_TLS_CERT и BD_TLS_KEY: внешняя панель доступна только по HTTPS")}
	// Fail before starting workers if TLS configuration is unusable.
	if _,err:=tls.LoadX509KeyPair(cert,key);err!=nil{return err}
	ctx,stop:=signal.NotifyContext(context.Background(),os.Interrupt,syscall.SIGTERM)
	defer stop()
	app,err:=server.New(cfg)
	if err!=nil{return err}
	srv:=&http.Server{Addr:cfg.Address,Handler:app.Handler(),ReadHeaderTimeout:12*time.Second,IdleTimeout:90*time.Second}
	result:=make(chan error,1)
	go func(){result<-srv.ListenAndServeTLS(cert,key)}()
	log.Printf("Boosty Downloader listening on %s (HTTPS)",cfg.Address)
	select{
	case err:=<-result:
		app.Close()
		if errors.Is(err,http.ErrServerClosed){return nil}
		return err
	case <-ctx.Done():
		// Drain HTTP before closing the SQL handle and releasing the data
		// directory lock. On timeout, exit with an error; startup journal
		// recovery handles interrupted operations, never pretend clean stop.
		shutdown,cancel:=context.WithTimeout(context.Background(),30*time.Second)
		defer cancel()
		if err:=srv.Shutdown(shutdown);err!=nil{
			_ = srv.Close()
			return err
		}
		app.Close()
		err:=<-result
		if errors.Is(err,http.ErrServerClosed){return nil}
		return err
	}
}
