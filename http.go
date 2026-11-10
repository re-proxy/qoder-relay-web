package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// httpResult 是 JSON 请求的统一返回。
type httpResult struct {
	Status int
	OK     bool
	JSON   any
	Text   string
}

// errText 返回适合展示在错误信息里的响应片段。
func (r httpResult) errText() string {
	t := strings.TrimSpace(r.Text)
	if t == "" && r.JSON != nil {
		if b, err := json.Marshal(r.JSON); err == nil {
			t = string(b)
		}
	}
	return truncateRunes(t, 300)
}

// doJSON 发起 JSON 请求（body 为 nil 时无请求体）。
func doJSON(ctx context.Context, method, url string, headers map[string]string, body any, timeout time.Duration) (httpResult, error) {
	var reader io.Reader
	rawBody := ""
	if body != nil {
		switch t := body.(type) {
		case string:
			rawBody = t
		case []byte:
			rawBody = string(t)
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return httpResult{}, err
			}
			rawBody = string(b)
		}
		reader = strings.NewReader(rawBody)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return httpResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return httpResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return httpResult{}, err
	}
	out := httpResult{Status: resp.StatusCode, OK: resp.StatusCode >= 200 && resp.StatusCode < 300, Text: string(raw)}
	if len(bytes.TrimSpace(raw)) > 0 {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			out.JSON = v
		}
	}
	return out, nil
}

// sseStream 是逐行读取的 SSE 流。
type sseStream struct {
	resp    *http.Response
	reader  *bufio.Reader
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	idle    bool
	closed  bool
}

var errIdleTimeout = errors.New("上游空闲超时")

// openSSE 建立 SSE 连接。idleTimeout 按「多久没收到数据」计时。
func openSSE(ctx context.Context, method, url string, headers map[string]string, body any, idleTimeout time.Duration) (*sseStream, error) {
	var reader io.Reader
	if body != nil {
		switch t := body.(type) {
		case string:
			reader = strings.NewReader(t)
		case []byte:
			reader = bytes.NewReader(t)
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return nil, err
			}
			reader = bytes.NewReader(b)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("accept") == "" {
		req.Header.Set("accept", "text/event-stream")
	}
	if req.Header.Get("cache-control") == "" {
		req.Header.Set("cache-control", "no-cache")
	}
	req.Header.Set("accept-encoding", "identity")

	s := &sseStream{cancel: cancel, timeout: idleTimeout}
	if idleTimeout > 0 {
		s.arm()
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		s.stop()
		if s.idle {
			return nil, errIdleTimeout
		}
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Body == nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		s.stop()
		return nil, fmt.Errorf("上游 HTTP %d: %s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 500))
	}
	s.resp = resp
	s.reader = bufio.NewReaderSize(resp.Body, 32<<10)
	return s, nil
}

func (s *sseStream) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timeout <= 0 || s.closed {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(s.timeout, func() {
		s.mu.Lock()
		s.idle = true
		s.mu.Unlock()
		s.cancel()
	})
}

func (s *sseStream) stop() {
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.cancel()
	if s.resp != nil && s.resp.Body != nil {
		s.resp.Body.Close()
	}
}

// NextLine 返回下一行（不含换行符）；流结束时返回 ("", false)。
func (s *sseStream) NextLine() (string, bool, error) {
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if len(line) > 0 {
				s.arm()
				return strings.TrimRight(line, "\r\n"), true, nil
			}
			s.mu.Lock()
			idle := s.idle
			s.mu.Unlock()
			if idle {
				return "", false, errIdleTimeout
			}
			if errors.Is(err, io.EOF) {
				return "", false, nil
			}
			return "", false, err
		}
		s.arm()
		return strings.TrimRight(line, "\r\n"), true, nil
	}
}

// Close 关闭流。
func (s *sseStream) Close() { s.stop() }

// sseData 从 "data: xxx" 行中取出内容，非 data 行返回 ("",false)。
func sseData(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "data:") {
		return "", false
	}
	return strings.TrimSpace(t[len("data:"):]), true
}

// sseEvent 从 "event: xxx" 行中取出事件名。
func sseEvent(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "event:") {
		return "", false
	}
	return strings.TrimSpace(t[len("event:"):]), true
}
