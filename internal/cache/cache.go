package cache

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Optional Redis invalidation for content-node/player-node. Missing or
// unavailable Redis is fail-open; Cloudflare is still purged separately.
var client *redis.Client

func Init(redisURL string) {
	if redisURL == "" {
		log.Println("📦 Redis invalidation disabled (no REDIS_URL)")
		return
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("⚠️ REDIS_URL invalid — redis invalidation disabled: %v", err)
		return
	}
	c := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		log.Printf("⚠️ Redis unreachable — redis invalidation disabled: %v", err)
		return
	}
	client = c
	log.Printf("📦 Redis invalidation enabled: %s", opt.Addr)
}

func Del(ctx context.Context, keys ...string) {
	if client == nil || len(keys) == 0 {
		return
	}
	delCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := client.Del(delCtx, keys...).Err(); err != nil {
		log.Printf("⚠️ Redis DEL failed (ignored): %v", err)
		return
	}
	log.Printf("🧹 Redis DEL %d key(s)", len(keys))
}
