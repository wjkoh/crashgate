package watchdog

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TaxLot represents a single open FIFO lot reconstructed from brokerage CSV activity or hledger cost basis.
type TaxLot struct {
	Date      time.Time
	Shares    float64
	UnitCost  float64
	TotalCost float64
}

// WashSaleLockout tracks a symbol sold at a loss within the 30-day IRC § 1091 window.
type WashSaleLockout struct {
	Symbol        string
	SaleDate      time.Time
	LockoutUntil  time.Time // Last blocked day (SaleDate + 30 days)
	EarliestRebuy time.Time // SaleDate + 31 days
	RealizedLoss  float64
}

// LedgerSnapshot holds live brokerage balances, tax lots, lockouts, and cross-account holdings.
type LedgerSnapshot struct {
	CashBalance     float64                    // Negative means margin debit
	Positions       map[string]float64         // Symbol -> shares from hledger
	Lots            map[string][]TaxLot        // Symbol -> open FIFO tax lots (or hledger aggregate cost basis)
	Lockouts        map[string]WashSaleLockout // Symbol -> active or recently-unlocked wash-sale lockout
	OtherAccountSym map[string][]string        // Symbol -> non-primary accounts holding it
	AsOf            time.Time
}

func parseMoney(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// LoadLedger queries hledger for authoritative brokerage balances and cross-account holdings.
// When hledger.schwab_activity_glob is configured, it also replays FIFO tax lots and 30-day wash-sale lockouts;
// otherwise it falls back to hledger cost basis (`hledger bal -B`).
func LoadLedger(hcfg HledgerConfig, hledgerBin string, now time.Time) (*LedgerSnapshot, error) {
	snap := &LedgerSnapshot{
		Positions:       make(map[string]float64),
		Lots:            make(map[string][]TaxLot),
		Lockouts:        make(map[string]WashSaleLockout),
		OtherAccountSym: make(map[string][]string),
		AsOf:            now,
	}

	if err := loadHledgerBalances(hcfg, hledgerBin, snap); err != nil {
		return nil, err
	}

	if strings.TrimSpace(hcfg.SchwabActivityGlob) != "" {
		if err := LoadSchwabActivityCSV(hcfg.SchwabActivityGlob, hcfg.IgnoreSymbols, now, snap); err != nil {
			return nil, err
		}
	} else {
		// Generic hledger-only fallback: read aggregate cost basis via `hledger bal -B`
		_ = loadHledgerCostBasis(hcfg, hledgerBin, snap)
	}
	return snap, nil
}

func loadHledgerBalances(hcfg HledgerConfig, hledgerBin string, snap *LedgerSnapshot) error {
	if hledgerBin == "" {
		hledgerBin = "hledger"
	}
	// 1. Primary brokerage balances (native units)
	cmd := exec.Command(hledgerBin, "-f", hcfg.Journal, "bal", "-O", "csv", "--no-total", hcfg.BrokerageAccount)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("hledger bal %s failed: %w", hcfg.BrokerageAccount, err)
	}
	r := csv.NewReader(bytes.NewReader(out))
	rows, err := r.ReadAll()
	if err != nil {
		return fmt.Errorf("parse hledger csv: %w", err)
	}
	for i, row := range rows {
		if i == 0 || len(row) < 2 {
			continue
		}
		acct := strings.TrimSpace(row[0])
		balStr := strings.TrimSpace(row[1])
		if acct == hcfg.CashAccount {
			snap.CashBalance = parseMoney(balStr)
		} else if strings.HasPrefix(acct, hcfg.InvestmentsPrefix) {
			sym := strings.TrimPrefix(acct, hcfg.InvestmentsPrefix)
			fields := strings.Fields(balStr)
			if len(fields) >= 1 {
				q := parseMoney(fields[0])
				if math.Abs(q) > 1e-6 {
					snap.Positions[sym] = q
				}
			}
		}
	}

	// 2. Cross-account holdings (external brokerages, IRAs, 401k, HSA) for IRC § 1091 collision awareness
	if len(hcfg.OtherAccounts) > 0 {
		args := append([]string{"-f", hcfg.Journal, "bal", "-O", "csv", "--no-total"}, hcfg.OtherAccounts...)
		cmdOther := exec.Command(hledgerBin, args...)
		if outOther, err := cmdOther.Output(); err == nil {
			r2 := csv.NewReader(bytes.NewReader(outOther))
			if rows2, err2 := r2.ReadAll(); err2 == nil {
				for i, row := range rows2 {
					if i == 0 || len(row) < 2 {
						continue
					}
					acct := strings.TrimSpace(row[0])
					if !strings.Contains(acct, ":Investments:") {
						continue
					}
					idx := strings.LastIndex(acct, ":")
					if idx < 0 {
						continue
					}
					sym := acct[idx+1:]
					fields := strings.Fields(strings.TrimSpace(row[1]))
					if len(fields) >= 1 && parseMoney(fields[0]) > 1e-6 {
						snap.OtherAccountSym[sym] = append(snap.OtherAccountSym[sym], acct)
					}
				}
			}
		}
	}
	return nil
}

func loadHledgerCostBasis(hcfg HledgerConfig, hledgerBin string, snap *LedgerSnapshot) error {
	if hledgerBin == "" {
		hledgerBin = "hledger"
	}
	cmd := exec.Command(hledgerBin, "-f", hcfg.Journal, "bal", "-B", "-O", "csv", "--no-total", hcfg.InvestmentsPrefix)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	rows, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		return err
	}
	for i, row := range rows {
		if i == 0 || len(row) < 2 {
			continue
		}
		acct := strings.TrimSpace(row[0])
		if !strings.HasPrefix(acct, hcfg.InvestmentsPrefix) {
			continue
		}
		sym := strings.TrimPrefix(acct, hcfg.InvestmentsPrefix)
		sh := snap.Positions[sym]
		cost := parseMoney(row[1])
		if sh > 1e-6 && cost > 0 {
			snap.Lots[sym] = []TaxLot{{
				Shares:    sh,
				UnitCost:  cost / sh,
				TotalCost: cost,
			}}
		}
	}
	return nil
}

// LoadSchwabActivityCSV replays chronological Buy, Reinvest Shares, and Sell rows from Schwab activity CSVs
// to reconstruct FIFO tax lots and 30-day IRC § 1091 wash-sale lockouts.
func LoadSchwabActivityCSV(pattern string, ignoreSymbols []string, now time.Time, snap *LedgerSnapshot) error {
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob schwab activity csv: %w", err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no schwab brokerage activity csv found matching %s", pattern)
	}

	ignored := make(map[string]bool, len(ignoreSymbols))
	for _, s := range ignoreSymbols {
		ignored[strings.TrimSpace(s)] = true
	}

	type rawTx struct {
		date     time.Time
		action   string
		sym      string
		qty      float64
		price    float64
		amt      float64
		seqOrder int
	}
	var all []rawTx
	seq := 0
	for _, fpath := range files {
		data, err := os.ReadFile(fpath)
		if err != nil {
			return fmt.Errorf("read %s: %w", fpath, err)
		}
		recs, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
		if err != nil || len(recs) < 2 {
			continue
		}
		// Schwab CSV is reverse chronological; replay chronologically
		for i := len(recs) - 1; i >= 1; i-- {
			row := recs[i]
			if len(row) < 8 {
				continue
			}
			dt, err := time.Parse("01/02/2006", strings.TrimSpace(row[0]))
			if err != nil {
				continue
			}
			seq++
			all = append(all, rawTx{
				date:     dt,
				action:   strings.TrimSpace(row[1]),
				sym:      strings.TrimSpace(row[2]),
				qty:      parseMoney(row[4]),
				price:    parseMoney(row[5]),
				amt:      parseMoney(row[7]),
				seqOrder: seq,
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].date.Equal(all[j].date) {
			return all[i].date.Before(all[j].date)
		}
		return all[i].seqOrder < all[j].seqOrder
	})

	todayDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	for _, tx := range all {
		if tx.sym == "" || ignored[tx.sym] {
			continue
		}
		switch tx.action {
		case "Buy", "Reinvest Shares":
			cost := math.Abs(tx.amt)
			if tx.qty <= 0 {
				continue
			}
			snap.Lots[tx.sym] = append(snap.Lots[tx.sym], TaxLot{
				Date:      tx.date,
				Shares:    tx.qty,
				UnitCost:  cost / tx.qty,
				TotalCost: cost,
			})
		case "Sell":
			proceeds := math.Abs(tx.amt)
			rem := tx.qty
			disposedCost := 0.0
			lots := snap.Lots[tx.sym]
			for rem > 1e-6 && len(lots) > 0 {
				if lots[0].Shares <= rem+1e-6 {
					disposedCost += lots[0].TotalCost
					rem -= lots[0].Shares
					lots = lots[1:]
				} else {
					frac := rem / lots[0].Shares
					disposedCost += lots[0].TotalCost * frac
					lots[0].TotalCost -= lots[0].TotalCost * frac
					lots[0].Shares -= rem
					rem = 0
				}
			}
			snap.Lots[tx.sym] = lots
			realizedGL := proceeds - disposedCost
			if realizedGL < -1.0 {
				lockoutUntil := tx.date.AddDate(0, 0, 30)
				earliestRebuy := tx.date.AddDate(0, 0, 31)
				// Retain active lockouts plus a 3-day weekend/holiday catch-up window after earliestRebuy
				if !earliestRebuy.AddDate(0, 0, 3).Before(todayDate) {
					snap.Lockouts[tx.sym] = WashSaleLockout{
						Symbol:        tx.sym,
						SaleDate:      tx.date,
						LockoutUntil:  lockoutUntil,
						EarliestRebuy: earliestRebuy,
						RealizedLoss:  realizedGL,
					}
				}
			}
		}
	}
	return nil
}
