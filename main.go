package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func healthcheck() error {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + env("PORT", "8787") + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		OK bool `json:"ok"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if resp.StatusCode != 200 || !body.OK {
		return errors.New("unhealthy")
	}
	return nil
}
func run() error {
	host, port, password := env("HOST", "127.0.0.1"), env("PORT", "8787"), os.Getenv("APP_PASSWORD")
	if host != "127.0.0.1" && host != "localhost" && host != "::1" && len([]rune(password)) < 8 {
		return errors.New("对局域网提供服务时，请设置至少 8 位 APP_PASSWORD")
	}
	s, err := newStore(env("MEDIA_ROOT", "./media"), env("DATA_DIR", "./data"))
	if err != nil {
		return fmt.Errorf("初始化数据目录失败: %w", err)
	}
	defer s.close()
	server := &http.Server{Addr: net.JoinHostPort(host, port), Handler: newWebApp(s, password), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 130 * time.Second, IdleTimeout: 60 * time.Second}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	log.Printf("AI NFO ready at http://%s", listener.Addr())
	select {
	case <-ctx.Done():
		s.cancel()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
		}
		err = <-serverErrors
	case err = <-serverErrors:
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
