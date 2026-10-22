package sched

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// errPermanent marks a delivery failure that retrying cannot fix (e.g. 4xx).
var errPermanent = errors.New("permanent callback failure")

// callbackSender delivers terminal-state webhooks asynchronously. Delivery is
// best-effort with bounded retries: a slow or dead endpoint must never stall
// scheduler workers or back-pressure the queue.
type callbackSender struct {
	client   *http.Client
	attempts int
	backoff  time.Duration
	log      *slog.Logger
	sem      chan struct{}
	wg       sync.WaitGroup
}

func newCallbackSender(logger *slog.Logger) *callbackSender {
	return &callbackSender{
		client: &http.Client{
			Timeout: 10 * time.Second,
			// Never follow redirects: the signed body must go exactly where the
			// caller pointed it, not wherever a 3xx leads.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		attempts: 5,
		backoff:  time.Second,
		log:      logger,
		sem:      make(chan struct{}, 8),
	}
}

// callbackPayload is the webhook body. The event fields are inlined so a
// consumer can switch on `type` without unwrapping.
type callbackPayload struct {
	Event
	Counts          map[string]int `json:"counts,omitempty"`
	CountsByAccount map[string]int `json:"countsByAccount,omitempty"`
}

func (c *callbackSender) enqueue(ev Event, cb *Callback) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.sem <- struct{}{}
		defer func() { <-c.sem }()
		c.deliver(ev, cb)
	}()
}

func (c *callbackSender) deliver(ev Event, cb *Callback) {
	body, err := json.Marshal(payloadFor(ev))
	if err != nil {
		c.log.Error("回调负载序列化失败", "batchId", ev.BatchID, "err", err)
		return
	}
	delay := c.backoff
	for attempt := 1; attempt <= c.attempts; attempt++ {
		err = c.post(cb, ev, body)
		if err == nil {
			c.log.Info("回调已送达", "batchId", ev.BatchID, "event", ev.Type, "url", safeURL(cb.URL), "attempt", attempt)
			return
		}
		if attempt == c.attempts {
			c.log.Error("回调送达失败，已放弃", "batchId", ev.BatchID, "event", ev.Type, "url", safeURL(cb.URL), "attempts", attempt, "err", err)
			return
		}
		if errors.Is(err, errPermanent) {
			c.log.Error("回调被拒绝（不可重试）", "batchId", ev.BatchID, "event", ev.Type, "url", safeURL(cb.URL), "err", err)
			return
		}
		c.log.Warn("回调失败，准备重试", "batchId", ev.BatchID, "event", ev.Type, "attempt", attempt, "retryIn", delay, "err", err)
		time.Sleep(delay)
		if delay < 30*time.Second {
			delay *= 2
		}
	}
}

func (c *callbackSender) post(cb *Callback, ev Event, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, cb.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	delivery := randomID()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "jq-mcp")
	req.Header.Set("X-JQ-Event", ev.Type)
	req.Header.Set("X-JQ-Event-Id", strconv.FormatInt(ev.ID, 10))
	req.Header.Set("X-JQ-Batch-Id", ev.BatchID)
	req.Header.Set("X-JQ-Delivery", delivery)
	req.Header.Set("X-JQ-Timestamp", ts)
	if ev.Job != nil && ev.Job.Account != "" {
		req.Header.Set("X-JQ-Account", ev.Job.Account)
	}
	if cb.Secret != "" {
		req.Header.Set("X-JQ-Signature", "sha256="+sign(cb.Secret, ts, body))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("%w: HTTP %d", errPermanent, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// sign binds the timestamp and body with HMAC-SHA256 so a receiver can verify
// authenticity (and reject replays outside its timestamp window).
func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func payloadFor(ev Event) callbackPayload {
	p := callbackPayload{Event: ev}
	if len(ev.Jobs) > 0 {
		p.Counts = map[string]int{}
		p.CountsByAccount = map[string]int{}
		for _, js := range ev.Jobs {
			p.Counts[js.Status]++
			if js.Account != "" {
				p.CountsByAccount[js.Account]++
			}
		}
	}
	return p
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// safeURL strips credentials, query and fragment before logging.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// wait blocks until in-flight deliveries finish. Used by tests and shutdown.
func (c *callbackSender) wait() { c.wg.Wait() }
