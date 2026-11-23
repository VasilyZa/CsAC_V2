package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"csac/v2/internal/account"
	"csac/v2/internal/chat"
	"csac/v2/internal/config"
	"csac/v2/internal/mailer"
	"csac/v2/internal/media"
	"csac/v2/internal/moderation"
	"csac/v2/internal/netserve"
	"csac/v2/internal/realtime"
	"csac/v2/internal/release"
	"csac/v2/internal/rpc"
	"csac/v2/internal/schema"
	"csac/v2/internal/server"
	"csac/v2/internal/social"
	"csac/v2/internal/space"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, logger); err != nil {
		logger.Error("service exited", "reason", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Env()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return err
	}
	dbCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return errors.New("Invalid CSAC_DATABASE_URL")
	}
	dbCfg.MaxConns = cfg.PoolSize
	dbCfg.ConnConfig.ConnectTimeout = 5 * time.Second
	start, stop := context.WithTimeout(ctx, time.Minute)
	defer stop()
	pool, err := pgxpool.NewWithConfig(start, dbCfg)
	if err != nil {
		return errors.New("PostgreSQL pool could not initialize")
	}
	defer pool.Close()
	if err := pool.Ping(start); err != nil {
		return errors.New("PostgreSQL is unreachable or credentials are invalid")
	}
	if err := schema.Apply(start, pool); err != nil {
		return err
	}
	if err := config.SyncAdmins(start, pool, cfg.AdminUIDs); err != nil {
		return err
	}

	mail := mailer.Sender{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: cfg.SMTPFrom}
	var deliver func(context.Context, string, string, string) error
	if cfg.SMTPHost != "" {
		deliver = mail.Send
	}
	auth := account.New(pool, account.Config{
		RequireRegistrationEmail: cfg.RequireRegistrationEmail,
		RequireMemberEmail:       cfg.RequireMemberEmail,
		SessionTTL:               cfg.SessionTTL,
		Deliver:                  deliver,
		RegistrationCodeKey:      cfg.AccountKey,
		PasswordPepper:           cfg.PasswordPepper,
	})
	clear(cfg.AccountKey)
	clear(cfg.PasswordPepper)
	files, err := media.Open(pool, cfg.MediaRoot, media.Options{
		MaxBytes: cfg.MaxFileBytes, FileRetention: cfg.FileRetention, FileExtensions: cfg.FileExtensions,
	})
	if err != nil {
		return err
	}
	defer files.Close()
	releases, err := release.OpenWithOptions(cfg.ReleaseRoot, release.Options{
		GitHubRepo: cfg.ReleaseGitHubRepo, PollInterval: cfg.ReleasePollInterval, DeployWeb: cfg.ReleaseDeployWeb,
	})
	if err != nil {
		return err
	}
	defer releases.Close()

	r := rpc.New(pool, log)
	hub := realtime.New(pool, log)
	auth.Register(r)
	social.Register(r)
	files.Register(r)
	space.Register(r)
	chat.Register(r, hub)
	moderation.Register(r)
	releases.Register(r)
	hub.Register(r)
	app := server.New(pool, r, auth, hub, files, releases, cfg.MaxConnections, cfg.AllowedOrigins, log)

	// Keep workers available until the transports have drained their handlers.
	live, closeLive := context.WithCancel(context.WithoutCancel(ctx))
	defer closeLive()
	done, err := hub.Start(live, cfg.EventRetention)
	if err != nil {
		return err
	}
	defer func() { closeLive(); <-done }()
	handler := server.WithTrustedProxy(app, cfg.TrustedProxyToken)
	clear(cfg.TrustedProxyToken)
	srv, err := netserve.Listen(ctx, netserve.Config{Addr: cfg.Addr, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}, handler)
	if err != nil {
		return err
	}
	log.Info("CsAC listening", "addr", srv.Addr(), "http_versions", []int{1, 2, 3})
	cleanDone, releaseDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(cleanDone); maintenance(live, log, files, auth) }()
	go func() {
		defer close(releaseDone)
		releases.Run(live, func(err error) { log.Warn("release refresh failed", "reason", err.Error()) })
	}()
	err = srv.Wait()
	closeLive()
	<-cleanDone
	<-releaseDone
	return err
}

func maintenance(ctx context.Context, log *slog.Logger, files *media.Service, auth *account.Service) {
	tick := time.NewTicker(15 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			clean, cancel := context.WithTimeout(ctx, time.Minute)
			if err := files.Cleanup(clean); err != nil && ctx.Err() == nil {
				log.Warn("media cleanup failed")
			}
			if err := auth.Maintenance(clean); err != nil && ctx.Err() == nil {
				log.Warn("account cleanup failed")
			}
			cancel()
		}
	}
}
