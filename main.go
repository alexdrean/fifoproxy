package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxBodyBytes      = 1 << 20         // 1 MB per request
	retryInterval     = 5 * time.Second // between attempts
	requestTimeout    = 5 * time.Second // per upstream attempt
	defaultMaxRetries = 0               // 0 = infinite retries
	defaultQueueSize  = 1000
	defaultPort       = 8080
)

// Config holds runtime configuration.
type Config struct {
	TargetBaseURL    string
	TelegramBotToken string
	TelegramChatID   string
	MaxRetries       int // 0 means infinite
	QueueSize        int
	Port             int
}

type QueuedRequest struct {
	ID         string
	Method     string
	Header     http.Header
	Body       []byte
	Path       string
	RawQuery   string
	ReceivedAt time.Time

	Attempts  int
	AlertSent bool
}

type Proxy struct {
	cfg        Config
	targetBase *url.URL
	queue      chan *QueuedRequest
	client     *http.Client
	telegram   *TelegramNotifier
	mux        *http.ServeMux
}

type TelegramNotifier struct {
	botToken string
	chatID   string
	client   *http.Client
}

func main() {
	cfg := loadConfig()
	p := newProxy(cfg)

	server := &http.Server{
		Addr:    ":" + strconv.Itoa(cfg.Port),
		Handler: p.mux,
	}

	// Worker lifecycle
	shutdownCh := make(chan struct{})
	var wg sync.WaitGroup
	p.startWorker(shutdownCh, &wg)

	// Graceful shutdown on SIGINT/SIGTERM
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		<-signals

		log.Println("shutdown signal received")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Printf("server shutdown error: %v", err)
		}

		// Now let the worker drain outstanding jobs.
		close(shutdownCh)
	}()

	log.Printf("Starting async proxy on :%d -> %s", cfg.Port, cfg.TargetBaseURL)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}

	// Wait for worker to finish draining.
	wg.Wait()
	log.Println("shutdown complete")
}

func loadConfig() Config {
	target := flag.String("target-url", "", "Base target URL, e.g. https://example.com")
	telegramBotToken := flag.String("telegram-bot-token", "", "Telegram bot token (optional)")
	telegramChatID := flag.String("telegram-chat-id", "", "Telegram chat ID (optional)")
	maxRetries := flag.Int("max-retries", defaultMaxRetries, "Max retries per job; 0 = infinite")
	queueSize := flag.Int("queue-size", defaultQueueSize, "Max in-memory queue size")
	port := flag.Int("port", defaultPort, "HTTP listen port")

	flag.Parse()

	if *target == "" {
		flag.Usage()
		log.Fatal("missing required -target-url")
	}
	if *queueSize <= 0 {
		log.Fatal("queue-size must be > 0")
	}
	if *maxRetries < 0 {
		*maxRetries = 0
	}

	return Config{
		TargetBaseURL:    *target,
		TelegramBotToken: *telegramBotToken,
		TelegramChatID:   *telegramChatID,
		MaxRetries:       *maxRetries,
		QueueSize:        *queueSize,
		Port:             *port,
	}
}

func newProxy(cfg Config) *Proxy {
	base, err := url.Parse(cfg.TargetBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		log.Fatalf("invalid -target-url %q: %v", cfg.TargetBaseURL, err)
	}

	p := &Proxy{
		cfg:        cfg,
		targetBase: base,
		queue:      make(chan *QueuedRequest, cfg.QueueSize),
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxConnsPerHost:     100,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  false,
				DisableKeepAlives:   false,
				TLSHandshakeTimeout: 5 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}

	if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
		p.telegram = &TelegramNotifier{
			botToken: cfg.TelegramBotToken,
			chatID:   cfg.TelegramChatID,
			client:   &http.Client{Timeout: 5 * time.Second},
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", p.handleIncoming)

	p.mux = mux
	return p
}

// handleIncoming enqueues the request and immediately 200s.
func (p *Proxy) handleIncoming(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := ioutil.ReadAll(limited)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	job := &QueuedRequest{
		ID:         generateID(),
		Method:     r.Method,
		Header:     cloneHeader(r.Header),
		Body:       body,
		Path:       r.URL.Path,
		RawQuery:   r.URL.RawQuery,
		ReceivedAt: time.Now(),
	}

	select {
	case p.queue <- job:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("accepted"))
	default:
		http.Error(w, "queue full", http.StatusServiceUnavailable)
		log.Printf("queue full; dropping request")
	}
}

// startWorker runs a single FIFO worker, drains on shutdown.
func (p *Proxy) startWorker(shutdownCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			select {
			case <-shutdownCh:
				// Drain remaining queue, then exit.
				for {
					select {
					case job := <-p.queue:
						if job != nil {
							p.processJob(job)
						}
					default:
						return
					}
				}
			case job := <-p.queue:
				if job == nil {
					continue
				}
				p.processJob(job)
			}
		}
	}()
}

func (p *Proxy) processJob(job *QueuedRequest) {
	for {
		job.Attempts++

		if err := p.deliver(job); err != nil {
			log.Printf("delivery failed (id=%s attempt=%d): %v", job.ID, job.Attempts, err)

			if job.Attempts > 3 && !job.AlertSent && p.telegram != nil {
				job.AlertSent = true
				go p.sendAlert(job, err)
			}

			if p.cfg.MaxRetries > 0 && job.Attempts >= p.cfg.MaxRetries {
				log.Printf("giving up on job id=%s after %d attempts", job.ID, job.Attempts)
				return
			}

			time.Sleep(retryInterval)
			continue
		}

		log.Printf("delivery succeeded (id=%s attempts=%d)", job.ID, job.Attempts)
		return
	}
}

// deliver forwards to targetBase + original path + query with same method/body/headers.
func (p *Proxy) deliver(job *QueuedRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	targetURL := buildTargetURL(p.targetBase, job.Path, job.RawQuery)

	req, err := http.NewRequestWithContext(ctx, job.Method, targetURL, bytes.NewReader(job.Body))
	if err != nil {
		return err
	}

	for k, vv := range job.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Host", "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailers", "Transfer-Encoding", "Upgrade":
			continue
		default:
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 512))
		return &UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       string(b),
		}
	}

	return nil
}

func (p *Proxy) sendAlert(job *QueuedRequest, lastErr error) {
	if p.telegram == nil {
		return
	}

	msg := struct {
		JobID      string `json:"job_id"`
		Attempts   int    `json:"attempts"`
		LastError  string `json:"last_error"`
		ReceivedAt string `json:"received_at"`
	}{
		JobID:      job.ID,
		Attempts:   job.Attempts,
		LastError:  lastErr.Error(),
		ReceivedAt: job.ReceivedAt.Format(time.RFC3339),
	}

	b, _ := json.Marshal(msg)
	if err := p.telegram.SendMessage("Delivery failure:\n" + string(b)); err != nil {
		log.Printf("failed to send telegram alert for job id=%s: %v", job.ID, err)
	}
}

func (t *TelegramNotifier) SendMessage(text string) error {
	apiURL := "https://api.telegram.org/bot" + t.botToken + "/sendMessage"

	payload := map[string]string{
		"chat_id": t.chatID,
		"text":    text,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 512))
		return &UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       string(b),
		}
	}

	return nil
}

type UpstreamError struct {
	StatusCode int
	Body       string
}

func (e *UpstreamError) Error() string {
	if e.Body == "" {
		return "upstream status " + strconv.Itoa(e.StatusCode)
	}
	return "upstream status " + strconv.Itoa(e.StatusCode) + ": " + e.Body
}

func generateID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vv := range h {
		cp := make([]string, len(vv))
		copy(cp, vv)
		out[k] = cp
	}
	return out
}

// buildTargetURL joins base URL with the original request path + query.
func buildTargetURL(base *url.URL, path, rawQuery string) string {
	u := *base

	if path != "" {
		switch {
		case strings.HasSuffix(u.Path, "/") && strings.HasPrefix(path, "/"):
			u.Path = u.Path + path[1:]
		case !strings.HasSuffix(u.Path, "/") && !strings.HasPrefix(path, "/"):
			u.Path = u.Path + "/" + path
		default:
			u.Path = u.Path + path
		}
	}

	u.RawQuery = rawQuery
	return u.String()
}
