package rules

import (
	"bytes"
	"github.com/Borislavv/advanced-cache/pkg/config"
)

func Match(cfg *config.Cache, path []byte) *config.Rule {
	for _, rule := range cfg.Cache.Rules {
		if bytes.HasPrefix(path, rule.PathBytes) {
			return rule
		}
	}
	return nil
}
