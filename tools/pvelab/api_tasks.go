package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type TaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

type taskLine struct {
	N int    `json:"n"`
	T string `json:"t"`
}

// upidNode returns the node a task runs on: UPID:node:pid:pstart:...
func upidNode(upid string) (string, error) {
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[0] != "UPID" || parts[1] == "" {
		return "", fmt.Errorf("malformed task ID %q", upid)
	}
	return parts[1], nil
}

// taskOutcome turns a stopped task's status into an error. Proxmox reports
// "OK", or "WARNINGS: n" for a task that finished with warnings, or the
// failure message.
func taskOutcome(s TaskStatus) (done bool, err error) {
	if s.Status != "stopped" {
		return false, nil
	}
	if s.ExitStatus == "OK" || strings.HasPrefix(s.ExitStatus, "WARNINGS") {
		return true, nil
	}
	if s.ExitStatus == "" {
		return true, fmt.Errorf("task stopped without an exit status")
	}
	return true, fmt.Errorf("task failed: %s", s.ExitStatus)
}

func (a *API) TaskStatus(ctx context.Context, upid string) (TaskStatus, error) {
	node, err := upidNode(upid)
	if err != nil {
		return TaskStatus{}, err
	}
	var s TaskStatus
	err = a.call(ctx, http.MethodGet, "/nodes/"+esc(node)+"/tasks/"+esc(upid)+"/status", nil, &s)
	return s, err
}

// TaskLogTail returns the last n lines of a task's log.
func (a *API) TaskLogTail(ctx context.Context, upid string, n int) ([]string, error) {
	node, err := upidNode(upid)
	if err != nil {
		return nil, err
	}
	var lines []taskLine
	form := url.Values{"start": {"0"}, "limit": {"5000"}}
	if err := a.call(ctx, http.MethodGet, "/nodes/"+esc(node)+"/tasks/"+esc(upid)+"/log", form, &lines); err != nil {
		return nil, err
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = redact(l.T, a.secrets...)
	}
	return out, nil
}

// WaitTask polls a task until it stops and requires success. A failed task's
// error carries the end of its log. An empty upid is a call that finished
// synchronously.
func (a *API) WaitTask(ctx context.Context, upid string) error {
	if upid == "" {
		return nil
	}
	a.logf("task %s", upid)
	interval := 250 * time.Millisecond
	for {
		s, err := a.TaskStatus(ctx, upid)
		if err != nil {
			return err
		}
		done, taskErr := taskOutcome(s)
		if done {
			if taskErr == nil {
				return nil
			}
			logCtx, cancel := context.WithTimeout(context.Background(), apiRequestTimeout)
			defer cancel()
			if tail, err := a.TaskLogTail(logCtx, upid, 15); err == nil && len(tail) > 0 {
				return fmt.Errorf("%w\n    %s", taskErr, strings.Join(tail, "\n    "))
			}
			return taskErr
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for task %s: %w", upid, ctx.Err())
		case <-time.After(interval):
		}
		if interval < 2*time.Second {
			interval *= 2
		}
	}
}
