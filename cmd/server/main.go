package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"neutrino/internal/app"
	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func main() {
	cfg := config.Load()

	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	if err := db.Migrate(conn); err != nil {
		log.Fatalf("migrate db: %v", err)
	}

	store := repo.New(conn, cfg)
	if cfg.AllowBasicAuth && !cfg.BasicAuthAllowed() {
		log.Fatalf("ALLOW_BASIC_AUTH requires non-default ADMIN_USER/ADMIN_PASS")
	}
	if err := store.EnsureAdminCredential(context.Background(), cfg.AdminUser, cfg.AdminPass); err != nil {
		log.Fatalf("init admin credential: %v", err)
	}

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := app.New(cfg, store)
	go server.StartWorkers(runCtx)

	log.Printf("admin user: %s", cfg.AdminUser)
	log.Printf("listening on %s", cfg.Addr)

	errCh := make(chan error, 2)
	var agentSrv *http.Server

	// Agent -> panel (mTLS) listener. Greenfield: this is the only supported auth for node-agent.
	if cfg.PanelAgentMTLSAddr != "" {
		if cfg.PanelAgentMTLSCACertPaths == "" || cfg.PanelAgentMTLSServerCertPath == "" || cfg.PanelAgentMTLSServerKeyPath == "" {
			log.Fatalf("agent mTLS listener requires PANEL_AGENT_MTLS_CA_CERT_PATHS, PANEL_AGENT_MTLS_SERVER_CERT_PATH, PANEL_AGENT_MTLS_SERVER_KEY_PATH")
		}
		pool := x509.NewCertPool()
		for _, p := range splitCSV(cfg.PanelAgentMTLSCACertPaths) {
			caPEM, err := os.ReadFile(p)
			if err != nil {
				log.Fatalf("read PANEL_AGENT_MTLS_CA_CERT_PATHS item %q: %v", p, err)
			}
			if ok := pool.AppendCertsFromPEM(caPEM); !ok {
				log.Fatalf("invalid CA cert at %q (PANEL_AGENT_MTLS_CA_CERT_PATHS)", p)
			}
		}
		agentTLS := &tls.Config{
			MinVersion: tls.VersionTLS12,
			ClientCAs:  pool,
			ClientAuth: tls.RequireAndVerifyClientCert,
		}
		agentSrv = &http.Server{
			Addr:              cfg.PanelAgentMTLSAddr,
			Handler:           server.AgentRoutes(),
			TLSConfig:         agentTLS,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			log.Printf("agent mTLS listening on %s", cfg.PanelAgentMTLSAddr)
			if err := agentSrv.ListenAndServeTLS(cfg.PanelAgentMTLSServerCertPath, cfg.PanelAgentMTLSServerKeyPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("agent mTLS server: %w", err)
			}
		}()
	}

	mainSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		if err := mainSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("main server: %w", err)
		}
	}()

	var runErr error
	select {
	case runErr = <-errCh:
		stop()
	case <-runCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if agentSrv != nil {
		_ = agentSrv.Shutdown(shutdownCtx)
	}
	_ = mainSrv.Shutdown(shutdownCtx)

	select {
	case err := <-errCh:
		if runErr == nil {
			runErr = err
		}
	default:
	}

	if runErr != nil {
		log.Fatalf("server: %v", runErr)
	}
}

func splitCSV(s string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
