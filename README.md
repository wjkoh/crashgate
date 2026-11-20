# CrashGate

A zero-dependency, read-only Go watchdog for **deep-crash margin gating**, **tax-loss harvesting**, and **30-day IRC Section 1091 wash-sale tracking** built on top of a local plain-text [hledger](https://hledger.org/) database.

Read the full quantitative background and Monte Carlo simulation behind this architecture:
- [Why Margin Rebalancing Fails, and When Deep-Crash Leverage Wins](https://wjkoh.com/posts/why-margin-rebalancing-fails-deep-crash-leverage/)
- [The Agentic Ledger: Managing Personal Finance with hledger and AI](https://wjkoh.com/posts/the-agentic-ledger/)

## Why Read-Only?

Wiring an autonomous trading bot directly to a retail brokerage API is usually counterproductive: individual brokerage OAuth refresh tokens expire frequently, forcing weekly 2FA logins to babysit a strategy that borrows only a couple of times a decade, while placing live order-execution credentials on a background machine.

CrashGate takes the opposite approach:
- **Zero brokerage API keys** and **zero order-execution code** (compile-time read-only).
- Reads your local `hledger` journal (plus optional Schwab activity CSV exports for exact FIFO lots and 30-day wash-sale lockouts).
- Fetches public two-year highs, VIX, and short-term Treasury yields (`^IRX`) with automatic `query1 → query2` failover.
- Evaluates five deterministic risk gates and sends a plain-text transactional email via [Resend](https://resend.com/) only when an actionable event fires—leaving final review and manual limit orders to you.

## Features

1. **Core-Only Deep-Crash Gating:** Tracks rolling 2-year drawdowns per sleeve and isolates margin recommendations strictly to designated core sleeves (never volatile satellites).
2. **Episodic LTV Cooldown & Tax-Wall Blackouts:** Blocks new margin draws whenever LTV exceeds your configured cooldown ceiling (default `8%`) or during an upcoming tax-payment blackout window, with an optional broad equity panic override (`≤ -30%`).
3. **Stagflation Rate-Spread Filter:** Estimates floating brokerage margin APR (`^IRX + margin_spread_pct`) and blocks new borrowing when rates spike above your ceiling (default `7.5%`).
4. **Dynamic ETF Twin Aliases & Cross-Account Wash-Sale Checks:** Maps each sleeve to an ordered list of non-substantially-identical ETFs (for example `VOO / SCHX / IVV`), automatically tracks whichever twin you currently hold in `hledger`, selects the first unlocked replacement during a tax-loss harvest, and warns if the same ticker is held in external IRAs, 401k, or HSA accounts (Rev. Rul. 2008-5).
5. **Weekend/Holiday Lockout Catch-Up & Dead-Man Alerting:** Catches 30-day wash-sale unlock dates that land on weekends or 3-day holidays on the next trading day, deduplicates alerts over a configurable cooldown (default 7 days), and emails a health alert if quote/ledger fetches fail two consecutive runs.

## Quick Start

### 1. Install

**Go (`go 1.24+`):**
```bash
go install github.com/wjkoh/crashgate/cmd/crashgate@latest
```

**Nix Flake:**
```bash
nix run github:wjkoh/crashgate -- --config ./config.json
```

### 2. Configure

Copy `config.example.json` to a private `config.json` (kept outside public repos) and edit your `hledger` paths, sleeves, thresholds, and notification settings:

```bash
cp config.example.json config.json
```

- **Works with `hledger` alone:** Leave `hledger.schwab_activity_glob` empty and CrashGate automatically reads positions, cash/margin balances, and aggregate cost basis (`hledger bal -B`) directly from your `hledger` journal—compatible with any brokerage.
- **Optional Schwab CSV lot replay:** Point `hledger.schwab_activity_glob` to your Schwab brokerage activity CSVs (`Buy`, `Reinvest Shares`, `Sell`) for exact FIFO tax-lot dates, 30-day partial-wash-sale warnings, and automatic 30-day unlock alerts.

### 3. Dry-Run (Stdout Only)

```bash
crashgate --config ./config.json
```

### 4. Send Actionable Alerts via Resend

Provide your Resend API key via `RESEND_API_KEY` or `notify.resend_key_file`:

```bash
export RESEND_API_KEY="re_..."
crashgate --config ./config.json --send
```

Useful flags:
- `--config <path>`: Path to JSON configuration file (default `config.json`).
- `--hledger <path>`: Path to `hledger` binary (default `hledger`).
- `--send`: Send Resend email only when an unsent `HIGH`/`MEDIUM` alert or monthly paydown digest fires.
- `--force-send`: Send a full portfolio briefing email regardless of alert state.
- `--digest`: Include the monthly margin paydown digest regardless of calendar day.
- `--to`, `--from`, `--resend-key-file`, `--state-file`: CLI overrides for config values.

## Scheduling with Nix (`nix-darwin` / NixOS)

Because `crashgate` has zero external Go dependencies (`vendorHash = null`), you can consume it directly as a flake input in your `nix-config`:

```nix
# flake.nix
inputs.crashgate = {
  url = "github:wjkoh/crashgate";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Example `nix-darwin` weekday 11:30 AM local schedule (`launchd.user.agents`):

```nix
launchd.user.agents.crashgate = {
  serviceConfig = {
    ProgramArguments = [
      "${inputs.crashgate.packages.${pkgs.system}.default}/bin/crashgate"
      "--config" "/path/to/private/config.json"
      "--hledger" "${pkgs.hledger}/bin/hledger"
      "--send"
    ];
    StartCalendarInterval = map (day: {
      Hour = 11;
      Minute = 30;
      Weekday = day;
    }) [1 2 3 4 5];
    RunAtLoad = false;
    StandardOutPath = "/Users/you/Library/Logs/crashgate.log";
    StandardErrorPath = "/Users/you/Library/Logs/crashgate.error.log";
  };
};
```

## Development

```bash
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" ./cmd/crashgate
```

Unit tests are strictly isolated from live email delivery via `testing.Testing()` inside `SendResendEmail`.

## License

Apache License 2.0. See [LICENSE](LICENSE).
