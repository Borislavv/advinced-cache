package main

import (
	"context"
	"github.com/Borislavv/advanced-cache/internal/cache"
	"github.com/Borislavv/advanced-cache/internal/cache/config"
	config2 "github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/k8s/probe/liveness"
	"github.com/Borislavv/advanced-cache/pkg/shutdown"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
	"go.uber.org/automaxprocs/maxprocs"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"time"
)

// Initializes environment variables from .env files and binds them using Viper.
// This allows overriding any value via environment variables.
func init() {
	go func() {
		http.ListenAndServe("localhost:6060", nil)
	}()

	// Load .env and .env.local files for configuration overrides.
	if err := godotenv.Overload(".env", ".env.local"); err != nil {
		panic(err)
	}

	// Bind all relevant environment variables using Viper.
	viper.AutomaticEnv()
	_ = viper.BindEnv("CACHE_CONFIG_PATH")
	_ = viper.BindEnv("FASTHTTP_SERVER_NAME")
	_ = viper.BindEnv("FASTHTTP_SERVER_PORT")
	_ = viper.BindEnv("FASTHTTP_SERVER_SHUTDOWN_TIMEOUT")
	_ = viper.BindEnv("FASTHTTP_SERVER_REQUEST_TIMEOUT")
	_ = viper.BindEnv("LIVENESS_PROBE_TIMEOUT")
	_ = viper.BindEnv("IS_PROMETHEUS_METRICS_ENABLED")
}

// setMaxProcs automatically sets the optimal GOMAXPROCS value (CPU parallelism)
// based on the available CPUs and cgroup/docker CPU quotas (uses automaxprocs).
func setMaxProcs() {
	if _, err := maxprocs.Set(); err != nil {
		log.Err(err).Msg("[main] setting up GOMAXPROCS value failed")
		panic(err)
	}
	log.Info().Msgf("[main] optimized GOMAXPROCS=%d was set up", runtime.GOMAXPROCS(0))
}

// loadCfg loads the configuration struct from environment variables
// and computes any derived configuration values.
func loadCfg() *config.Config {
	cfg := &config.Config{}
	if err := viper.Unmarshal(cfg); err != nil {
		log.Err(err).Msg("[main] failed to unmarshal config from envs")
		panic(err)
	}

	type configPath struct {
		ConfigPath string `envconfig:"IS_PROMETHEUS_METRICS_ENABLED" mapstructure:"CACHE_CONFIG_PATH" default:"/config/config.dev.yaml"`
	}

	cfgPath := &configPath{}
	if err := viper.Unmarshal(cfgPath); err != nil {
		log.Err(err).Msg("[main] failed to unmarshal config from envs")
		panic(err)
	}

	cacheConfig, err := config2.LoadConfig(cfgPath.ConfigPath)
	if err != nil {
		log.Err(err).Msg("[main] failed to load config from envs")
		panic(err)
	}
	cfg.Cache = cacheConfig

	return cfg
}

// Main entrypoint: configures and starts the cache application.
func main() {
	// Create a root context for gracefulShutdown shutdown and cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Optimize GOMAXPROCS for the current environment.
	setMaxProcs()

	// Load the application configuration from env vars.
	cfg := loadCfg()

	// Setup gracefulShutdown shutdown handler (SIGTERM, SIGINT, etc).
	gracefulShutdown := shutdown.NewGraceful(ctx, cancel)
	gracefulShutdown.SetGracefulTimeout(time.Second * 300) // 9.0s

	// Initialize liveness probe for Kubernetes/Cloud health checks.
	probe := liveness.NewProbe(cfg.LivenessTimeout())

	// Initialize and start the cache application.
	if app, err := cache.NewApp(ctx, cfg, probe); err != nil {
		log.Err(err).Msg("[main] failed to init cache app")
	} else {
		// Register app for gracefulShutdown shutdown.
		gracefulShutdown.Add(1)
		go app.Start(gracefulShutdown)
	}

	// Listen for OS signals or context cancellation and wait for gracefulShutdown shutdown.
	if err := gracefulShutdown.ListenCancelAndAwait(); err != nil {
		log.Err(err).Msg("failed to gracefully shut down service")
	}
}
