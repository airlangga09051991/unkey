package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/Southclaws/fault"
	"github.com/Southclaws/fault/fmsg"
	"github.com/unkeyed/unkey/apps/agent/pkg/api"
	v1Liveness "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_liveness"
	v1RatelimitCommitLease "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_ratelimit_commitLease"
	v1RatelimitMultiRatelimit "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_ratelimit_multiRatelimit"
	v1RatelimitRatelimit "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_ratelimit_ratelimit"
	v1VaultDecrypt "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_vault_decrypt"
	v1VaultEncrypt "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_vault_encrypt"
	v1VaultEncryptBulk "github.com/unkeyed/unkey/apps/agent/pkg/api/routes/v1_vault_encrypt_bulk"
	"github.com/unkeyed/unkey/apps/agent/pkg/cluster"
	"github.com/unkeyed/unkey/apps/agent/pkg/config"
	"github.com/unkeyed/unkey/apps/agent/pkg/connect"
	"github.com/unkeyed/unkey/apps/agent/pkg/membership"
	"github.com/unkeyed/unkey/apps/agent/pkg/metrics"
	"github.com/unkeyed/unkey/apps/agent/pkg/profiling"
	"github.com/unkeyed/unkey/apps/agent/pkg/prometheus"
	"github.com/unkeyed/unkey/apps/agent/pkg/tinybird"
	"github.com/unkeyed/unkey/apps/agent/pkg/tracing"
	"github.com/unkeyed/unkey/apps/agent/pkg/uid"
	"github.com/unkeyed/unkey/apps/agent/pkg/version"
	"github.com/unkeyed/unkey/apps/agent/services/eventrouter"
	"github.com/unkeyed/unkey/apps/agent/services/ratelimit"
	"github.com/unkeyed/unkey/apps/agent/services/vault"
	"github.com/unkeyed/unkey/apps/agent/services/vault/storage"
	storageMiddleware "github.com/unkeyed/unkey/apps/agent/services/vault/storage/middleware"
	"github.com/urfave/cli/v2"
)

var Cmd = &cli.Command{
	Name: "agent",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:        "config",
			Aliases:     []string{"c"},
			Usage:       "Load configuration file",
			Value:       "unkey.json",
			DefaultText: "unkey.json",
			EnvVars:     []string{"AGENT_CONFIG_FILE"},
		},
	},
	Action: run,
}

func run(c *cli.Context) error {
	configFile := c.String("config")

	cfg := config.Agent{}
	err := config.LoadFile(&cfg, configFile)
	if err != nil {
		return err
	}

	if cfg.NodeId == "" {
		cfg.NodeId = uid.Node()

	}
	if cfg.Region == "" {
		cfg.Region = "unknown"
	}
	logger, err := setupLogging(cfg)
	if err != nil {
		return err
	}
	logger = logger.With().Str("nodeId", cfg.NodeId).Str("platform", cfg.Platform).Str("region", cfg.Region).Str("version", version.Version).Logger()

	// Catch any panics now after we have a logger but before we start the server
	defer func() {
		if r := recover(); r != nil {
			logger.Panic().Interface("panic", r).Bytes("stack", debug.Stack()).Msg("panic")
		}
	}()

	logger.Info().Str("file", configFile).Msg("configuration loaded")

	err = profiling.Start(cfg, logger)
	if err != nil {
		return err
	}

	{
		if cfg.Tracing != nil && cfg.Tracing.Axiom != nil {
			closeTracer, err := tracing.Init(context.Background(), tracing.Config{
				Dataset:     cfg.Tracing.Axiom.Dataset,
				Application: "agent",
				Version:     "1.0.0",
				AxiomToken:  cfg.Tracing.Axiom.Token,
			})
			if err != nil {
				return err
			}
			defer func() {
				err = closeTracer()
				if err != nil {
					logger.Error().Err(err).Msg("failed to close tracer")
				}
			}()
			logger.Info().Msg("tracing to axiom")
		}
	}

	m := metrics.NewNoop()
	if cfg.Metrics != nil && cfg.Metrics.Axiom != nil {
		realMetrics, err := metrics.New(metrics.Config{
			Token:   cfg.Metrics.Axiom.Token,
			Dataset: cfg.Metrics.Axiom.Dataset,
			Logger:  logger.With().Str("pkg", "metrics").Logger(),
			NodeId:  cfg.NodeId,
			Region:  cfg.Region,
		})
		if err != nil {
			logger.Fatal().Err(err).Msg("unable to start metrics")
		}
		m = realMetrics

	}
	defer m.Close()

	if cfg.Heartbeat != nil {
		setupHeartbeat(cfg, logger)
	}

	srv := api.New(api.Config{
		NodeId:  cfg.NodeId,
		Logger:  logger,
		Metrics: m,
	})

	v1Liveness.Register(srv.HumaAPI(), srv.Services())

	connectSrv, err := connect.New(connect.Config{Logger: logger, Image: cfg.Image, Metrics: m})
	if err != nil {
		return err
	}

	if cfg.Services.Vault != nil {
		s3, err := storage.NewS3(storage.S3Config{
			S3URL:             cfg.Services.Vault.S3Url,
			S3Bucket:          cfg.Services.Vault.S3Bucket,
			S3AccessKeyId:     cfg.Services.Vault.S3AccessKeyId,
			S3AccessKeySecret: cfg.Services.Vault.S3AccessKeySecret,
			Logger:            logger,
		})
		if err != nil {
			return fmt.Errorf("failed to create s3 storage: %w", err)
		}
		s3 = storageMiddleware.WithTracing("s3", s3)
		srv.Vault, err = vault.New(vault.Config{
			Logger:     logger,
			Metrics:    m,
			Storage:    s3,
			MasterKeys: strings.Split(cfg.Services.Vault.MasterKeys, ","),
		})
		if err != nil {
			return fmt.Errorf("failed to create vault: %w", err)
		}

		if err != nil {
			return fmt.Errorf("failed to create vault service: %w", err)
		}

		v1VaultEncrypt.Register(srv.HumaAPI(), srv.Services(), srv.BearerAuthFromSecret(cfg.Services.Vault.AuthToken))
		v1VaultEncryptBulk.Register(srv.HumaAPI(), srv.Services(), srv.BearerAuthFromSecret(cfg.Services.Vault.AuthToken))
		v1VaultDecrypt.Register(srv.HumaAPI(), srv.Services(), srv.BearerAuthFromSecret(cfg.Services.Vault.AuthToken))
		logger.Info().Msg("started vault service")
	}

	if cfg.Services.EventRouter != nil {
		er, err := eventrouter.New(eventrouter.Config{
			Logger:        logger,
			Metrics:       m,
			BatchSize:     cfg.Services.EventRouter.Tinybird.BatchSize,
			BufferSize:    cfg.Services.EventRouter.Tinybird.BufferSize,
			FlushInterval: time.Duration(cfg.Services.EventRouter.Tinybird.FlushInterval) * time.Second,
			Tinybird:      tinybird.New("https://api.tinybird.co", cfg.Services.EventRouter.Tinybird.Token),
			AuthToken:     cfg.Services.EventRouter.AuthToken,
		})
		if err != nil {
			return err
		}
		srv.WithEventRouter(er)
		if err != nil {
			return fmt.Errorf("failed to add event router service: %w", err)

		}
	}

	var clus cluster.Cluster

	if cfg.Cluster != nil {

		memb, err := membership.New(membership.Config{
			NodeId:   cfg.NodeId,
			RpcAddr:  cfg.Cluster.RpcAddr,
			SerfAddr: cfg.Cluster.SerfAddr,
			Logger:   logger,
		})
		if err != nil {
			return fmt.Errorf("failed to create membership: %w", err)
		}

		var join []string
		if cfg.Cluster.Join.Dns != nil {
			addrs, err := net.LookupHost(cfg.Cluster.Join.Dns.AAAA)
			if err != nil {
				return fmt.Errorf("failed to lookup dns: %w", err)
			}
			logger.Info().Strs("addrs", addrs).Msg("found dns records")
			join = addrs
		} else if cfg.Cluster.Join.Env != nil {
			join = cfg.Cluster.Join.Env.Addrs
		}

		_, err = memb.Join(join...)
		if err != nil {
			return fault.Wrap(err, fmsg.With("failed to join cluster"))
		}
		defer func() {
			logger.Info().Msg("leaving membership")
			err = memb.Leave()
			if err != nil {
				logger.Error().Err(err).Msg("failed to leave cluster")
			}
		}()

		clus, err = cluster.New(cluster.Config{
			NodeId:     cfg.NodeId,
			RpcAddr:    cfg.Cluster.RpcAddr,
			Membership: memb,
			Logger:     logger,
			Metrics:    m,
			Debug:      true,
			AuthToken:  cfg.Cluster.AuthToken,
		})
		if err != nil {
			return fmt.Errorf("failed to create cluster: %w", err)
		}
		defer func() {
			err := clus.Shutdown()
			if err != nil {
				logger.Error().Err(err).Msg("failed to shutdown cluster")
			}
		}()

		err = connectSrv.AddService(connect.NewClusterServer(clus, logger))
		if err != nil {
			return fmt.Errorf("failed to add cluster service: %w", err)

		}
	}

	if cfg.Services.Ratelimit != nil {
		rl, err := ratelimit.New(ratelimit.Config{
			Logger:  logger,
			Metrics: m,
			Cluster: clus,
		})
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to create service")
		}

		srv.Ratelimit = ratelimit.WithTracing(rl)

		v1RatelimitRatelimit.Register(srv.HumaAPI(), srv.Services(), srv.BearerAuthFromSecret(cfg.Services.Ratelimit.AuthToken))
		v1RatelimitMultiRatelimit.Register(srv.HumaAPI(), srv.Services(), srv.BearerAuthFromSecret(cfg.Services.Ratelimit.AuthToken))
		v1RatelimitCommitLease.Register(srv.HumaAPI(), srv.Services(), srv.BearerAuthFromSecret(cfg.Services.Ratelimit.AuthToken))

		err = connectSrv.AddService(connect.NewRatelimitServer(rl, logger, cfg.Services.Ratelimit.AuthToken))
		if err != nil {
			return fmt.Errorf("failed to add ratelimit service: %w", err)
		}
		logger.Info().Msg("started ratelimit service")
	}

	if cfg.Pprof != nil {
		connectSrv.EnablePprof(cfg.Pprof.Username, cfg.Pprof.Password)
	}

	go func() {
		err := connectSrv.Listen(fmt.Sprintf(":%s", cfg.RpcPort))
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to start connect service")
		}
	}()

	go func() {
		logger.Info().Msgf("listening on port %s", cfg.Port)
		err := srv.Listen(fmt.Sprintf(":%s", cfg.Port))
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to start service")
		}
	}()

	if cfg.Prometheus != nil {
		go func() {
			err := prometheus.Listen(cfg.Prometheus.Path, cfg.Prometheus.Port)
			if err != nil {
				logger.Fatal().Err(err).Msg("failed to start prometheus")
			}
		}()
	}

	cShutdown := make(chan os.Signal, 1)
	signal.Notify(cShutdown, os.Interrupt, syscall.SIGTERM)

	<-cShutdown
	logger.Info().Msg("shutting down")

	err = connectSrv.Shutdown()
	if err != nil {
		return fmt.Errorf("failed to shutdown connect service: %w", err)
	}
	err = srv.Shutdown()
	if err != nil {
		return fmt.Errorf("failed to shutdown service: %w", err)
	}
	err = clus.Shutdown()
	if err != nil {
		return fmt.Errorf("failed to shutdown cluster: %w", err)
	}

	return nil
}

// TODO: generating this every time is a bit stupid, we should make this its own command
//
//	and then run it as part of the build process
func init() {
	_, err := config.GenerateJsonSchema(config.Agent{}, "schema.json")
	if err != nil {
		panic(err)
	}
}
