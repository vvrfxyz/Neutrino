package singboxapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type stateFile struct {
	UpdatedAt string            `json:"updated_at"`
	Users     map[string]string `json:"users"`
}

type Client struct {
	enabled   bool
	statePath string
	reloadCmd string
	mu        sync.Mutex
}

func NewRaw(enabled bool, statePath string, reloadCmd string) *Client {
	return &Client{
		enabled:   enabled,
		statePath: strings.TrimSpace(statePath),
		reloadCmd: strings.TrimSpace(reloadCmd),
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.enabled && strings.TrimSpace(c.statePath) != ""
}

func (c *Client) UpsertUser(ctx context.Context, email, uuid string) error {
	if !c.Enabled() {
		return nil
	}
	email = strings.TrimSpace(email)
	uuid = strings.TrimSpace(uuid)
	if email == "" || uuid == "" {
		return errors.New("empty email or uuid")
	}
	return c.updateState(ctx, func(st *stateFile) {
		if st.Users == nil {
			st.Users = map[string]string{}
		}
		st.Users[email] = uuid
	})
}

func (c *Client) RemoveUser(ctx context.Context, email string) error {
	if !c.Enabled() {
		return nil
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	return c.updateState(ctx, func(st *stateFile) {
		delete(st.Users, email)
	})
}

func (c *Client) updateState(ctx context.Context, mutator func(*stateFile)) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, err := c.load()
	if err != nil {
		return err
	}
	mutator(&st)
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := c.save(st); err != nil {
		return err
	}
	return c.reload(ctx)
}

func (c *Client) load() (stateFile, error) {
	st := stateFile{Users: map[string]string{}}
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if len(data) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return stateFile{}, err
	}
	if st.Users == nil {
		st.Users = map[string]string{}
	}
	return st, nil
}

func (c *Client) save(st stateFile) error {
	if err := os.MkdirAll(filepath.Dir(c.statePath), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.statePath)
}

func (c *Client) reload(ctx context.Context) error {
	if c.reloadCmd == "" {
		return nil
	}
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(execCtx, "sh", "-lc", c.reloadCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reload singbox failed: %w output=%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
