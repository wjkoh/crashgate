package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// HealthFailAlertCode is the deduplication key used for consecutive-failure dead-man alerts.
const HealthFailAlertCode = "WATCHDOG_HEALTH_FAIL"

// AlertState persists recently sent alert codes and consecutive health failures.
type AlertState struct {
	SentAt              map[string]time.Time `json:"sent_at"`
	ConsecutiveFailures int                  `json:"consecutive_failures,omitempty"`
	LastSuccess         time.Time            `json:"last_success,omitempty"`
	LastError           string               `json:"last_error,omitempty"`
}

func LoadAlertState(path string) *AlertState {
	st := &AlertState{SentAt: make(map[string]time.Time)}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, st)
	if st.SentAt == nil {
		st.SentAt = make(map[string]time.Time)
	}
	return st
}

func SaveAlertState(path string, st *AlertState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// RecordSuccess resets the consecutive-failure counter and stamps LastSuccess.
func RecordSuccess(st *AlertState, now time.Time) {
	st.ConsecutiveFailures = 0
	st.LastError = ""
	st.LastSuccess = now
}

// RecordFailureAndShouldAlert increments ConsecutiveFailures and returns true once
// failures reach threshold consecutive runs and no health alert was sent in the last 72 hours.
func RecordFailureAndShouldAlert(st *AlertState, threshold int, errMsg string, now time.Time) bool {
	if threshold <= 0 {
		threshold = 2
	}
	st.ConsecutiveFailures++
	st.LastError = errMsg
	if st.ConsecutiveFailures < threshold {
		return false
	}
	if last, ok := st.SentAt[HealthFailAlertCode]; ok && now.Sub(last) < 72*time.Hour {
		return false
	}
	return true
}

// FilterUnsentAlerts drops alerts already emailed within the configured cooldown window
// (default 7 days for HIGH/MEDIUM alerts, 25 days for monthly digest).
func FilterUnsentAlerts(alerts []AlertItem, st *AlertState, cooldownDays int, now time.Time) []AlertItem {
	if cooldownDays <= 0 {
		cooldownDays = 7
	}
	var out []AlertItem
	for _, a := range alerts {
		if a.Severity == "INFO" && !strings.HasPrefix(a.Code, "MONTHLY_DIGEST_") {
			continue
		}
		cooldown := time.Duration(cooldownDays) * 24 * time.Hour
		if strings.HasPrefix(a.Code, "MONTHLY_DIGEST_") {
			cooldown = 25 * 24 * time.Hour
		}
		if last, ok := st.SentAt[a.Code]; ok && now.Sub(last) < cooldown {
			continue
		}
		out = append(out, a)
	}
	return out
}

func ResolveResendAPIKey(keyFile string) (string, error) {
	if env := strings.TrimSpace(os.Getenv("RESEND_API_KEY")); env != "" {
		return env, nil
	}
	if keyFile == "" {
		return "", fmt.Errorf("RESEND_API_KEY env var is unset and notify.resend_key_file is empty")
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return "", fmt.Errorf("read resend api key from %s: %w", keyFile, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("empty resend api key in %s", keyFile)
	}
	return key, nil
}

type resendPayload struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// SendResendEmail dispatches a plain-text alert via the Resend REST API.
// Guarded by testing.Testing() so unit tests can never dispatch live emails.
func SendResendEmail(ctx context.Context, client *http.Client, apiKey, from, to, subject, body string) error {
	if testing.Testing() {
		return fmt.Errorf("refusing to send live Resend email during go test")
	}
	if strings.TrimSpace(to) == "" || strings.TrimSpace(from) == "" {
		return fmt.Errorf("both notify.from and notify.to are required to send Resend email")
	}
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	payload := resendPayload{
		From:    from,
		To:      []string{to},
		Subject: subject,
		Text:    body,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("resend API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
