package watchdog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPolicyConfig() PolicyConfig {
	c := Config{
		PortfolioName: "Test 40:40:20",
		Hledger:       HledgerConfig{Journal: "/tmp/test.journal"},
		Policy: PolicyConfig{
			TaxWallStartStr: "2027-01-15",
			TaxWallEndStr:   "2027-04-15",
			Sleeves: []SleeveConfig{
				{
					ID: "us-large-cap", Name: "US Large Cap Core", IsCore: true, IsBroadEquity: true,
					CoreTargetW: 0.40, CrashThreshold: -0.20,
					Twins: []TwinETF{
						{Symbol: "VOO", Reason: "S&P 500"},
						{Symbol: "SCHX", Reason: "Dow Jones US Large-Cap"},
						{Symbol: "IVV", Reason: "iShares S&P 500"},
					},
				},
				{
					ID: "us-mid-cap", Name: "US Mid Cap Core", IsCore: true, IsBroadEquity: true,
					CoreTargetW: 0.40, CrashThreshold: -0.20,
					Twins: []TwinETF{
						{Symbol: "VO", Reason: "CRSP US Mid Cap"},
						{Symbol: "SCHM", Reason: "Dow Jones US Mid-Cap"},
						{Symbol: "IJH", Reason: "S&P MidCap 400"},
					},
				},
				{
					ID: "physical-gold", Name: "Physical Gold Core", IsCore: true, IsBroadEquity: false,
					CoreTargetW: 0.20, CrashThreshold: -0.25,
					Twins: []TwinETF{
						{Symbol: "GLDM", Reason: "SPDR Gold MiniShares"},
						{Symbol: "IAUM", Reason: "iShares Gold Trust Micro"},
						{Symbol: "SGOL", Reason: "abrdn Physical Gold"},
					},
				},
				{
					ID: "sector-sat", Name: "Sector Satellite", IsCore: false, IsBroadEquity: false,
					CoreTargetW: 0.00, CrashThreshold: -0.35,
					Twins: []TwinETF{
						{Symbol: "QQQ", Reason: "Nasdaq 100"},
						{Symbol: "CASH_31D", Reason: "Park cash 31d"},
					},
				},
			},
		},
	}
	if err := c.ApplyDefaultsAndValidate("/tmp"); err != nil {
		panic(err)
	}
	return c.Policy
}

func baseTestSnapshot(now time.Time, cash float64) *LedgerSnapshot {
	old := now.AddDate(0, -6, 0)
	return &LedgerSnapshot{
		CashBalance: cash,
		Positions: map[string]float64{
			"VOO":  800,
			"VO":   1600,
			"GLDM": 4000,
			"QQQ":  20,
		},
		Lots: map[string][]TaxLot{
			"VOO":  {{Date: old, Shares: 800, UnitCost: 500.0, TotalCost: 400000}},
			"VO":   {{Date: old, Shares: 1600, UnitCost: 250.0, TotalCost: 400000}},
			"GLDM": {{Date: old, Shares: 4000, UnitCost: 50.0, TotalCost: 200000}},
			"QQQ":  {{Date: old, Shares: 20, UnitCost: 500.0, TotalCost: 10000}},
		},
		Lockouts:        make(map[string]WashSaleLockout),
		OtherAccountSym: make(map[string][]string),
		AsOf:            now,
	}
}

func baseMarket(vooDD, voDD, gldmDD, qqqDD, apr float64) *MarketData {
	mk := func(sym string, high, dd float64) Quote {
		return Quote{Symbol: sym, TwoYearHigh: high, Price: high * (1 + dd), DrawdownPct: dd}
	}
	return &MarketData{
		Quotes: map[string]Quote{
			"VOO":  mk("VOO", 550.0, vooDD),
			"VO":   mk("VO", 275.0, voDD),
			"GLDM": mk("GLDM", 60.0, gldmDD),
			"QQQ":  mk("QQQ", 550.0, qqqDD),
		},
		VIX:          18.0,
		EstimatedAPR: apr,
	}
}

func TestLoadConfigAndValidation(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	raw := `{
		"portfolio_name": "My Core",
		"hledger": { "journal": "finance/all.journal" },
		"policy": {
			"sleeves": [
				{
					"name": "US Large Cap",
					"is_core": true,
					"is_broad_equity": true,
					"core_target_w": 1.0,
					"crash_threshold": 0.20,
					"twins": [{"symbol": "VOO", "reason": "S&P 500"}]
				}
			]
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Hledger.Journal != filepath.Join(tmp, "finance/all.journal") {
		t.Fatalf("expected relative journal path resolved against config dir, got %s", cfg.Hledger.Journal)
	}
	if cfg.Policy.Sleeves[0].CrashThreshold != -0.20 {
		t.Fatalf("expected positive crash_threshold normalized to -0.20, got %v", cfg.Policy.Sleeves[0].CrashThreshold)
	}
	if cfg.Policy.CooldownLTVMax != 0.08 || cfg.Policy.TLHMinLossDollars != 5000 {
		t.Fatalf("unexpected defaults: %+v", cfg.Policy)
	}
}

func TestCoreCrashApprovedWhenLowLTV(t *testing.T) {
	now := time.Date(2027, 6, 15, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(now, -20000) // ~2% LTV (<= 8% cooldown)
	market := baseMarket(-0.22, -0.21, -0.10, -0.45, 5.25)

	rep := Evaluate("Test", cfg, ledger, market, now, false)
	var found bool
	for _, a := range rep.Alerts {
		if a.Severity == "HIGH" && a.Code == "CORE_CRASH_APPROVED_VOO_VO" {
			found = true
			if strings.Contains(a.Detail, "QQQ") {
				t.Fatalf("non-core satellite must never be recommended for margin buy: %s", a.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("expected HIGH core crash alert, got %+v", rep.Alerts)
	}
}

func TestSatelliteCrashAloneNeverTriggersMargin(t *testing.T) {
	now := time.Date(2027, 6, 15, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(now, 0)
	market := baseMarket(-0.08, -0.09, -0.12, -0.50, 5.50)

	rep := Evaluate("Test", cfg, ledger, market, now, false)
	for _, a := range rep.Alerts {
		if strings.HasPrefix(a.Code, "CORE_CRASH_") {
			t.Fatalf("satellite crash must not trigger core margin gate: %+v", a)
		}
	}
}

func TestGate2CooldownAndTaxWallBlockMargin(t *testing.T) {
	now := time.Date(2027, 2, 10, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(now, -140000)
	market := baseMarket(-0.22, -0.21, -0.28, -0.40, 6.25)

	rep := Evaluate("Test", cfg, ledger, market, now, false)
	for _, a := range rep.Alerts {
		if a.Severity == "HIGH" {
			t.Fatalf("expected margin deployment blocked by cooldown & tax wall, got HIGH: %+v", a)
		}
	}
	var blocked bool
	for _, a := range rep.Alerts {
		if strings.HasPrefix(a.Code, "CORE_CRASH_BLOCKED_") && strings.Contains(a.Detail, "Tax-Wall") && strings.Contains(a.Detail, "Cooldown") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected CORE_CRASH_BLOCKED with both Tax-Wall and Cooldown reasons, got %+v", rep.Alerts)
	}
}

func TestGate3StagflationAPRBlock(t *testing.T) {
	now := time.Date(2027, 8, 10, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(now, 0)
	market := baseMarket(-0.25, -0.24, -0.10, -0.30, 8.00)

	rep := Evaluate("Test", cfg, ledger, market, now, false)
	for _, a := range rep.Alerts {
		if a.Severity == "HIGH" {
			t.Fatalf("expected APR >= 7.5%% to block margin, got HIGH: %+v", a)
		}
	}
}

func TestDynamicSleeveAliasTracksRotatedSCHXAndPicksUnlockedIVV(t *testing.T) {
	now := time.Date(2026, 12, 1, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(now, -10000)
	delete(ledger.Positions, "VOO")
	delete(ledger.Lots, "VOO")
	ledger.Positions["SCHX"] = 16000
	ledger.Lots["SCHX"] = []TaxLot{{Date: now.AddDate(0, 0, -10), Shares: 16000, UnitCost: 26.0, TotalCost: 416000}}
	ledger.Lockouts["VOO"] = WashSaleLockout{
		Symbol:        "VOO",
		SaleDate:      time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		LockoutUntil:  time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC),
		EarliestRebuy: time.Date(2026, 12, 22, 0, 0, 0, 0, time.UTC),
		RealizedLoss:  -12000,
	}

	syms := ActiveSymbols(cfg, ledger)
	hasSCHX := false
	for _, s := range syms {
		if s == "SCHX" {
			hasSCHX = true
		}
		if s == "VOO" {
			t.Fatalf("did not expect rotated-out VOO in ActiveSymbols: %v", syms)
		}
	}
	if !hasSCHX {
		t.Fatalf("expected SCHX in ActiveSymbols, got %v", syms)
	}

	market := baseMarket(-0.05, -0.05, -0.05, -0.05, 6.00)
	market.Quotes["SCHX"] = Quote{Symbol: "SCHX", TwoYearHigh: 32.0, Price: 25.0, DrawdownPct: -0.21875}

	rep := Evaluate("Test", cfg, ledger, market, now, false)
	var gotTLH, gotCrash bool
	for _, a := range rep.Alerts {
		if a.Code == "TLH_READY_SCHX" {
			gotTLH = true
			if !strings.Contains(a.Title, "-> IVV") {
				t.Fatalf("expected unlocked twin IVV when VOO is locked, got: %s", a.Title)
			}
		}
		if a.Code == "CORE_CRASH_APPROVED_SCHX" && a.Severity == "HIGH" {
			gotCrash = true
		}
	}
	if !gotTLH || !gotCrash {
		t.Fatalf("expected both TLH_READY_SCHX and CORE_CRASH_APPROVED_SCHX, got %+v", rep.Alerts)
	}
}

func TestCrossAccountWashSaleWarningAndFlat5kTLHFloor(t *testing.T) {
	now := time.Date(2027, 6, 15, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(now, 0)
	ledger.OtherAccountSym["VOO"] = []string{"Assets:Retirement:RothIRA:Investments:VOO"}
	market := baseMarket(-0.15, -0.02, -0.02, -0.40, 6.00)
	// VOO: 800 * 475 = 380k vs 400k basis = -$20k loss; QQQ: 20 * 330 = 6.6k vs 10k = -$3.4k (suppressed < $5k)
	market.Quotes["VOO"] = Quote{Symbol: "VOO", TwoYearHigh: 550.0, Price: 475.0, DrawdownPct: -0.136}
	market.Quotes["QQQ"] = Quote{Symbol: "QQQ", TwoYearHigh: 550.0, Price: 330.0, DrawdownPct: -0.40}

	rep := Evaluate("Test", cfg, ledger, market, now, false)
	var gotWarning bool
	for _, a := range rep.Alerts {
		if a.Code == "TLH_READY_QQQ" {
			t.Fatalf("expected QQQ (-$3,400) suppressed under flat -$5,000 TLH floor: %+v", a)
		}
		if a.Code == "TLH_READY_VOO" && strings.Contains(a.Detail, "CROSS-ACCOUNT WASH-SALE WARNING") && strings.Contains(a.Detail, "Rev. Rul. 2008-5") {
			gotWarning = true
		}
	}
	if !gotWarning {
		t.Fatalf("expected cross-account wash-sale warning on TLH_READY_VOO, got %+v", rep.Alerts)
	}
}

func TestWeekendWashSaleUnlockCaughtOnMonday(t *testing.T) {
	monday := time.Date(2026, 12, 21, 11, 30, 0, 0, time.UTC)
	cfg := testPolicyConfig()
	ledger := baseTestSnapshot(monday, -100000)
	ledger.Lockouts["IAUM"] = WashSaleLockout{
		Symbol:        "IAUM",
		SaleDate:      time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
		LockoutUntil:  time.Date(2026, 12, 19, 0, 0, 0, 0, time.UTC),
		EarliestRebuy: time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC),
		RealizedLoss:  -15000.0,
	}
	market := baseMarket(-0.05, -0.05, -0.10, -0.05, 6.25)

	rep := Evaluate("Test", cfg, ledger, market, monday, false)
	var unlocked bool
	for _, a := range rep.Alerts {
		if a.Code == "LOCKOUT_EXPIRED_IAUM_20261220" && a.Severity == "MEDIUM" {
			unlocked = true
		}
	}
	if !unlocked {
		t.Fatalf("expected weekend unlock alert LOCKOUT_EXPIRED_IAUM_20261220 on Monday, got %+v", rep.Alerts)
	}
	if len(rep.ActiveLockouts) != 0 {
		t.Fatalf("expected already-unlocked IAUM excluded from ActiveLockouts, got %+v", rep.ActiveLockouts)
	}
}

func TestYahooFailoverFromPrimary429ToFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"regularMarketPrice":480.0,"previousClose":485.0,"fiftyTwoWeekHigh":550.0,"fiftyTwoWeekLow":420.0,"regularMarketTime":1763660000},"indicators":{"quote":[{"high":[520.0,560.0,540.0]}]}}]}}`))
	}))
	defer fallback.Close()

	md, err := FetchMarketDataWithFallback(context.Background(), primary.Client(), primary.URL, fallback.URL, 0, 0, 2.25, 6.25, []string{"VOO"})
	if err != nil {
		t.Fatalf("expected failover to succeed, got error: %v", err)
	}
	q := md.Quotes["VOO"]
	if q.Price != 480.0 || q.TwoYearHigh != 560.0 {
		t.Fatalf("unexpected failover quote: %+v", q)
	}
}

func TestReinvestSharesReplayInSchwabCSV(t *testing.T) {
	tmp := t.TempDir()
	csvPath := filepath.Join(tmp, "2026-schwab-brokerage-activity.csv")
	content := strings.Join([]string{
		`"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"`,
		`"11/10/2026","Reinvest Shares","VOO","VANGUARD","2","$500.00","","-$1,000.00"`,
		`"10/01/2026","Buy","VOO","VANGUARD","100","$510.00","","-$51,000.00"`,
	}, "\n")
	if err := os.WriteFile(csvPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	snap := &LedgerSnapshot{
		Positions: make(map[string]float64),
		Lots:      make(map[string][]TaxLot),
		Lockouts:  make(map[string]WashSaleLockout),
	}
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if err := LoadSchwabActivityCSV(filepath.Join(tmp, "*-schwab-brokerage-activity.csv"), []string{"SNSXX"}, now, snap); err != nil {
		t.Fatalf("LoadSchwabActivityCSV failed: %v", err)
	}
	if len(snap.Lots["VOO"]) != 2 {
		t.Fatalf("expected 2 lots (Buy + Reinvest Shares), got %+v", snap.Lots["VOO"])
	}
	totalCost := snap.Lots["VOO"][0].TotalCost + snap.Lots["VOO"][1].TotalCost
	if totalCost != 52000.0 {
		t.Fatalf("expected total cost 52000.0, got %.2f", totalCost)
	}
}

func TestDeadManFailureAlertingAndResendSafetyGuard(t *testing.T) {
	now := time.Date(2027, 6, 15, 11, 30, 0, 0, time.UTC)
	st := &AlertState{SentAt: map[string]time.Time{
		"CORE_CRASH_APPROVED_VOO": now.Add(-48 * time.Hour),
	}}

	alerts := []AlertItem{
		{Severity: "HIGH", Code: "CORE_CRASH_APPROVED_VOO", Title: "dup"},
		{Severity: "MEDIUM", Code: "TLH_READY_VO", Title: "fresh"},
		{Severity: "INFO", Code: "CORE_CRASH_BLOCKED_GLDM", Title: "info skipped"},
	}
	filtered := FilterUnsentAlerts(alerts, st, 7, now)
	if len(filtered) != 1 || filtered[0].Code != "TLH_READY_VO" {
		t.Fatalf("unexpected dedup result: %+v", filtered)
	}

	if RecordFailureAndShouldAlert(st, 2, "yahoo 429", now) {
		t.Fatalf("first failure should not trigger dead-man alert")
	}
	if !RecordFailureAndShouldAlert(st, 2, "yahoo 429 again", now.Add(24*time.Hour)) {
		t.Fatalf("second consecutive failure should trigger dead-man alert")
	}
	st.SentAt[HealthFailAlertCode] = now.Add(24 * time.Hour)
	if RecordFailureAndShouldAlert(st, 2, "yahoo 429 third", now.Add(48*time.Hour)) {
		t.Fatalf("72h cooldown should suppress repeated health alert")
	}
	RecordSuccess(st, now.Add(72*time.Hour))
	if st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Fatalf("expected RecordSuccess to reset failures, got %+v", st)
	}

	if err := SendResendEmail(context.Background(), nil, "re_fake", "a@b.com", "c@d.com", "sub", "body"); err == nil {
		t.Fatalf("expected testing.Testing() guard to block live Resend send")
	}

	tmp := filepath.Join(t.TempDir(), "state.json")
	if err := SaveAlertState(tmp, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	loaded := LoadAlertState(tmp)
	if _, ok := loaded.SentAt["CORE_CRASH_APPROVED_VOO"]; !ok || loaded.LastSuccess.IsZero() {
		t.Fatalf("expected saved alert state and LastSuccess to round-trip, got %+v", loaded)
	}
}
