package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/wjkoh/crashgate/internal/watchdog"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "Path to crashgate JSON config file")
	hledgerBin := flag.String("hledger", "hledger", "Path to hledger binary")
	send := flag.Bool("send", false, "Send Resend email if unsent actionable alerts or monthly digest fire")
	forceSend := flag.Bool("force-send", false, "Force-send Resend email briefing even if no actionable alert fired")
	digest := flag.Bool("digest", false, "Include monthly paydown digest regardless of day of month")
	toOverride := flag.String("to", "", "Override recipient email address from config")
	fromOverride := flag.String("from", "", "Override verified Resend sender address from config")
	resendKeyOverride := flag.String("resend-key-file", "", "Override Resend API key file path from config")
	stateOverride := flag.String("state-file", "", "Override alert deduplication state file path from config")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("crashgate %s\n", version)
		return
	}

	cfg, err := watchdog.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if *toOverride != "" {
		cfg.Notify.To = *toOverride
	}
	if *fromOverride != "" {
		cfg.Notify.From = *fromOverride
	}
	if *resendKeyOverride != "" {
		cfg.Notify.ResendKeyFile = *resendKeyOverride
	}
	if *stateOverride != "" {
		cfg.Notify.StateFile = *stateOverride
	}

	now := time.Now()
	state := watchdog.LoadAlertState(cfg.Notify.StateFile)

	handleFatal := func(stage string, err error) {
		msg := fmt.Sprintf("%s: %v", stage, err)
		if *send || *forceSend {
			if watchdog.RecordFailureAndShouldAlert(state, cfg.Policy.HealthFailThreshold, msg, now) {
				if apiKey, kErr := watchdog.ResolveResendAPIKey(cfg.Notify.ResendKeyFile); kErr == nil {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					sub := fmt.Sprintf("%s Health Failure (%d consecutive runs)", cfg.Notify.SubjectPrefix, state.ConsecutiveFailures)
					body := fmt.Sprintf("CRASHGATE HEALTH ALERT\nPortfolio: %s\nTime: %s\nConsecutive Failures: %d\nLast Success: %s\nError: %s\n",
						cfg.PortfolioName, now.Format("Mon, Jan 2, 2006 3:04 PM MST"), state.ConsecutiveFailures, state.LastSuccess.Format(time.RFC3339), msg)
					if sErr := watchdog.SendResendEmail(ctx, nil, apiKey, cfg.Notify.From, cfg.Notify.To, sub, body); sErr == nil {
						state.SentAt[watchdog.HealthFailAlertCode] = now
						fmt.Printf("[notify] Sent dead-man health alert to %s\n", cfg.Notify.To)
					}
				}
			}
			_ = watchdog.SaveAlertState(cfg.Notify.StateFile, state)
		}
		log.Fatal(msg)
	}

	ledger, err := watchdog.LoadLedger(cfg.Hledger, *hledgerBin, now)
	if err != nil {
		handleFatal("load ledger", err)
	}

	syms := watchdog.ActiveSymbols(cfg.Policy, ledger)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	market, err := watchdog.FetchMarketData(ctx, nil, "", cfg.Policy.MarginSpreadPct, cfg.Policy.FallbackAPRPct, syms)
	if err != nil {
		handleFatal("fetch market data", err)
	}

	report := watchdog.Evaluate(cfg.PortfolioName, cfg.Policy, ledger, market, now, *digest)
	body := watchdog.FormatPlainText(report)
	fmt.Print(body)

	if !*send && !*forceSend {
		return
	}

	watchdog.RecordSuccess(state, now)
	unsent := watchdog.FilterUnsentAlerts(report.Alerts, state, cfg.Policy.AlertCooldownDays, now)
	if len(unsent) == 0 && !*forceSend {
		if err := watchdog.SaveAlertState(cfg.Notify.StateFile, state); err != nil {
			log.Printf("warning: save alert state: %v", err)
		}
		fmt.Println("\n[notify] No new unsent alerts to email.")
		return
	}

	apiKey, err := watchdog.ResolveResendAPIKey(cfg.Notify.ResendKeyFile)
	if err != nil {
		log.Fatalf("resolve resend api key: %v", err)
	}

	subject := buildSubject(cfg.Notify.SubjectPrefix, report, unsent)
	if err := watchdog.SendResendEmail(ctx, nil, apiKey, cfg.Notify.From, cfg.Notify.To, subject, body); err != nil {
		log.Fatalf("send resend email: %v", err)
	}
	fmt.Printf("\n[notify] Sent Resend email to %s: %s\n", cfg.Notify.To, subject)

	for _, a := range unsent {
		state.SentAt[a.Code] = now
	}
	if err := watchdog.SaveAlertState(cfg.Notify.StateFile, state); err != nil {
		log.Printf("warning: save alert state: %v", err)
	}
	os.Exit(0)
}

func buildSubject(prefix string, rep *watchdog.EvaluationReport, unsent []watchdog.AlertItem) string {
	var tags []string
	for _, a := range unsent {
		if a.Severity == "HIGH" {
			return "[ACTION] " + a.Title
		}
		if a.Severity == "MEDIUM" {
			tags = append(tags, a.Title)
		}
	}
	if len(tags) > 0 {
		return prefix + " " + strings.Join(tags, " | ")
	}
	if rep.IsMonthlyDigest {
		return fmt.Sprintf("%s Monthly Margin Paydown Digest (%s) - LTV %.1f%%", prefix, rep.EvaluatedAt.Format("Jan 2006"), rep.LTV*100)
	}
	return fmt.Sprintf("%s Portfolio Status Briefing - LTV %.1f%%", prefix, rep.LTV*100)
}
