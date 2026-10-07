// scan.go holds the key-walking helper.
//
// Redis has no index of key sizes or access counts, so every question about
// "which key is biggest" or "which key is hottest" is answered by visiting
// keys. That makes the sampling policy part of the answer rather than an
// implementation detail — which is why scanKeys returns how many keys it
// visited, and why every caller reports that number instead of presenting a
// sample as a ranking.
package redis

import (
	"context"
	"fmt"
)

// scanKeys walks the keyspace with SCAN and returns up to limit keys along
// with the number of keys visited.
//
// SCAN is used rather than KEYS on purpose. KEYS blocks the server for the
// duration of a full keyspace walk, and this adapter is called precisely
// when the server is already the thing in trouble; a diagnostic that
// freezes production to produce a report is a second incident.
//
// The visited count is returned even when the limit was not reached, because
// "I looked at 40 keys" and "I looked at everything" support different
// conclusions, and only the caller knows which one it needs.
func (a *Adapter) scanKeys(ctx context.Context, pattern string, limit int) ([]string, int, error) {
	client, err := a.handle()
	if err != nil {
		return nil, 0, err
	}
	if pattern == "" {
		pattern = "*"
	}
	if limit <= 0 {
		limit = defaultScanLimit
	}
	if limit > maxScanLimit {
		limit = maxScanLimit
	}

	const batch = 500
	keys := make([]string, 0, min(limit, batch))
	visited := 0
	var cursor uint64
	for {
		page, next, err := client.Scan(ctx, cursor, pattern, batch).Result()
		if err != nil {
			return nil, visited, fmt.Errorf("redis: SCAN: %w", err)
		}
		visited += len(page)
		keys = append(keys, page...)
		if len(keys) >= limit {
			return keys[:limit], visited, nil
		}
		cursor = next
		if cursor == 0 {
			// A full pass. Redis guarantees SCAN terminates, so reaching
			// cursor 0 means the keyspace was walked to whatever extent
			// the server considers complete.
			return keys, visited, nil
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
