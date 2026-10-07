// Package logging provides the structured logger used across noded and the
// control plane. Zero deps: JSON lines to stderr.
package logging

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return Debug
	case "warn":
		return Warn
	case "error":
		return Error
	default:
		return Info
	}
}

func (l Level) String() string {
	switch l {
	case Debug:
		return "debug"
	case Warn:
		return "warn"
	case Error:
		return "error"
	default:
		return "info"
	}
}

type Logger struct {
	mu     sync.Mutex
	min    Level
	fields map[string]any
	out    *os.File
}

func New(level string) *Logger {
	return &Logger{min: ParseLevel(level), fields: map[string]any{}, out: os.Stderr}
}

func (l *Logger) With(kv ...any) *Logger {
	n := &Logger{min: l.min, out: l.out, fields: map[string]any{}}
	for k, v := range l.fields {
		n.fields[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			n.fields[k] = kv[i+1]
		}
	}
	return n
}

func (l *Logger) log(lv Level, msg string, kv ...any) {
	if lv < l.min {
		return
	}
	rec := map[string]any{
		"ts":  time.Now().UTC().Format(time.RFC3339Nano),
		"lvl": lv.String(),
		"msg": msg,
	}
	for k, v := range l.fields {
		rec[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			rec[k] = kv[i+1]
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out.Write(append(b, '\n'))
}

func (l *Logger) Debug(msg string, kv ...any) { l.log(Debug, msg, kv...) }
func (l *Logger) Info(msg string, kv ...any)  { l.log(Info, msg, kv...) }
func (l *Logger) Warn(msg string, kv ...any)  { l.log(Warn, msg, kv...) }
func (l *Logger) Error(msg string, kv ...any) { l.log(Error, msg, kv...) }
