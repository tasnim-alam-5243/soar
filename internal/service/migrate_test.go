package service

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/yourorg/soar/internal/config"
)

func TestMigrateFailsFastWithoutRetentionFile(t *testing.T) {
	cfg := config.Config{OSURLs: []string{"http://127.0.0.1:1"}, IndexPrefix: "soar", RetentionFile: "/nonexistent/retention.yaml"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, fn := range map[string]func(context.Context, config.Config, *slog.Logger) error{"apply": Migrate, "check": MigrateCheck} {
		err := fn(context.Background(), cfg, log)
		if err == nil || !strings.Contains(err.Error(), "retention") {
			t.Errorf("%s: want a retention error before touching OpenSearch, got %v", name, err)
		}
	}
}
