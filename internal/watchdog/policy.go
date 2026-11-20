package watchdog

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ActiveSymbols returns all live symbols to fetch from Yahoo: every held twin across sleeves,
// falling back to a sleeve's primary twin when the sleeve holds zero shares.
func ActiveSymbols(cfg PolicyConfig, ledger *LedgerSnapshot) []string {
	seen := make(map[string]bool)
	var syms []string
	add := func(sym string) {
		if sym == "" || sym == "CASH_31D" || seen[sym] {
			return
		}
		seen[sym] = true
		syms = append(syms, sym)
	}
	for _, sc := range cfg.Sleeves {
		heldAny := false
		for _, tw := range sc.Twins {
			if tw.Symbol == "CASH_31D" {
				continue
			}
			if ledger != nil && ledger.Positions[tw.Symbol] > 1e-6 {
				add(tw.Symbol)
				heldAny = true
			}
		}
		if !heldAny && len(sc.Twins) > 0 {
			add(sc.Twins[0].Symbol)
		}
	}
	return syms
}

// AlertItem captures a single actionable or informational alert.
type AlertItem struct {
	Severity string // "HIGH", "MEDIUM", "INFO"
	Code     string // Stable dedup key
	Title    string
	Detail   string
}

// PositionSummary summarizes a single active holding (or zero-share primary sleeve proxy) at live prices.
type PositionSummary struct {
	Sleeve         SleeveConfig
	Symbol         string
	Shares         float64
	Price          float64
	MarketValue    float64
	CostBasis      float64
	UnrealizedGL   float64
	UnrealizedPct  float64
	TwoYearHigh    float64
	DrawdownPct    float64
	ShareOfCore    float64
	ShareOfGross   float64
	HasRecentBuy30 bool
}

// EvaluationReport is the full deterministic output of running the 5-gate policy.
type EvaluationReport struct {
	PortfolioName   string
	EvaluatedAt     time.Time
	GrossETFValue   float64
	CoreValue       float64
	CashBalance     float64 // negative = margin debit
	MarginDebit     float64 // positive magnitude of debit
	NetEquity       float64
	LTV             float64
	EstimatedAPR    float64
	VIX             float64
	Positions       []PositionSummary
	ActiveLockouts  []WashSaleLockout // Still locked as of todayUTC
	Alerts          []AlertItem
	IsMonthlyDigest bool
}

func (r *EvaluationReport) HasActionableAlerts() bool {
	for _, a := range r.Alerts {
		if a.Severity == "HIGH" || a.Severity == "MEDIUM" {
			return true
		}
	}
	return false
}

func isLockedOn(lockouts map[string]WashSaleLockout, sym string, todayUTC time.Time) (WashSaleLockout, bool) {
	lo, ok := lockouts[sym]
	if !ok {
		return WashSaleLockout{}, false
	}
	if !todayUTC.After(lo.LockoutUntil) {
		return lo, true
	}
	return lo, false
}

func pickUnlockedReplacement(sc SleeveConfig, currentSym string, lockouts map[string]WashSaleLockout, todayUTC time.Time) (chosen TwinETF, locked bool, lockout WashSaleLockout) {
	var earliestTwin TwinETF
	var earliestLO WashSaleLockout
	hasLockedCandidate := false

	for _, tw := range sc.Twins {
		if tw.Symbol == currentSym {
			continue
		}
		if tw.Symbol == "CASH_31D" {
			return tw, false, WashSaleLockout{}
		}
		if lo, isL := isLockedOn(lockouts, tw.Symbol, todayUTC); isL {
			if !hasLockedCandidate || lo.EarliestRebuy.Before(earliestLO.EarliestRebuy) {
				earliestTwin = tw
				earliestLO = lo
				hasLockedCandidate = true
			}
			continue
		}
		return tw, false, WashSaleLockout{}
	}
	if hasLockedCandidate {
		return earliestTwin, true, earliestLO
	}
	return TwinETF{Symbol: "CASH_31D", Reason: "All twins locked; park cash 31d"}, false, WashSaleLockout{}
}

// Evaluate runs all 5 gates over the live ledger snapshot and live market quotes.
func Evaluate(portfolioName string, cfg PolicyConfig, ledger *LedgerSnapshot, market *MarketData, now time.Time, forceDigest bool) *EvaluationReport {
	if portfolioName == "" {
		portfolioName = "Portfolio"
	}
	rep := &EvaluationReport{
		PortfolioName: portfolioName,
		EvaluatedAt:   now,
		CashBalance:   ledger.CashBalance,
		EstimatedAPR:  market.EstimatedAPR,
		VIX:           market.VIX,
	}
	if ledger.CashBalance < 0 {
		rep.MarginDebit = -ledger.CashBalance
	}

	todayUTC := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	thirtyDaysAgo := todayUTC.AddDate(0, 0, -30)

	// 1. Aggregate active positions across sleeve twins
	for _, sc := range cfg.Sleeves {
		var activeSyms []string
		for _, tw := range sc.Twins {
			if tw.Symbol == "CASH_31D" {
				continue
			}
			if ledger.Positions[tw.Symbol] > 1e-6 {
				activeSyms = append(activeSyms, tw.Symbol)
			}
		}
		if len(activeSyms) == 0 && len(sc.Twins) > 0 {
			activeSyms = append(activeSyms, sc.Twins[0].Symbol)
		}

		for _, sym := range activeSyms {
			sh := ledger.Positions[sym]
			q := market.Quotes[sym]
			mv := sh * q.Price
			cost := 0.0
			recentBuy := false
			for _, lot := range ledger.Lots[sym] {
				cost += lot.TotalCost
				if !lot.Date.IsZero() && !lot.Date.Before(thirtyDaysAgo) {
					recentBuy = true
				}
			}
			gl := mv - cost
			glPct := 0.0
			if cost > 0 {
				glPct = gl / cost
			}
			rep.GrossETFValue += mv
			if sc.IsCore {
				rep.CoreValue += mv
			}
			rep.Positions = append(rep.Positions, PositionSummary{
				Sleeve:         sc,
				Symbol:         sym,
				Shares:         sh,
				Price:          q.Price,
				MarketValue:    mv,
				CostBasis:      cost,
				UnrealizedGL:   gl,
				UnrealizedPct:  glPct,
				TwoYearHigh:    q.TwoYearHigh,
				DrawdownPct:    q.DrawdownPct,
				HasRecentBuy30: recentBuy,
			})
		}
	}

	rep.NetEquity = rep.GrossETFValue + rep.CashBalance
	if rep.GrossETFValue > 0 {
		rep.LTV = rep.MarginDebit / rep.GrossETFValue
	}
	for i := range rep.Positions {
		if rep.GrossETFValue > 0 {
			rep.Positions[i].ShareOfGross = rep.Positions[i].MarketValue / rep.GrossETFValue
		}
		if rep.Positions[i].Sleeve.IsCore && rep.CoreValue > 0 {
			rep.Positions[i].ShareOfCore = rep.Positions[i].MarketValue / rep.CoreValue
		}
	}

	var allLockouts []WashSaleLockout
	for _, lo := range ledger.Lockouts {
		allLockouts = append(allLockouts, lo)
		if !todayUTC.After(lo.LockoutUntil) {
			rep.ActiveLockouts = append(rep.ActiveLockouts, lo)
		}
	}
	sortLockouts := func(xs []WashSaleLockout) {
		sort.Slice(xs, func(i, j int) bool {
			if !xs[i].EarliestRebuy.Equal(xs[j].EarliestRebuy) {
				return xs[i].EarliestRebuy.Before(xs[j].EarliestRebuy)
			}
			return xs[i].Symbol < xs[j].Symbol
		})
	}
	sortLockouts(allLockouts)
	sortLockouts(rep.ActiveLockouts)

	// 2. Gate 1-3: Core-Only Deep-Crash Detection (deduped by sleeve)
	type crashedSleeve struct {
		sleeve SleeveConfig
		buyPos PositionSummary
	}
	seenCrashSleeve := make(map[string]bool)
	var crashedCore []crashedSleeve
	broadEqDD := 0.0
	for _, ps := range rep.Positions {
		if ps.Sleeve.IsBroadEquity && ps.DrawdownPct < broadEqDD {
			broadEqDD = ps.DrawdownPct
		}
		if ps.Sleeve.IsCore && ps.DrawdownPct <= ps.Sleeve.CrashThreshold && !seenCrashSleeve[ps.Sleeve.ID] {
			buyPS := ps
			if _, locked := isLockedOn(ledger.Lockouts, ps.Symbol, todayUTC); locked {
				if alt, altLocked, _ := pickUnlockedReplacement(ps.Sleeve, ps.Symbol, ledger.Lockouts, todayUTC); !altLocked && alt.Symbol != "CASH_31D" {
					if q, ok := market.Quotes[alt.Symbol]; ok && q.Price > 0 {
						buyPS.Symbol = alt.Symbol
						buyPS.Price = q.Price
						buyPS.TwoYearHigh = q.TwoYearHigh
						buyPS.DrawdownPct = q.DrawdownPct
					}
				}
			}
			seenCrashSleeve[ps.Sleeve.ID] = true
			crashedCore = append(crashedCore, crashedSleeve{sleeve: ps.Sleeve, buyPos: buyPS})
		}
	}

	if len(crashedCore) > 0 {
		inTaxWall := !cfg.TaxWallStart.IsZero() && !cfg.TaxWallEnd.IsZero() &&
			!todayUTC.Before(cfg.TaxWallStart) && !todayUTC.After(cfg.TaxWallEnd)
		isBroadPanic := broadEqDD <= cfg.BroadCrashEqDD
		ltvCap := cfg.SectorLTVCap
		if isBroadPanic {
			ltvCap = cfg.BroadCrashLTVCap
		}

		var blockers []string
		if inTaxWall && !isBroadPanic {
			blockers = append(blockers, fmt.Sprintf("Gate 2 Tax-Wall Blackout active (%s to %s)", cfg.TaxWallStart.Format("2006-01-02"), cfg.TaxWallEnd.Format("2006-01-02")))
		}
		if rep.LTV > cfg.CooldownLTVMax && !isBroadPanic {
			blockers = append(blockers, fmt.Sprintf("Gate 2 Cooldown active: LTV %.1f%% > %.1f%% (pay down margin first unless broad US equities hit <= %.0f%%)", rep.LTV*100, cfg.CooldownLTVMax*100, cfg.BroadCrashEqDD*100))
		}
		if market.EstimatedAPR >= cfg.StagflationAPRBlock {
			blockers = append(blockers, fmt.Sprintf("Gate 3 Stagflation APR Block: est. margin APR %.2f%% >= %.2f%%", market.EstimatedAPR, cfg.StagflationAPRBlock))
		}

		maxAdd := math.Max(0, (ltvCap*rep.GrossETFValue-rep.MarginDebit)/(1.0-ltvCap))

		var syms []string
		var lines []string
		for _, cs := range crashedCore {
			ps := cs.buyPos
			syms = append(syms, ps.Symbol)
			lines = append(lines, fmt.Sprintf("  - %s (%s): $%.2f (%.1f%% from 2y high $%.2f; threshold <= %.0f%%)",
				ps.Symbol, cs.sleeve.Name, ps.Price, ps.DrawdownPct*100, ps.TwoYearHigh, cs.sleeve.CrashThreshold*100))
		}

		minTranche := cfg.MinTrancheDollars
		if minTranche <= 0 {
			minTranche = 10000.0
		}

		if len(blockers) == 0 && maxAdd >= minTranche {
			wSum := 0.0
			for _, cs := range crashedCore {
				wSum += cs.sleeve.CoreTargetW
			}
			lines = append(lines, fmt.Sprintf("  - Approved Capped Margin Capacity (<= %.0f%% LTV): $%.0f", ltvCap*100, maxAdd))
			for _, cs := range crashedCore {
				ps := cs.buyPos
				if ps.Price <= 0 || wSum <= 0 {
					continue
				}
				alloc := maxAdd * (cs.sleeve.CoreTargetW / wSum)
				shBuy := math.Floor(alloc / ps.Price)
				lines = append(lines, fmt.Sprintf("  - Suggested Core Buy: %.0f sh %s (~$%.0f @ $%.2f)", shBuy, ps.Symbol, shBuy*ps.Price, ps.Price))
			}
			rep.Alerts = append(rep.Alerts, AlertItem{
				Severity: "HIGH",
				Code:     "CORE_CRASH_APPROVED_" + strings.Join(syms, "_"),
				Title:    fmt.Sprintf("DEEP CRASH GATE CLEARED: %s", strings.Join(syms, ", ")),
				Detail:   strings.Join(lines, "\n"),
			})
		} else {
			lines = append(lines, "  - Margin Deployment BLOCKED (Stay in Cash-Flow Paydown Mode):")
			for _, b := range blockers {
				lines = append(lines, "    * "+b)
			}
			if maxAdd < minTranche {
				lines = append(lines, fmt.Sprintf("    * Available headroom under %.0f%% LTV cap is $%.0f (< $%.0f minimum tranche)", ltvCap*100, maxAdd, minTranche))
			}
			rep.Alerts = append(rep.Alerts, AlertItem{
				Severity: "INFO",
				Code:     "CORE_CRASH_BLOCKED_" + strings.Join(syms, "_"),
				Title:    fmt.Sprintf("Core Drawdown Observed (%s) - Margin Blocked by Guardrails", strings.Join(syms, ", ")),
				Detail:   strings.Join(lines, "\n"),
			})
		}
	}

	// 3. Gate 4A: Tax-Loss Harvest Scanner
	for _, ps := range rep.Positions {
		if ps.Shares <= 0 || ps.CostBasis <= 0 {
			continue
		}
		if ps.UnrealizedGL > -cfg.TLHMinLossDollars {
			continue
		}

		repl, replLocked, replLO := pickUnlockedReplacement(ps.Sleeve, ps.Symbol, ledger.Lockouts, todayUTC)
		var notes []string
		if ps.HasRecentBuy30 {
			notes = append(notes, "  - Note: Lot purchased within last 30 days; must sell 100% of shares to avoid partial wash sale.")
		}
		if otherAccts := ledger.OtherAccountSym[ps.Symbol]; len(otherAccts) > 0 {
			notes = append(notes, fmt.Sprintf("  - CROSS-ACCOUNT WASH-SALE WARNING: %s is also held in %s. Verify zero buys or DRIP within +/-30 days (IRA/Roth wash sales permanently disallow losses under Rev. Rul. 2008-5).",
				ps.Symbol, strings.Join(otherAccts, ", ")))
		}
		if repl.Symbol != "CASH_31D" {
			if otherRepl := ledger.OtherAccountSym[repl.Symbol]; len(otherRepl) > 0 {
				notes = append(notes, fmt.Sprintf("  - Cross-Account Note: replacement %s is also held in %s; keep DRIP disabled if rotating again later.",
					repl.Symbol, strings.Join(otherRepl, ", ")))
			}
		}

		extra := ""
		if len(notes) > 0 {
			extra = "\n" + strings.Join(notes, "\n")
		}

		if replLocked {
			rep.Alerts = append(rep.Alerts, AlertItem{
				Severity: "INFO",
				Code:     "TLH_BLOCKED_" + ps.Symbol,
				Title:    fmt.Sprintf("TLH Candidate %s ($%.0f / %.1f%%) - All Replacement Twins Locked Out", ps.Symbol, ps.UnrealizedGL, ps.UnrealizedPct*100),
				Detail: fmt.Sprintf("  - Earliest unlocking twin: %s locked until %s (earliest buy %s)%s",
					repl.Symbol, replLO.LockoutUntil.Format("2006-01-02"), replLO.EarliestRebuy.Format("2006-01-02"), extra),
			})
		} else {
			rep.Alerts = append(rep.Alerts, AlertItem{
				Severity: "MEDIUM",
				Code:     "TLH_READY_" + ps.Symbol,
				Title:    fmt.Sprintf("TLH Opportunity: Sell 100%% %s (%.0f sh, $%.0f / %.1f%%) -> %s", ps.Symbol, ps.Shares, ps.UnrealizedGL, ps.UnrealizedPct*100, repl.Symbol),
				Detail: fmt.Sprintf("  - Current: $%.2f | Basis: $%.2f | Replacement: %s (%s)%s",
					ps.Price, ps.CostBasis, repl.Symbol, repl.Reason, extra),
			})
		}
	}

	// 4. Gate 4B: Wash-Sale Lockout Unlock Alerts (4-day window catches Sat/Sun/holiday unlocks)
	for _, lo := range allLockouts {
		if !todayUTC.Before(lo.EarliestRebuy) && todayUTC.Before(lo.EarliestRebuy.AddDate(0, 0, 4)) {
			rep.Alerts = append(rep.Alerts, AlertItem{
				Severity: "MEDIUM",
				Code:     "LOCKOUT_EXPIRED_" + lo.Symbol + "_" + lo.EarliestRebuy.Format("20060102"),
				Title:    fmt.Sprintf("30-Day Wash-Sale Lockout Cleared: %s", lo.Symbol),
				Detail: fmt.Sprintf("  - Sold %s on %s ($%.2f realized loss). Safe to repurchase starting %s.",
					lo.Symbol, lo.SaleDate.Format("2006-01-02"), lo.RealizedLoss, lo.EarliestRebuy.Format("2006-01-02")),
			})
		}
	}

	// 5. Monthly 1st-of-Month Paydown Digest
	digestEnabled := cfg.MonthlyDigestEnabled == nil || *cfg.MonthlyDigestEnabled
	isFirstBusinessDay := (now.Day() == 1 && now.Weekday() >= time.Monday && now.Weekday() <= time.Friday) ||
		(now.Day() == 2 && now.Weekday() == time.Monday) ||
		(now.Day() == 3 && now.Weekday() == time.Monday)
	if forceDigest || (digestEnabled && isFirstBusinessDay) {
		rep.IsMonthlyDigest = true
		estMonthlyInt := rep.MarginDebit * (rep.EstimatedAPR / 100.0) / 12.0
		rep.Alerts = append(rep.Alerts, AlertItem{
			Severity: "INFO",
			Code:     fmt.Sprintf("MONTHLY_DIGEST_%04d%02d", now.Year(), now.Month()),
			Title:    fmt.Sprintf("Monthly Margin Paydown Digest (%s)", now.Format("January 2006")),
			Detail: fmt.Sprintf("  - Margin Debit: $%.2f (%.2f%% LTV) | Est. Monthly Interest @ %.2f%%: $%.2f\n  - Action: %s",
				rep.MarginDebit, rep.LTV*100, rep.EstimatedAPR, estMonthlyInt, cfg.MonthlySweepNote),
		})
	}

	return rep
}

// FormatPlainText renders a crisp, terminal- and email-friendly briefing.
func FormatPlainText(rep *EvaluationReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CRASHGATE: %s\n", strings.ToUpper(rep.PortfolioName))
	fmt.Fprintf(&b, "Evaluated: %s\n", rep.EvaluatedAt.Format("Mon, Jan 2, 2006 3:04 PM MST"))
	fmt.Fprintf(&b, "Gross ETFs: $%.2f | Core: $%.2f | Margin Debit: -$%.2f (%.2f%% LTV) | Net Equity: $%.2f\n",
		rep.GrossETFValue, rep.CoreValue, rep.MarginDebit, rep.LTV*100, rep.NetEquity)
	fmt.Fprintf(&b, "Macro Context: Est. Margin APR %.2f%% | VIX %.2f\n\n", rep.EstimatedAPR, rep.VIX)

	fmt.Fprintf(&b, "ALERTS (%d)\n", len(rep.Alerts))
	if len(rep.Alerts) == 0 {
		fmt.Fprintf(&b, "  None. All gates nominal; stay in cash-flow paydown mode.\n\n")
	} else {
		for _, a := range rep.Alerts {
			fmt.Fprintf(&b, "[%s] %s\n%s\n\n", a.Severity, a.Title, a.Detail)
		}
	}

	fmt.Fprintf(&b, "LIVE PORTFOLIO & 2-YEAR DRAWDOWN STATUS\n")
	fmt.Fprintf(&b, "%-6s %7s %9s %11s %7s %8s %9s %11s\n", "SYM", "SHARES", "PRICE", "MKT VAL", "CORE%", "GROSS%", "2Y DD%", "UNREAL G/L")
	for _, ps := range rep.Positions {
		coreStr := "   N/A"
		if ps.Sleeve.IsCore {
			coreStr = fmt.Sprintf("%6.2f%%", ps.ShareOfCore*100)
		}
		fmt.Fprintf(&b, "%-6s %7.0f %9.3f %11.2f %7s %7.2f%% %8.1f%% %+11.2f\n",
			ps.Symbol, ps.Shares, ps.Price, ps.MarketValue, coreStr, ps.ShareOfGross*100, ps.DrawdownPct*100, ps.UnrealizedGL)
	}

	if len(rep.ActiveLockouts) > 0 {
		fmt.Fprintf(&b, "\nACTIVE 30-DAY IRC § 1091 WASH-SALE LOCKOUTS\n")
		for _, lo := range rep.ActiveLockouts {
			fmt.Fprintf(&b, "  - %-5s locked through %s (earliest buy: %s; sold %s for $%.2f)\n",
				lo.Symbol, lo.LockoutUntil.Format("2006-01-02"), lo.EarliestRebuy.Format("2006-01-02"), lo.SaleDate.Format("2006-01-02"), lo.RealizedLoss)
		}
	}
	return b.String()
}
