// Package miniredisx runs an in-process Redis so OpsKeeper's embedded mode
// needs no Redis container. The server speaks real TCP on a loopback port,
// so the ordinary go-redis client connects to it unchanged — the callers in
// cmd/opskeeper keep the same client, only the address differs.
package miniredisx

import (
	"fmt"

	"github.com/alicebob/miniredis/v2"
)

// Server is a running in-process Redis.
type Server struct {
	mr *miniredis.Miniredis
}

// Start launches the server on a free loopback port.
func Start() (*Server, error) {
	mr, err := miniredis.Run()
	if err != nil {
		return nil, fmt.Errorf("miniredisx: start in-process redis: %w", err)
	}
	return &Server{mr: mr}, nil
}

// Addr is the host:port to hand to redis.NewClient.
func (s *Server) Addr() string { return s.mr.Addr() }

// Close shuts the server down.
func (s *Server) Close() { s.mr.Close() }