// Adapted from TogetherWeOwn/cpa-plugin-subscription-pool at ad3cb9ecd0130b1b7a0ffb122d0d4685bd8e659b; MIT licensed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
)

const (
	maxQuotaResponse    = 1 << 20
	defaultQuotaTimeout = 15 * time.Second
	quotaHeader         = "X-Synthetic-Quotas"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// quotaReading is one parsed weekly-credit observation. PercentRemaining is
// 0-100 as Synthetic reports it. Every field is optional on the wire; the
// parser never fabricates a value that was not reported.
type quotaReading struct {
	PercentRemaining *float64
	NextRegenAt      time.Time
	NextRegenPercent *float64
	Shape            string
}

func (q quotaReading) utilization() *float64 {
	if q.PercentRemaining == nil {
		return nil
	}
	used := (100 - clamp(*q.PercentRemaining, 0, 100)) / 100
	return &used
}

// quotaWire covers every shape Synthetic has published for GET /v2/quotas
// and the x-synthetic-quotas response header:
//   - current plans: planWeeklyLimit {percentRemaining, nextRegenAt, nextRegenPercent}
//   - legacy token plans: weeklyTokenLimit {percentRemaining, nextRegenAt, ...credits}
//   - legacy request packs: rollingFiveHourLimit {remaining, max, nextTickAt, tickPercent}
//   - the documented example: subscription {limit, requests, renewsAt}
type quotaWire struct {
	PlanWeeklyLimit *struct {
		PercentRemaining *float64 `json:"percentRemaining"`
		NextRegenAt      string   `json:"nextRegenAt"`
		NextRegenPercent *float64 `json:"nextRegenPercent"`
	} `json:"planWeeklyLimit"`
	WeeklyTokenLimit *struct {
		PercentRemaining *float64 `json:"percentRemaining"`
		NextRegenAt      string   `json:"nextRegenAt"`
	} `json:"weeklyTokenLimit"`
	RollingFiveHourLimit *struct {
		Remaining   *float64 `json:"remaining"`
		Max         *float64 `json:"max"`
		NextTickAt  string   `json:"nextTickAt"`
		TickPercent *float64 `json:"tickPercent"`
	} `json:"rollingFiveHourLimit"`
	Subscription *struct {
		Limit    *float64 `json:"limit"`
		Requests *float64 `json:"requests"`
		RenewsAt string   `json:"renewsAt"`
	} `json:"subscription"`
}

var errNoQuotaShape = errors.New("quota payload has no recognized limit")

// parseQuotas returns one reading per quota shape present in the payload:
// weekly (current plan preferred), rolling-5h (legacy request
// packs), and subscription (the documented example). Shape order follows the
// wire struct, so parseQuota's historical first-match result is readings[0].
// planWeeklyLimit and weeklyTokenLimit are the same weekly-credits bucket in
// its current and legacy form, so a payload reporting both keeps only the
// current plan shape instead of rendering two identical "Weekly credits" cards.
func parseQuotas(raw []byte) ([]quotaReading, error) {
	var wire quotaWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decode quota payload")
	}
	var readings []quotaReading
	hasPlanWeekly := wire.PlanWeeklyLimit != nil && wire.PlanWeeklyLimit.PercentRemaining != nil
	if hasPlanWeekly {
		w := wire.PlanWeeklyLimit
		readings = append(readings, quotaReading{PercentRemaining: floatPtr(*w.PercentRemaining), NextRegenAt: parseTime(w.NextRegenAt), NextRegenPercent: copyFloat(w.NextRegenPercent), Shape: "planWeeklyLimit"})
	}
	if !hasPlanWeekly && wire.WeeklyTokenLimit != nil && wire.WeeklyTokenLimit.PercentRemaining != nil {
		w := wire.WeeklyTokenLimit
		readings = append(readings, quotaReading{PercentRemaining: floatPtr(*w.PercentRemaining), NextRegenAt: parseTime(w.NextRegenAt), Shape: "weeklyTokenLimit"})
	}
	if wire.RollingFiveHourLimit != nil && wire.RollingFiveHourLimit.Remaining != nil && wire.RollingFiveHourLimit.Max != nil && *wire.RollingFiveHourLimit.Max > 0 {
		w := wire.RollingFiveHourLimit
		reading := quotaReading{PercentRemaining: floatPtr(*w.Remaining / *w.Max * 100), NextRegenAt: parseTime(w.NextTickAt), Shape: "rollingFiveHourLimit"}
		if w.TickPercent != nil {
			reading.NextRegenPercent = floatPtr(*w.TickPercent * 100)
		}
		readings = append(readings, reading)
	}
	if wire.Subscription != nil && wire.Subscription.Limit != nil && wire.Subscription.Requests != nil && *wire.Subscription.Limit > 0 {
		w := wire.Subscription
		readings = append(readings, quotaReading{PercentRemaining: floatPtr((*w.Limit - *w.Requests) / *w.Limit * 100), NextRegenAt: parseTime(w.RenewsAt), Shape: "subscription"})
	}
	if len(readings) == 0 {
		return nil, errNoQuotaShape
	}
	return readings, nil
}

func parseQuota(raw []byte) (quotaReading, error) {
	readings, err := parseQuotas(raw)
	if err != nil {
		return quotaReading{}, err
	}
	return readings[0], nil
}

// quotaReadingsFromHeaders reads the x-synthetic-quotas header Synthetic
// attaches to inference responses, keeping every shape the header reports.
// It returns ok=false when the header is absent or unparseable.
func quotaReadingsFromHeaders(headers http.Header) ([]quotaReading, bool) {
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), quotaHeader) {
			continue
		}
		for _, value := range values {
			if readings, err := parseQuotas([]byte(value)); err == nil {
				return readings, true
			}
		}
	}
	return nil, false
}

func quotaFromHeaders(headers http.Header) (quotaReading, bool) {
	readings, ok := quotaReadingsFromHeaders(headers)
	if !ok || len(readings) == 0 {
		return quotaReading{}, false
	}
	return readings[0], true
}

// newQuotaHTTPClient follows the proxy that the kit's CachedQuota attaches to
// each poll: the auth's proxy_url, else the host's global proxy-url. The kit
// transport keeps pooled connections from crossing proxies.
func newQuotaHTTPClient() *http.Client {
	client := &http.Client{Transport: providerkit.NewQuotaTransport(), Timeout: defaultQuotaTimeout}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

// fetchQuotas performs one bounded GET of the quota endpoint and returns one
// reading per shape the payload reports. Errors are generic: neither the API
// key nor the response body is ever included.
func fetchQuotas(ctx context.Context, client httpDoer, endpoint, apiKey string) ([]quotaReading, error) {
	if client == nil || endpoint == "" || apiKey == "" {
		return nil, fmt.Errorf("quota poll not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultQuotaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build quota request")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read quota response")
	}
	if len(body) > maxQuotaResponse {
		return nil, fmt.Errorf("quota response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota endpoint returned HTTP %d", resp.StatusCode)
	}
	readings, err := parseQuotas(body)
	if err != nil {
		return nil, err
	}
	return readings, nil
}

func fetchQuota(ctx context.Context, client httpDoer, endpoint, apiKey string) (quotaReading, error) {
	readings, err := fetchQuotas(ctx, client, endpoint, apiKey)
	if err != nil {
		return quotaReading{}, err
	}
	return readings[0], nil
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func floatPtr(value float64) *float64 { return &value }

func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return floatPtr(*value)
}

func clamp(value, lo, hi float64) float64 {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}
