package webshell

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = cli.Close()
		mr.Close()
	})
	return cli, mr
}

type redisClient = redis.Client

func redisClientAt(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr})
}
