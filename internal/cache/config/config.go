package config

import (
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/k8s/probe/liveness"
	prometheusconifg "github.com/Borislavv/advanced-cache/pkg/prometheus/metrics/config"
	fasthttpconfig "github.com/Borislavv/advanced-cache/pkg/server/config"
)

type Config struct {
	*config.Cache
	prometheusconifg.Metrics `mapstructure:",squash"`
	fasthttpconfig.Server    `mapstructure:",squash"`
	liveness.Config          `mapstructure:",squash"`
}
