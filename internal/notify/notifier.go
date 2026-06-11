package notify

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/config"
)

// smtpTimeout bounds the entire SMTP exchange (dial + handshake + send).
// SendMail runs inside the enforcement worker tick, so a black-holed SMTP
// host must not be able to stall it indefinitely.
const smtpTimeout = 15 * time.Second

type Notifier struct {
	cfg        config.Config
	httpClient *http.Client
}

func New(cfg config.Config) *Notifier {
	return &Notifier{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 8 * time.Second},
	}
}

func parseAdminChatIDs(raw string) []int64 {
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err == nil && v != 0 {
			out = append(out, v)
		}
	}
	return out
}

func (n *Notifier) SendTelegram(chatID int64, text string) error {
	if strings.TrimSpace(n.cfg.TelegramBotToken) == "" || chatID == 0 {
		return nil
	}
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramBotToken)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram send status=%d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) SendTelegramAdmins(text string) error {
	chatIDs := parseAdminChatIDs(n.cfg.TelegramAdminChatIDs)
	var firstErr error
	for _, chatID := range chatIDs {
		if err := n.SendTelegram(chatID, text); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (n *Notifier) SendMail(subject, body string, to []string) error {
	if strings.TrimSpace(n.cfg.SMTPHost) == "" || len(to) == 0 {
		return nil
	}
	addr := fmt.Sprintf("%s:%d", n.cfg.SMTPHost, n.cfg.SMTPPort)
	msg := strings.Builder{}
	msg.WriteString("From: " + n.cfg.SMTPFrom + "\r\n")
	msg.WriteString("To: " + strings.Join(to, ",") + "\r\n")
	msg.WriteString("Subject: " + subject + "\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)
	return sendMailWithTimeout(addr, n.cfg.SMTPHost, n.cfg.SMTPUser, n.cfg.SMTPPass, n.cfg.SMTPFrom, to, []byte(msg.String()))
}

// sendMailWithTimeout mirrors smtp.SendMail (STARTTLS when offered, optional
// PLAIN auth) but bounds the whole exchange with a connection deadline, which
// stdlib smtp.SendMail cannot do.
func sendMailWithTimeout(addr, host, user, pass, from string, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
		_ = conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if user != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
