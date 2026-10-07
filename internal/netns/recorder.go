package netns

import (
	"fmt"
	"strings"
	"sync"
)

// Recorder is a Runner that logs commands instead of executing them: dry-run
// mode lets the full stack run unprivileged and makes rule programming
// unit-testable.
type Recorder struct {
	mu   sync.Mutex
	log  []string
	fail map[string]bool
}

func NewRecorder() *Recorder {
	return &Recorder{fail: map[string]bool{}}
}

func (r *Recorder) Run(name string, args ...string) error {
	cmd := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail[cmd] {
		return fmt.Errorf("injected failure: %s", cmd)
	}
	r.log = append(r.log, cmd)
	return nil
}

func (r *Recorder) Output(name string, args ...string) ([]byte, error) { return nil, nil }

func (r *Recorder) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

func (r *Recorder) FailOn(cmd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail[cmd] = true
}
