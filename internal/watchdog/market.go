package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const (
	defaultYahooPrimaryURL  = "https://query1.finance.yahoo.com/v8/finance/chart"
	defaultYahooFallbackURL = "https://query2.finance.yahoo.com/v8/finance/chart"
)

// Quote represents live price and 2-year high drawdown telemetry for a ticker.
type Quote struct {
	Symbol       string
	Price        float64
	PrevClose    float64
	TwoYearHigh  float64
	FiftyTwoLow  float64
	DrawdownPct  float64 // Negative percentage from TwoYearHigh (e.g., -0.22 = -22%)
	DayChangePct float64
	MarketTime   time.Time
}

// MarketData bundles ETF quotes and macro rate/volatility context.
type MarketData struct {
	Quotes       map[string]Quote
	VIX          float64
	TBillYield   float64 // 13-week T-bill proxy for short-term rates (%)
	EstimatedAPR float64 // TBillYield + MarginSpreadPct (%)
}

type yahooChartResp struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ChartPreviousClose float64 `json:"chartPreviousClose"`
				PreviousClose      float64 `json:"previousClose"`
				FiftyTwoWeekHigh   float64 `json:"fiftyTwoWeekHigh"`
				FiftyTwoWeekLow    float64 `json:"fiftyTwoWeekLow"`
				RegularMarketTime  int64   `json:"regularMarketTime"`
			} `json:"meta"`
			Indicators struct {
				Quote []struct {
					High []*float64 `json:"high"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error any `json:"error"`
	} `json:"chart"`
}

// FetchMarketData pulls 2-year daily highs and live prices for symbols plus ^VIX and ^IRX,
// using query1 -> query2 failover with polite pacing.
func FetchMarketData(ctx context.Context, client *http.Client, baseURL string, marginSpreadPct, fallbackAPRPct float64, symbols []string) (*MarketData, error) {
	primary := baseURL
	fallback := ""
	if primary == "" {
		primary = defaultYahooPrimaryURL
		fallback = defaultYahooFallbackURL
	}
	return FetchMarketDataWithFallback(ctx, client, primary, fallback, 200*time.Millisecond, 1500*time.Millisecond, marginSpreadPct, fallbackAPRPct, symbols)
}

// FetchMarketDataWithFallback fetches quotes with configurable primary/fallback hosts, pacing, and retry backoff.
func FetchMarketDataWithFallback(
	ctx context.Context,
	client *http.Client,
	primaryURL, fallbackURL string,
	pacing, retryBackoff time.Duration,
	marginSpreadPct, fallbackAPRPct float64,
	symbols []string,
) (*MarketData, error) {
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	if primaryURL == "" {
		primaryURL = defaultYahooPrimaryURL
	}
	if fallbackAPRPct <= 0 {
		fallbackAPRPct = 6.25
	}
	md := &MarketData{
		Quotes:       make(map[string]Quote),
		EstimatedAPR: fallbackAPRPct,
	}

	for i, sym := range symbols {
		if i > 0 && pacing > 0 {
			if err := sleepContext(ctx, pacing); err != nil {
				return nil, err
			}
		}
		q, err := fetchYahooQuoteWithRetry(ctx, client, primaryURL, fallbackURL, retryBackoff, sym, "2y")
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", sym, err)
		}
		md.Quotes[sym] = q
	}

	// Best-effort macro context (^VIX and ^IRX 13-week T-bill yield)
	if pacing > 0 {
		_ = sleepContext(ctx, pacing)
	}
	if vixQ, err := fetchYahooQuoteWithRetry(ctx, client, primaryURL, fallbackURL, retryBackoff, "^VIX", "5d"); err == nil {
		md.VIX = vixQ.Price
	}
	if pacing > 0 {
		_ = sleepContext(ctx, pacing)
	}
	if irxQ, err := fetchYahooQuoteWithRetry(ctx, client, primaryURL, fallbackURL, retryBackoff, "^IRX", "5d"); err == nil && irxQ.Price > 0.1 && irxQ.Price < 15.0 {
		md.TBillYield = irxQ.Price
		md.EstimatedAPR = irxQ.Price + marginSpreadPct
	}
	return md, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func fetchYahooQuoteWithRetry(
	ctx context.Context,
	client *http.Client,
	primaryURL, fallbackURL string,
	retryBackoff time.Duration,
	sym, rng string,
) (Quote, error) {
	q, err := fetchYahooQuote(ctx, client, primaryURL, sym, rng)
	if err == nil {
		return q, nil
	}
	if fallbackURL == "" {
		return Quote{}, err
	}
	if sleepErr := sleepContext(ctx, retryBackoff); sleepErr != nil {
		return Quote{}, err
	}
	q2, err2 := fetchYahooQuote(ctx, client, fallbackURL, sym, rng)
	if err2 != nil {
		return Quote{}, fmt.Errorf("primary failed (%v); fallback failed (%w)", err, err2)
	}
	return q2, nil
}

func fetchYahooQuote(ctx context.Context, client *http.Client, baseURL, sym, rng string) (Quote, error) {
	u := fmt.Sprintf("%s/%s?interval=1d&range=%s", baseURL, url.PathEscape(sym), url.QueryEscape(rng))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Quote{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")

	resp, err := client.Do(req)
	if err != nil {
		return Quote{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Quote{}, fmt.Errorf("HTTP %d for %s", resp.StatusCode, sym)
	}

	var parsed yahooChartResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Quote{}, err
	}
	if len(parsed.Chart.Result) == 0 {
		return Quote{}, fmt.Errorf("empty chart result for %s", sym)
	}
	res := parsed.Chart.Result[0]
	cur := res.Meta.RegularMarketPrice
	if cur <= 0 {
		return Quote{}, fmt.Errorf("invalid non-positive price %.4f for %s", cur, sym)
	}
	ath := res.Meta.FiftyTwoWeekHigh
	if len(res.Indicators.Quote) > 0 {
		for _, h := range res.Indicators.Quote[0].High {
			if h != nil && *h > ath {
				ath = *h
			}
		}
	}
	if ath < cur {
		ath = cur
	}
	dd := 0.0
	if ath > 0 {
		dd = (cur / ath) - 1.0
	}
	prev := res.Meta.PreviousClose
	if prev <= 0 {
		prev = res.Meta.ChartPreviousClose
	}
	dayChg := 0.0
	if prev > 0 {
		dayChg = (cur / prev) - 1.0
	}

	return Quote{
		Symbol:       sym,
		Price:        cur,
		PrevClose:    prev,
		TwoYearHigh:  ath,
		FiftyTwoLow:  res.Meta.FiftyTwoWeekLow,
		DrawdownPct:  dd,
		DayChangePct: dayChg,
		MarketTime:   time.Unix(res.Meta.RegularMarketTime, 0).UTC(),
	}, nil
}
