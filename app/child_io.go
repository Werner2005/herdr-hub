package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Werner2005/herdr-hub/internal/workload"
	"github.com/coder/websocket"
)

type childSessionLimiter struct {
	once  sync.Once
	slots chan struct{}
}

func (s *childSessionLimiter) acquire() bool {
	s.once.Do(func() { s.slots = make(chan struct{}, 4) })
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *application) childIO(w http.ResponseWriter, r *http.Request, c childRecord, action string, input workload.ExecRequest) {
	if !a.childSessions.acquire() {
		writeError(w, 429, "too many concurrent workload I/O sessions")
		return
	}
	defer func() { <-a.childSessions.slots }()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Recheck the immutable resource identity immediately before accessing I/O.
	pod, _, err := a.kube.call(ctx, "GET", "pods", childResource(c.ID), nil)
	if err != nil || !childOwned(pod, c.ID, c.Parent) {
		writeError(w, 409, "workload Pod unavailable")
		return
	}
	if action == "exec" {
		result, err := a.kube.execChild(ctx, c, input)
		if err != nil {
			writeError(w, 502, "workload exec failed, timed out or exceeded output bounds")
			return
		}
		writeJSON(w, 200, result)
		return
	}
	req, err := a.kube.childRequest(ctx, "GET", childResource(c.ID)+"/log")
	if err != nil {
		writeError(w, 502, "workload logs unavailable")
		return
	}
	q := url.Values{"container": {"workload"}, "limitBytes": {"1048576"}, "tailLines": {"10000"}}
	req.URL.RawQuery = q.Encode()
	client := a.kube.childHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, 502, "workload logs unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeError(w, 502, "workload logs unavailable")
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		writeError(w, 502, "workload logs exceeded output bounds")
		return
	}
	writeJSON(w, 200, object{"logs": string(data)})
}

func (k *kubernetes) childHTTPClient() *http.Client {
	client := *k.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (k *kubernetes) childRequest(ctx context.Context, method, resource string) (*http.Request, error) {
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, k.endpoint("pods", resource), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	return req, nil
}

func (k *kubernetes) execChild(ctx context.Context, c childRecord, input workload.ExecRequest) (workload.ExecResult, error) {
	result := workload.ExecResult{}
	req, err := k.childRequest(ctx, "GET", childResource(c.ID)+"/exec")
	if err != nil {
		return result, err
	}
	q := url.Values{"container": {"workload"}, "stdout": {"true"}, "stderr": {"true"}, "stdin": {"false"}, "tty": {"false"}, "command": input.Command}
	req.URL.RawQuery = q.Encode()
	conn, resp, err := websocket.Dial(ctx, req.URL.String(), &websocket.DialOptions{
		HTTPClient: k.childHTTPClient(), HTTPHeader: req.Header, Subprotocols: []string{"v4.channel.k8s.io"},
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return result, fmt.Errorf("Kubernetes exec upgrade failed")
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != "v4.channel.k8s.io" {
		return result, fmt.Errorf("unsupported Kubernetes exec protocol")
	}
	conn.SetReadLimit(1 << 20)
	total := 0
	var stdout, stderr strings.Builder
	for {
		kind, message, err := conn.Read(ctx)
		if err != nil {
			return result, fmt.Errorf("exec ended without a valid terminal status")
		}
		if kind != websocket.MessageBinary || len(message) == 0 {
			return result, fmt.Errorf("invalid exec frame")
		}
		total += len(message)
		if total > 1<<20 {
			return result, fmt.Errorf("exec output exceeds 1 MiB")
		}
		switch message[0] {
		case 1:
			_, _ = stdout.Write(message[1:])
		case 2:
			_, _ = stderr.Write(message[1:])
		case 3:
			result.Stdout, result.Stderr = stdout.String(), stderr.String()
			var status struct {
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Details struct {
					Causes []struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"causes"`
				} `json:"details"`
			}
			if err := json.Unmarshal(message[1:], &status); err != nil {
				return result, fmt.Errorf("invalid exec status")
			}
			if status.Status == "Success" {
				return result, nil
			}
			if status.Status == "Failure" && status.Reason == "NonZeroExitCode" {
				for _, cause := range status.Details.Causes {
					if cause.Reason == "ExitCode" {
						n, err := strconv.Atoi(cause.Message)
						if err == nil && n > 0 && n <= 255 {
							result.ExitCode = n
							return result, nil
						}
					}
				}
			}
			return result, fmt.Errorf("exec failed without a valid exit code")
		default:
			return result, fmt.Errorf("unsupported exec channel")
		}
	}
}
