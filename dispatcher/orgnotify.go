package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	envTelegramTokenFile = "BCX_TELEGRAM_TOKEN_FILE"
	envTelegramChatID    = "BCX_TELEGRAM_CHAT_ID"

	telegramAPIBase = "https://api.telegram.org"
	// telegramTextLimit stays below Telegram's 4096-character message limit.
	telegramTextLimit = 3500
	notifyTimeout     = 10 * time.Second
)

// chatIDPattern accepts a numeric chat id (groups are negative) or a @channelname.
var chatIDPattern = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)

// Notifier tells a human about findings. A failure is logged by the caller and never
// stops a plan: the finding is already in the befund file and the log.
type Notifier interface {
	Notify(ctx context.Context, befunde []Befund) error
}

// TelegramNotifier posts findings to one Telegram chat. The token is the secret: it
// sits in the request URL, so no error from this type may carry that URL.
type TelegramNotifier struct {
	APIBase string
	Token   string
	ChatID  string
	Client  *http.Client
}

// LoadTelegram reads the optional Telegram wiring: BCX_TELEGRAM_TOKEN_FILE names a file
// holding the bot token (the environment never carries the token itself) and
// BCX_TELEGRAM_CHAT_ID the target chat. Both unset means no notifier (nil, nil); only
// one of them set, an unreadable or empty token file or a malformed chat id fails the
// start, so a half-configured alarm is never mistaken for a working one.
func LoadTelegram(getenv func(string) string) (*TelegramNotifier, error) {
	file, chat := getenv(envTelegramTokenFile), getenv(envTelegramChatID)
	if file == "" && chat == "" {
		return nil, nil
	}
	if file == "" || chat == "" {
		return nil, fmt.Errorf("%s and %s must be set together", envTelegramTokenFile, envTelegramChatID)
	}
	if !filepath.IsAbs(file) {
		return nil, fmt.Errorf("%s must be an absolute path", envTelegramTokenFile)
	}
	if !chatIDPattern.MatchString(chat) {
		return nil, fmt.Errorf("%s is not a chat id", envTelegramChatID)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading the telegram token file: %w", errors.Unwrap(err))
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n/") {
		return nil, fmt.Errorf("the telegram token file %s does not hold a bot token", file)
	}
	return &TelegramNotifier{
		APIBase: telegramAPIBase, Token: token, ChatID: chat,
		Client: &http.Client{Timeout: notifyTimeout},
	}, nil
}

// telegramText renders the findings, one line each, clipped to the message limit.
func telegramText(befunde []Befund) string {
	var b strings.Builder
	b.WriteString("Leitstand ekd-akd: ")
	b.WriteString(fmt.Sprintf("%d Befund(e)\n", len(befunde)))
	for _, f := range befunde {
		ref := ""
		if f.Ref != nil {
			ref = " " + *f.Ref
		}
		fmt.Fprintf(&b, "[%s] %s %s%s: %s\n", f.Schwere, f.Art, f.Repo, ref, f.Grund)
	}
	text := strings.TrimSuffix(b.String(), "\n")
	if len(text) > telegramTextLimit {
		text = text[:telegramTextLimit] + "\n(gekürzt)"
	}
	return text
}

// Notify sends one message for all findings of an event.
func (t *TelegramNotifier) Notify(ctx context.Context, befunde []Befund) error {
	if len(befunde) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     telegramText(befunde),
		"disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("telegram: encoding the message: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.APIBase+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: building the request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.Client.Do(req)
	if err != nil {
		// *url.Error carries the full URL, and with it the token: keep only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return fmt.Errorf("telegram: request failed: %w", uerr.Err)
		}
		return errors.New("telegram: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}
