package watchdog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the top-level user configuration loaded from a JSON file.
type Config struct {
	PortfolioName string        `json:"portfolio_name"`
	Hledger       HledgerConfig `json:"hledger"`
	Policy        PolicyConfig  `json:"policy"`
	Notify        NotifyConfig  `json:"notify"`
}

// HledgerConfig tells crashgate where the local hledger journal and optional CSV exports live.
type HledgerConfig struct {
	Journal            string   `json:"journal"`
	BrokerageAccount   string   `json:"brokerage_account"`
	CashAccount        string   `json:"cash_account"`
	InvestmentsPrefix  string   `json:"investments_prefix"`
	OtherAccounts      []string `json:"other_accounts"`
	SchwabActivityGlob string   `json:"schwab_activity_glob"`
	IgnoreSymbols      []string `json:"ignore_symbols"`
}

// NotifyConfig configures optional transactional email delivery via Resend and local alert deduplication.
type NotifyConfig struct {
	To            string `json:"to"`
	From          string `json:"from"`
	SubjectPrefix string `json:"subject_prefix"`
	ResendKeyFile string `json:"resend_key_file"`
	StateFile     string `json:"state_file"`
}

// TwinETF defines one non-substantially-identical ETF option within a sleeve.
type TwinETF struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

// SleeveConfig defines target weights, deep-crash thresholds, and ordered TLH twins.
type SleeveConfig struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	IsCore         bool      `json:"is_core"`
	IsBroadEquity  bool      `json:"is_broad_equity"`
	CoreTargetW    float64   `json:"core_target_w"`
	CrashThreshold float64   `json:"crash_threshold"`
	Twins          []TwinETF `json:"twins"`
}

// PolicyConfig holds all tunable thresholds for the 5-gate watchdog.
type PolicyConfig struct {
	CooldownLTVMax       float64        `json:"cooldown_ltv_max"`
	SectorLTVCap         float64        `json:"sector_ltv_cap"`
	BroadCrashLTVCap     float64        `json:"broad_crash_ltv_cap"`
	BroadCrashEqDD       float64        `json:"broad_crash_eq_dd"`
	MinTrancheDollars    float64        `json:"min_tranche_dollars"`
	StagflationAPRBlock  float64        `json:"stagflation_apr_block"`
	MarginSpreadPct      float64        `json:"margin_spread_pct"`
	FallbackAPRPct       float64        `json:"fallback_apr_pct"`
	TaxWallStartStr      string         `json:"tax_wall_start"`
	TaxWallEndStr        string         `json:"tax_wall_end"`
	TLHMinLossDollars    float64        `json:"tlh_min_loss_dollars"`
	AlertCooldownDays    int            `json:"alert_cooldown_days"`
	HealthFailThreshold  int            `json:"health_fail_threshold"`
	MonthlyDigestEnabled *bool          `json:"monthly_digest_enabled"`
	MonthlySweepNote     string         `json:"monthly_sweep_note"`
	Sleeves              []SleeveConfig `json:"sleeves"`

	// Parsed dates (populated by Validate / ApplyDefaults)
	TaxWallStart time.Time `json:"-"`
	TaxWallEnd   time.Time `json:"-"`
}

// ExpandPath expands a leading ~/ to the user's home directory, and resolves relative paths against baseDir.
func ExpandPath(p, baseDir string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) || baseDir == "" {
		return p
	}
	return filepath.Join(baseDir, p)
}

// LoadConfig reads, defaults, expands paths, and validates a crashgate JSON configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	absCfg, err := filepath.Abs(path)
	baseDir := filepath.Dir(path)
	if err == nil {
		baseDir = filepath.Dir(absCfg)
	}
	if err := cfg.ApplyDefaultsAndValidate(baseDir); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaultsAndValidate fills sensible defaults, expands paths, and validates all fields.
func (c *Config) ApplyDefaultsAndValidate(baseDir string) error {
	if strings.TrimSpace(c.PortfolioName) == "" {
		c.PortfolioName = "Portfolio"
	}

	// Hledger defaults and path expansion
	c.Hledger.Journal = ExpandPath(c.Hledger.Journal, baseDir)
	if c.Hledger.Journal == "" {
		return fmt.Errorf("hledger.journal is required")
	}
	if strings.TrimSpace(c.Hledger.BrokerageAccount) == "" {
		c.Hledger.BrokerageAccount = "Assets:Brokerage"
	}
	if strings.TrimSpace(c.Hledger.CashAccount) == "" {
		c.Hledger.CashAccount = c.Hledger.BrokerageAccount + ":Cash"
	}
	if strings.TrimSpace(c.Hledger.InvestmentsPrefix) == "" {
		c.Hledger.InvestmentsPrefix = c.Hledger.BrokerageAccount + ":Investments:"
	}
	if !strings.HasSuffix(c.Hledger.InvestmentsPrefix, ":") {
		c.Hledger.InvestmentsPrefix += ":"
	}
	if c.Hledger.SchwabActivityGlob != "" {
		c.Hledger.SchwabActivityGlob = ExpandPath(c.Hledger.SchwabActivityGlob, baseDir)
	}

	// Policy defaults
	p := &c.Policy
	if p.CooldownLTVMax <= 0 {
		p.CooldownLTVMax = 0.08
	}
	if p.SectorLTVCap <= 0 {
		p.SectorLTVCap = 0.20
	}
	if p.BroadCrashLTVCap <= 0 {
		p.BroadCrashLTVCap = 0.25
	}
	if p.BroadCrashEqDD == 0 {
		p.BroadCrashEqDD = -0.30
	}
	if p.MinTrancheDollars <= 0 {
		p.MinTrancheDollars = 10000
	}
	if p.StagflationAPRBlock <= 0 {
		p.StagflationAPRBlock = 7.50
	}
	if p.MarginSpreadPct <= 0 {
		p.MarginSpreadPct = 2.25
	}
	if p.FallbackAPRPct <= 0 {
		p.FallbackAPRPct = 6.25
	}
	if p.TLHMinLossDollars <= 0 {
		p.TLHMinLossDollars = 5000
	}
	if p.AlertCooldownDays <= 0 {
		p.AlertCooldownDays = 7
	}
	if p.HealthFailThreshold <= 0 {
		p.HealthFailThreshold = 2
	}
	if p.MonthlyDigestEnabled == nil {
		t := true
		p.MonthlyDigestEnabled = &t
	}
	if strings.TrimSpace(p.MonthlySweepNote) == "" {
		p.MonthlySweepNote = "Sweep excess checking cash directly into brokerage margin cash."
	}

	if strings.TrimSpace(p.TaxWallStartStr) != "" || strings.TrimSpace(p.TaxWallEndStr) != "" {
		s, err := time.Parse("2006-01-02", strings.TrimSpace(p.TaxWallStartStr))
		if err != nil {
			return fmt.Errorf("invalid policy.tax_wall_start %q (expected YYYY-MM-DD): %w", p.TaxWallStartStr, err)
		}
		e, err := time.Parse("2006-01-02", strings.TrimSpace(p.TaxWallEndStr))
		if err != nil {
			return fmt.Errorf("invalid policy.tax_wall_end %q (expected YYYY-MM-DD): %w", p.TaxWallEndStr, err)
		}
		if e.Before(s) {
			return fmt.Errorf("policy.tax_wall_end (%s) precedes tax_wall_start (%s)", p.TaxWallEndStr, p.TaxWallStartStr)
		}
		p.TaxWallStart = s
		p.TaxWallEnd = e
	}

	if len(p.Sleeves) == 0 {
		return fmt.Errorf("policy.sleeves must define at least one sleeve")
	}
	for i := range p.Sleeves {
		sc := &p.Sleeves[i]
		if len(sc.Twins) == 0 {
			return fmt.Errorf("sleeve %q must define at least one twin ETF", sc.Name)
		}
		if sc.ID == "" {
			sc.ID = strings.ToLower(strings.ReplaceAll(sc.Name, " ", "-"))
		}
		if sc.CrashThreshold == 0 {
			sc.CrashThreshold = -0.20
		}
		if sc.CrashThreshold > 0 {
			sc.CrashThreshold = -sc.CrashThreshold
		}
	}

	// Notify defaults
	if strings.TrimSpace(c.Notify.SubjectPrefix) == "" {
		c.Notify.SubjectPrefix = "[CrashGate]"
	}
	if c.Notify.StateFile == "" {
		c.Notify.StateFile = "~/.local/state/crashgate/state.json"
	}
	c.Notify.StateFile = ExpandPath(c.Notify.StateFile, baseDir)
	if c.Notify.ResendKeyFile != "" {
		c.Notify.ResendKeyFile = ExpandPath(c.Notify.ResendKeyFile, baseDir)
	}
	return nil
}
