package agent

import (
	"context"
	"fmt"
	"log"
	"os"

	"tailscale.com/tsnet"
)

// tsnetNode brings up a worker's own tailnet node in dir.
func tsnetNode(ctx context.Context, hostname, dir, authKey string) (Node, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	srv := &tsnet.Server{
		Dir:      dir,
		Hostname: hostname,
		AuthKey:  authKey,
		Logf:     func(string, ...any) {},
		UserLogf: log.Printf,
	}
	if _, err := srv.Up(ctx); err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("starting tailnet node %s: %w", hostname, err)
	}
	return srv, nil
}
