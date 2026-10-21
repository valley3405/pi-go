package llm

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type providerHTTPConfig struct {
	baseURL string
	client  *http.Client
	retry   RetryPolicy
	limiter RequestLimiter
}

func newProviderHTTPConfig(options []ProviderOption) providerHTTPConfig {
	cfg := providerHTTPConfig{
		client: &http.Client{},
		retry:  RetryPolicy{MaxRetries: 2},
	}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	if cfg.retry.MaxRetries < 0 {
		cfg.retry.MaxRetries = 0
	}
	if cfg.retry.InitialDelay <= 0 {
		cfg.retry.InitialDelay = 250 * time.Millisecond
	}
	if cfg.retry.MaxDelay <= 0 {
		cfg.retry.MaxDelay = 4 * time.Second
	}
	return cfg
}

// do 仅负责 HTTP 握手和错误响应。成功响应一旦返回，调用者消费 SSE，
// 其后的 EOF、超时、流内错误都不会再进入重试循环。
func (cfg providerHTTPConfig) do(req *http.Request, provider string) (*http.Response, error) {
	ctx := req.Context()
	defer req.Body.Close()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if cfg.limiter != nil {
			if err := cfg.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 每次构造独立 body，确保 POST 重试不会发送已读完的内容。
		attemptReq := req.Clone(ctx)
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		attemptReq.Body = body
		resp, err := cfg.client.Do(attemptReq)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return nil, ctx.Err()
		}

		retryable := retryableNetworkError(err)
		retryAfter := ""
		if err == nil {
			retryable = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 && resp.StatusCode <= 599
			retryAfter = resp.Header.Get("Retry-After")
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			err = fmt.Errorf("%s: HTTP %d: %s", provider, resp.StatusCode, string(b))
		}
		if !retryable || attempt >= cfg.retry.MaxRetries {
			return nil, err
		}
		delay, ok := cfg.retry.delay(attempt, retryAfter, time.Now())
		if !ok {
			// 不提前重试，也不因 Retry-After 无上限地等待。
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func retryableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// delay 指数增长至 MaxDelay，并使用 [delay/2, delay] 抖动。
// Retry-After（秒或 HTTP 日期）是最短等待时间；超过 MaxDelay 时放弃重试。
func (p RetryPolicy) delay(attempt int, retryAfter string, now time.Time) (time.Duration, bool) {
	delay := min(p.InitialDelay, p.MaxDelay)
	for i := 0; i < attempt && delay < p.MaxDelay; i++ {
		if delay > p.MaxDelay/2 {
			delay = p.MaxDelay
		} else {
			delay *= 2
		}
	}
	delay = delay/2 + time.Duration(rand.Int64N(int64(delay-delay/2)+1))
	if serverDelay, ok := parseRetryAfter(retryAfter, now); ok {
		if serverDelay > p.MaxDelay {
			return 0, false
		}
		delay = max(delay, serverDelay)
	}
	return delay, true
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		const maxDuration = time.Duration(1<<63 - 1)
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(time.Duration(0), date.Sub(now)), true
	}
	return 0, false
}
