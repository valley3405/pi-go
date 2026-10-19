package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var networkFixtures = []struct {
	name, path, success, partial string
	wantUsage                    Usage
}{
	{
		name: "openai", path: "/chat/completions",
		success: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n" +
			"data: [DONE]\n\n",
		partial:   "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		wantUsage: Usage{Input: 100, Output: 20, CacheRead: 40, TotalTokens: 120},
	},
	{
		name: "anthropic", path: "/v1/messages",
		success: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":50,\"cache_read_input_tokens\":40,\"cache_creation_input_tokens\":10}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
		partial: "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
		wantUsage: Usage{Input: 100, Output: 20, CacheRead: 40, CacheWrite: 10, TotalTokens: 120},
	},
}

func networkProvider(name, endpoint string, options ...ProviderOption) Provider {
	models := []Model{{ID: "test", MaxTokens: 100}}
	if name == "openai" {
		return NewOpenAIProvider(name, endpoint, models, options...)
	}
	p := NewAnthropicProvider(name, models, options...)
	p.baseURL = endpoint
	return p
}

func fastRetry(retries int) ProviderOption {
	return WithRetryPolicy(RetryPolicy{MaxRetries: retries, InitialDelay: time.Millisecond, MaxDelay: 4 * time.Millisecond})
}

func collectNetworkStream(ctx context.Context, p Provider, input Context, opts StreamOptions) (Message, []Event, error) {
	s := p.Stream(ctx, p.Models()[0], input, opts)
	var events []Event
	for event := range s.Events() {
		events = append(events, event)
	}
	msg, err := s.Result()
	return msg, events, err
}

func TestProvidersRetryHTTPStatus(t *testing.T) {
	for _, fixture := range networkFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			for _, status := range []int{429, 500, 502, 503, 504, 529} {
				t.Run(fmt.Sprint(status), func(t *testing.T) {
					var requests atomic.Int32
					var bodiesMu sync.Mutex
					var bodies []string
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						b, _ := io.ReadAll(r.Body)
						bodiesMu.Lock()
						bodies = append(bodies, string(b))
						bodiesMu.Unlock()
						if r.URL.Path != fixture.path || r.Method != http.MethodPost {
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						}
						if r.Header.Get("X-Audit") != "kept" ||
							(fixture.name == "openai" && r.Header.Get("Authorization") != "Bearer dynamic-key") ||
							(fixture.name == "anthropic" && r.Header.Get("x-api-key") != "dynamic-key") {
							t.Error("headers were not preserved")
						}
						if requests.Add(1) <= 2 {
							w.Header().Set("Retry-After", "0")
							http.Error(w, "transient", status)
							return
						}
						fmt.Fprint(w, fixture.success)
					}))
					defer server.Close()
					p := networkProvider(fixture.name, server.URL, fastRetry(2))
					input := Context{Messages: []Message{{Role: RoleUser, Content: []Content{
						&TextContent{Text: "hello"}, &ImageContent{MimeType: "image/png", Data: "aW1hZ2U="},
					}}}}
					msg, events, err := collectNetworkStream(context.Background(), p, input, StreamOptions{
						APIKey: "dynamic-key", Headers: map[string]string{"X-Audit": "kept"},
					})
					if err != nil || msg.Text() != "hi" || msg.Usage != fixture.wantUsage {
						t.Fatalf("result=%+v, err=%v", msg, err)
					}
					bodiesMu.Lock()
					defer bodiesMu.Unlock()
					if requests.Load() != 3 || len(bodies) != 3 || bodies[0] != bodies[1] || bodies[0] != bodies[2] || !strings.Contains(bodies[0], "aW1hZ2U=") {
						t.Fatalf("POST bodies changed or image lost: %v", bodies)
					}
					starts := 0
					for _, ev := range events {
						if ev.Type == EventStart {
							starts++
						}
					}
					if starts != 1 {
						t.Fatalf("retry emitted %d stream starts", starts)
					}
				})
			}
		})
	}
}

func TestProvidersRetryLimits(t *testing.T) {
	for _, fixture := range networkFixtures {
		for _, test := range []struct {
			name                      string
			status, retries, attempts int
		}{
			{"bounded", 503, 2, 3}, {"disabled", 503, 0, 1}, {"negative disables", 503, -1, 1},
			{"bad request", 400, 2, 1}, {"unauthorized", 401, 2, 1}, {"forbidden", 403, 2, 1},
			{"context overflow", 413, 2, 1},
		} {
			t.Run(fixture.name+"/"+test.name, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					http.Error(w, "failure", test.status)
				}))
				defer server.Close()
				p := networkProvider(fixture.name, server.URL, fastRetry(test.retries))
				msg, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{})
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", test.status)) || msg.StopReason != StopError || int(requests.Load()) != test.attempts {
					t.Fatalf("attempts=%d result=%+v err=%v", requests.Load(), msg, err)
				}
			})
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProvidersRetryNetworkErrors(t *testing.T) {
	permanent := errors.New("permanent transport configuration error")
	for _, fixture := range networkFixtures {
		for _, test := range []struct {
			name     string
			err      error
			attempts int
		}{
			{"reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 2},
			{"timeout", &net.DNSError{IsTimeout: true}, 2},
			{"header EOF", io.EOF, 2},
			{"permanent", permanent, 1},
		} {
			t.Run(fixture.name+"/"+test.name, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, fixture.success) }))
				defer server.Close()
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if requests.Add(1) == 1 {
						r.Body.Close()
						return nil, test.err
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
				p := networkProvider(fixture.name, server.URL, WithHTTPClient(client), fastRetry(2))
				msg, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{})
				if int(requests.Load()) != test.attempts {
					t.Fatalf("requests=%d", requests.Load())
				}
				if test.attempts == 2 && (err != nil || msg.Text() != "hi") {
					t.Fatalf("result=%+v err=%v", msg, err)
				}
				if test.attempts == 1 && !errors.Is(err, permanent) {
					t.Fatalf("err=%v", err)
				}
			})
		}
	}
}

func TestProvidersNeverReplaySSE(t *testing.T) {
	for _, fixture := range networkFixtures {
		for _, mode := range []string{"clean EOF", "broken connection", "stream error", "empty success", "cancel"} {
			t.Run(fixture.name+"/"+mode, func(t *testing.T) {
				var requests atomic.Int32
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if mode == "empty success" {
						return
					}
					if mode == "broken connection" {
						conn, rw, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						// Deliberately shorter than Content-Length, so the reader observes unexpected EOF.
						fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(fixture.partial)+100, fixture.partial)
						rw.Flush()
						return
					}
					fmt.Fprint(w, fixture.partial)
					w.(http.Flusher).Flush()
					if mode == "stream error" {
						fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n")
					}
					if mode == "cancel" {
						cancel()
						<-r.Context().Done()
					}
				}))
				defer server.Close()
				p := networkProvider(fixture.name, server.URL, fastRetry(2))
				msg, events, err := collectNetworkStream(ctx, p, Context{}, StreamOptions{})
				if err == nil || msg.StopReason != StopError || requests.Load() != 1 {
					t.Fatalf("requests=%d result=%+v err=%v", requests.Load(), msg, err)
				}
				for _, event := range events {
					if event.Type == EventDone {
						t.Fatal("incomplete stream emitted done")
					}
				}
				if (mode == "clean EOF" || mode == "broken connection" || mode == "empty success") && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("err=%v", err)
				}
			})
		}
	}
}

func TestRetryDelay(t *testing.T) {
	p := RetryPolicy{InitialDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	now := time.Now().UTC().Truncate(time.Second)
	for attempt, bound := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second} {
		for i := 0; i < 10; i++ {
			got, ok := p.delay(attempt, "", now)
			if !ok || got < bound/2 || got > bound {
				t.Fatalf("attempt=%d delay=%v bound=%v", attempt, got, bound)
			}
		}
	}
	for _, header := range []string{"1", now.Add(time.Second).Format(http.TimeFormat)} {
		if got, ok := p.delay(0, header, now); !ok || got != time.Second {
			t.Fatalf("Retry-After=%q delay=%v ok=%v", header, got, ok)
		}
	}
	for _, header := range []string{"2", "9223372036854775807", now.Add(2 * time.Second).Format(http.TimeFormat)} {
		if _, ok := p.delay(0, header, now); ok {
			t.Fatalf("accepted excessive Retry-After=%q", header)
		}
	}
	for _, header := range []string{"", "invalid", "-1", "0", now.Add(-time.Hour).Format(http.TimeFormat)} {
		if got, ok := p.delay(0, header, now); !ok || got < 50*time.Millisecond || got > 100*time.Millisecond {
			t.Fatalf("header=%q delay=%v ok=%v", header, got, ok)
		}
	}
}

func TestProvidersBackoffAndCancellation(t *testing.T) {
	for _, fixture := range networkFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var requests atomic.Int32
			first := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					close(first)
				}
				http.Error(w, "busy", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			p := networkProvider(fixture.name, server.URL, WithRetryPolicy(RetryPolicy{MaxRetries: 2, InitialDelay: time.Minute, MaxDelay: time.Minute}))
			finished := make(chan error, 1)
			go func() { _, _, err := collectNetworkStream(ctx, p, Context{}, StreamOptions{}); finished <- err }()
			<-first
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
					t.Fatalf("requests=%d err=%v", requests.Load(), err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not interrupt backoff")
			}
		})
	}
}

func TestProvidersExponentialBackoff(t *testing.T) {
	for _, fixture := range networkFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			times := make(chan time.Time, 3)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				times <- time.Now()
				if requests.Add(1) < 3 {
					http.Error(w, "busy", 503)
					return
				}
				fmt.Fprint(w, fixture.success)
			}))
			defer server.Close()
			p := networkProvider(fixture.name, server.URL, WithRetryPolicy(RetryPolicy{
				MaxRetries: 2, InitialDelay: 40 * time.Millisecond, MaxDelay: 80 * time.Millisecond,
			}))
			if _, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			previous := <-times
			for _, minimum := range []time.Duration{20 * time.Millisecond, 40 * time.Millisecond} {
				next := <-times
				if elapsed := next.Sub(previous); elapsed < minimum {
					t.Fatalf("retry after %v, minimum %v", elapsed, minimum)
				}
				previous = next
			}
		})
	}
}

func TestProvidersRetryAfter(t *testing.T) {
	for _, fixture := range networkFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var requests atomic.Int32
			var timesMu sync.Mutex
			var times []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				timesMu.Lock()
				times = append(times, time.Now())
				timesMu.Unlock()
				if requests.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					http.Error(w, "rate limited", 429)
					return
				}
				fmt.Fprint(w, fixture.success)
			}))
			defer server.Close()
			p := networkProvider(fixture.name, server.URL, WithRetryPolicy(RetryPolicy{MaxRetries: 1, InitialDelay: time.Millisecond, MaxDelay: 2 * time.Second}))
			if _, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			timesMu.Lock()
			if len(times) != 2 || times[1].Sub(times[0]) < time.Second {
				t.Fatalf("retry did not respect Retry-After: %v", times)
			}
			timesMu.Unlock()
			// A server-requested delay beyond the configured cap must not cause an early retry.
			requests.Store(0)
			p = networkProvider(fixture.name, server.URL, fastRetry(2))
			if _, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{}); err == nil || requests.Load() != 1 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestProvidersInjectedClient(t *testing.T) {
	for _, fixture := range networkFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var proxyRequests atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxyRequests.Add(1)
				if r.URL.Host != "provider.invalid" {
					t.Errorf("proxy URL=%v", r.URL)
				}
				fmt.Fprint(w, fixture.success)
			}))
			defer proxy.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			p := networkProvider(fixture.name, "http://provider.invalid", WithHTTPClient(client))
			if _, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if proxyRequests.Load() != 1 || client.Timeout != time.Second || client.Transport != transport {
				t.Fatal("injected client/proxy not preserved")
			}
		})
		t.Run(fixture.name+"/timeout", func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				requests.Add(1)
				<-r.Context().Done()
			}))
			defer server.Close()
			p := networkProvider(fixture.name, server.URL, WithHTTPClient(&http.Client{Timeout: 30 * time.Millisecond}), fastRetry(1))
			_, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{})
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || requests.Load() != 2 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

type auditLimiter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (l *auditLimiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.err
}

func TestProvidersShareRequestLimiter(t *testing.T) {
	limiter := &auditLimiter{}
	var group sync.WaitGroup
	errorsCh := make(chan error, len(networkFixtures))
	for _, fixture := range networkFixtures {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				http.Error(w, "busy", 503)
				return
			}
			fmt.Fprint(w, fixture.success)
		}))
		defer server.Close()
		p := networkProvider(fixture.name, server.URL, WithRequestLimiter(limiter), fastRetry(1))
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{})
			errorsCh <- err
		}()
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if limiter.calls != 4 {
		t.Fatalf("shared limiter calls=%d, want 4", limiter.calls)
	}
	limiter.err = context.Canceled
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("must not send")
	})}
	p := networkProvider("openai", "http://provider.invalid", WithHTTPClient(client), WithRequestLimiter(limiter))
	if _, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{}); !errors.Is(err, context.Canceled) || requests.Load() != 0 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
}

func TestProvidersDoNotRetryStreamingTimeout(t *testing.T) {
	for _, fixture := range networkFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				requests.Add(1)
				fmt.Fprint(w, fixture.partial)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			p := networkProvider(fixture.name, server.URL, WithHTTPClient(&http.Client{Timeout: 100 * time.Millisecond}), fastRetry(2))
			_, _, err := collectNetworkStream(context.Background(), p, Context{}, StreamOptions{})
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || requests.Load() != 1 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
		})
	}
}

func TestProviderDefaults(t *testing.T) {
	cfg := newProviderHTTPConfig([]ProviderOption{nil, WithHTTPClient(nil)})
	if cfg.client == nil || cfg.client.Timeout != 0 || cfg.retry != (RetryPolicy{MaxRetries: 2, InitialDelay: 250 * time.Millisecond, MaxDelay: 4 * time.Second}) {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}
