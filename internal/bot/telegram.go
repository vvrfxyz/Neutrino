package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/repo"
	"neutrino/internal/subscription"
)

// UserAdmin is the slice of service.UserService the bot's admin commands
// need. Mutations must go through the service (not repo.Store) so status
// changes and quota resets trigger node users-sync — otherwise /disable
// leaves the user connected on every node until the next full reconcile.
type UserAdmin interface {
	SetStatus(ctx context.Context, userID int64, status string) (repo.User, error)
	ResetQuota(ctx context.Context, userID int64, reason string) error
}

type TelegramBot struct {
	cfg        config.Config
	store      *repo.Store
	users      UserAdmin
	httpClient *http.Client
}

func New(cfg config.Config, store *repo.Store, users UserAdmin) *TelegramBot {
	return &TelegramBot{
		cfg:   cfg,
		store: store,
		users: users,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (b *TelegramBot) Enabled() bool {
	return strings.TrimSpace(b.cfg.TelegramBotToken) != ""
}

type tgUpdateResp struct {
	OK     bool `json:"ok"`
	Result []struct {
		UpdateID int64 `json:"update_id"`
		Message  struct {
			Text string `json:"text"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			From struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"from"`
		} `json:"message"`
	} `json:"result"`
}

func (b *TelegramBot) Start(ctx context.Context) {
	if !b.Enabled() {
		return
	}
	offset := int64(0)
	lastErrLog := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		updates, nextOffset, err := b.fetchUpdates(ctx, offset)
		if err != nil {
			// Rate limit to avoid spamming logs on network issues (common in some regions).
			now := time.Now()
			if lastErrLog.IsZero() || now.Sub(lastErrLog) >= 30*time.Second {
				lastErrLog = now
				log.Printf("telegram poll error: %v", err)
			}
			time.Sleep(3 * time.Second)
			continue
		}
		offset = nextOffset
		for _, item := range updates {
			text := strings.TrimSpace(item.Message.Text)
			if text == "" {
				continue
			}
			resp := b.handleCommand(ctx, item.Message.Chat.ID, item.Message.From.ID, item.Message.From.Username, text)
			if strings.TrimSpace(resp) != "" {
				if err := b.sendMessage(ctx, item.Message.Chat.ID, resp); err != nil {
					log.Printf("telegram send error chat_id=%d: %v", item.Message.Chat.ID, err)
				}
			}
		}
	}
}

func (b *TelegramBot) fetchUpdates(ctx context.Context, offset int64) ([]struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}, int64, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=25&offset=%d", b.cfg.TelegramBotToken, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, offset, b.redactToken(err)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, offset, b.redactToken(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, offset, fmt.Errorf("telegram getUpdates status=%d", resp.StatusCode)
	}
	var decoded tgUpdateResp
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, offset, err
	}
	nextOffset := offset
	for _, u := range decoded.Result {
		if u.UpdateID >= nextOffset {
			nextOffset = u.UpdateID + 1
		}
	}
	return decoded.Result, nextOffset, nil
}

func (b *TelegramBot) sendMessage(ctx context.Context, chatID int64, text string) error {
	payload := map[string]any{"chat_id": chatID, "text": text}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.cfg.TelegramBotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return b.redactToken(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return b.redactToken(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram sendMessage status=%d", resp.StatusCode)
	}
	return nil
}

// redactToken strips the bot token from transport errors before they reach
// logs: url.Error (and anything wrapping it) embeds the full request URL,
// which contains the token as a path segment.
func (b *TelegramBot) redactToken(err error) error {
	if err == nil {
		return nil
	}
	token := strings.TrimSpace(b.cfg.TelegramBotToken)
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}

func (b *TelegramBot) isAdminChat(chatID int64) bool {
	for _, part := range strings.Split(b.cfg.TelegramAdminChatIDs, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && id == chatID {
			return true
		}
	}
	return false
}

func (b *TelegramBot) handleCommand(ctx context.Context, chatID, fromUserID int64, fromUsername, raw string) string {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) == 0 {
		return ""
	}
	cmd := strings.ToLower(parts[0])
	admin := b.isAdminChat(chatID)

	if cmd == "/bind" {
		if len(parts) < 2 {
			return "usage: /bind <code>"
		}
		u, _, err := b.store.BindTelegramChatByCode(ctx, parts[1], chatID, fromUserID, fromUsername)
		if err != nil {
			if errors.Is(err, repo.ErrTelegramBindRateLimited) {
				return "bind failed: too many attempts"
			}
			return "bind failed: invalid code"
		}
		return fmt.Sprintf("bind ok: user=%s", u.Username)
	}

	if admin {
		switch cmd {
		case "/summary":
			users, err := b.store.ListUsers(ctx)
			if err != nil {
				return "query failed"
			}
			active := 0
			for _, u := range users {
				if u.Status == "active" {
					active++
				}
			}
			return fmt.Sprintf("users=%d active=%d", len(users), active)
		case "/user":
			if len(parts) < 2 {
				return "usage: /user <name>"
			}
			u, err := b.store.GetUserByUsername(ctx, parts[1])
			if err != nil {
				return "user not found"
			}
			return fmt.Sprintf("user=%s status=%s quota=%.2fGB used=%.2fGB", u.Username, u.Status, float64(u.MonthlyLimitBytes)/(1024*1024*1024), float64(u.WindowEffective)/(1024*1024*1024))
		case "/enable":
			if len(parts) < 2 {
				return "usage: /enable <name>"
			}
			u, err := b.store.GetUserByUsername(ctx, parts[1])
			if err != nil {
				return "user not found"
			}
			if _, err := b.users.SetStatus(ctx, u.ID, "active"); err != nil {
				return "enable failed"
			}
			return "ok"
		case "/disable":
			if len(parts) < 2 {
				return "usage: /disable <name>"
			}
			u, err := b.store.GetUserByUsername(ctx, parts[1])
			if err != nil {
				return "user not found"
			}
			if _, err := b.users.SetStatus(ctx, u.ID, "disabled"); err != nil {
				return "disable failed"
			}
			return "ok"
		case "/quota_reset":
			if len(parts) < 2 {
				return "usage: /quota_reset <name>"
			}
			u, err := b.store.GetUserByUsername(ctx, parts[1])
			if err != nil {
				return "user not found"
			}
			if err := b.users.ResetQuota(ctx, u.ID, "telegram_admin"); err != nil {
				return "quota reset failed"
			}
			return "ok"
		}
	}

	u, err := b.store.GetUserByTelegramChatID(ctx, chatID)
	if err != nil {
		return "user not bound. send /bind <code> first"
	}
	switch cmd {
	case "/me":
		return fmt.Sprintf("user=%s status=%s expires=%s", u.Username, u.Status, u.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	case "/usage":
		return fmt.Sprintf("used=%.2fGB limit=%.2fGB", float64(u.WindowEffective)/(1024*1024*1024), float64(u.MonthlyLimitBytes)/(1024*1024*1024))
	case "/sub":
		tok, err := b.store.GetOrCreateSubscriptionToken(ctx, u.ID)
		if err != nil {
			return "subscription token failed"
		}
		return subscription.BuildSubscriptionURL(b.cfg.SubBaseURL, tok.Token, "v2rayn")
	default:
		return "commands: /bind <code> /me /usage /sub"
	}
}
