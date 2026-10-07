package api

import (
	"context"
	"encoding/json"
	"time"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func sleepMS(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
