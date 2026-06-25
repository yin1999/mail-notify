package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"mail-notify/internal/goa"
	"mail-notify/internal/imapcheck"
	"mail-notify/internal/notify"
	"mail-notify/internal/sessionenv"
)

type mailboxState struct {
	UIDNext  uint32    `json:"uid_next"`
	Unseen   uint32    `json:"unseen"`
	LastSeen time.Time `json:"last_seen"`
}

type stateFile struct {
	Accounts map[string]mailboxState `json:"accounts"`
}

const staleStateAfter = 10 * 24 * time.Hour
const accountCheckTimeout = 45 * time.Second

type appConfig struct {
	TLSOverrides map[string]imapcheck.TLSOverride `json:"tls_overrides"`
	Notification notificationConfig               `json:"notification"`
}

type notificationConfig struct {
	OpenCommand []string `json:"open_command"`
}

func main() {
	var (
		interval       = flag.Duration("interval", 2*time.Minute, "mail check interval")
		once           = flag.Bool("once", false, "check once and exit")
		notifyExisting = flag.Bool("notify-existing", false, "notify about unread messages on first run")
		debug          = flag.Bool("debug", false, "print verbose diagnostics")
		statePath      = flag.String("state", defaultStatePath(), "state file path")
		runtimeState   = flag.Bool("runtime-state", false, "store state under XDG_RUNTIME_DIR instead of XDG_STATE_HOME")
		configPath     = flag.String("config", defaultConfigPath(), "config file path")
	)
	flag.Parse()
	stateExplicit := flagWasSet("state")

	logger := log.New(os.Stderr, "", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resolvedStatePath, err := resolveStatePath(*statePath, *runtimeState, stateExplicit)
	if err != nil {
		logger.Fatalf("resolve state path: %v", err)
	}

	state, err := loadState(resolvedStatePath)
	if err != nil {
		logger.Fatalf("load state: %v", err)
	}

	config, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	checker, err := newChecker()
	if err != nil {
		logger.Fatalf("init: %v", err)
	}
	defer checker.Close()

	if len(config.Notification.OpenCommand) > 0 {
		if err := checker.notifier.WatchActions(ctx, func(event notify.ActionEvent) {
			if event.Key != "default" && event.Key != "open" {
				return
			}
			if err := startConfiguredCommand(config.Notification.OpenCommand, logger); err != nil {
				logger.Printf("open notification command failed: %v", err)
			}
		}); err != nil {
			logger.Printf("watch notification actions: %v", err)
		}
	}

	run := func() {
		if ctx.Err() != nil {
			return
		}
		changed, err := checkAccounts(ctx, checker, state, config, *notifyExisting, *debug, logger)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("check failed: %v", err)
		}
		if changed {
			if err := saveState(resolvedStatePath, state); err != nil {
				logger.Printf("save state: %v", err)
			}
		}
	}

	run()
	if *once {
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

type checker struct {
	accounts *goa.Client
	notifier *notify.Client
}

func newChecker() (*checker, error) {
	accounts, err := goa.NewClient()
	if err != nil {
		return nil, err
	}

	notifier, err := notify.NewClient("Mail Notify")
	if err != nil {
		accounts.Close()
		return nil, err
	}

	return &checker{accounts: accounts, notifier: notifier}, nil
}

func (c *checker) Close() {
	if c.notifier != nil {
		c.notifier.Close()
	}
	if c.accounts != nil {
		c.accounts.Close()
	}
}

func checkAccounts(ctx context.Context, checker *checker, state stateFile, config appConfig, notifyExisting, debug bool, logger *log.Logger) (bool, error) {
	accounts, err := checker.accounts.MailAccounts(ctx)
	if err != nil {
		return false, err
	}
	if debug {
		logger.Printf("found %d GOA mail account(s)", len(accounts))
	}

	now := time.Now().UTC()
	seenKeys := make(map[string]struct{}, len(accounts))
	changed := false
	for _, account := range accounts {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}

		key := account.Key()
		seenKeys[key] = struct{}{}
		if touchAccountState(state, key, now) {
			changed = true
		}

		if debug {
			logger.Printf("checking %s via %s", account.DisplayName(), account.IMAPAddress())
		}

		accountCtx, cancelAccountCheck := context.WithTimeout(ctx, accountCheckTimeout)
		credentials, err := checker.accounts.Credentials(accountCtx, account)
		if err != nil {
			cancelAccountCheck()
			if ctx.Err() != nil {
				return changed, ctx.Err()
			}
			logger.Printf("%s: credentials unavailable: %v", account.DisplayName(), err)
			continue
		}

		status, err := imapcheck.Check(accountCtx, account.IMAP, credentials, imapcheck.Options{
			TLSOverrides: config.TLSOverrides,
		})
		cancelAccountCheck()
		if err != nil {
			if ctx.Err() != nil {
				return changed, ctx.Err()
			}
			logger.Printf("%s: IMAP check failed: %v", account.DisplayName(), err)
			continue
		}

		previous, known := state.Accounts[key]
		current := mailboxState{UIDNext: status.UIDNext, Unseen: status.Unseen, LastSeen: now}
		if !known {
			state.Accounts[key] = current
			changed = true
			if notifyExisting && status.Unseen > 0 {
				sendUnreadNotification(checker.notifier, config, account, status.Unseen, 0, logger)
			}
			continue
		}

		newMessages := uint32(0)
		if status.UIDNext > previous.UIDNext {
			newMessages = status.UIDNext - previous.UIDNext
		} else if status.UIDNext == 0 && previous.UIDNext == 0 && status.Unseen > previous.Unseen {
			newMessages = status.Unseen - previous.Unseen
		}

		if newMessages > 0 && status.Unseen > 0 {
			sendUnreadNotification(checker.notifier, config, account, status.Unseen, newMessages, logger)
		}

		if previous != current {
			state.Accounts[key] = current
			changed = true
		}
	}

	if pruneStaleAccountStates(state, seenKeys, now, staleStateAfter, logger) {
		changed = true
	}

	return changed, nil
}

func touchAccountState(state stateFile, key string, seenAt time.Time) bool {
	if state.Accounts == nil {
		state.Accounts = map[string]mailboxState{}
	}
	accountState, ok := state.Accounts[key]
	if !ok {
		return false
	}
	accountState.LastSeen = seenAt
	state.Accounts[key] = accountState
	return true
}

func pruneStaleAccountStates(state stateFile, seenKeys map[string]struct{}, now time.Time, maxAge time.Duration, logger *log.Logger) bool {
	changed := false
	for key, accountState := range state.Accounts {
		if _, ok := seenKeys[key]; ok {
			continue
		}

		if accountState.LastSeen.IsZero() {
			accountState.LastSeen = now
			state.Accounts[key] = accountState
			changed = true
			continue
		}

		if now.Sub(accountState.LastSeen) <= maxAge {
			continue
		}

		delete(state.Accounts, key)
		changed = true
		if logger != nil {
			logger.Printf("%s: removed stale state after %.0f days without GOA account", key, maxAge.Hours()/24)
		}
	}
	return changed
}

func sendUnreadNotification(notifier *notify.Client, config appConfig, account goa.MailAccount, unseen, newMessages uint32, logger *log.Logger) {
	summary := "New mail"
	if newMessages == 1 {
		summary = fmt.Sprintf("New mail for %s", account.DisplayName())
	}

	body := fmt.Sprintf("%s has %d unread message(s).", account.EmailAddress, unseen)
	if newMessages > 1 {
		body = fmt.Sprintf("%s has %d unread message(s), including %d new messages.", account.EmailAddress, unseen, newMessages)
	}

	actions := []notify.Action(nil)
	if len(config.Notification.OpenCommand) > 0 {
		actions = []notify.Action{
			{Key: "default", Label: "Open"},
			{Key: "open", Label: "Open"},
		}
	}

	if err := notifier.Notify(summary, body, actions); err != nil {
		logger.Printf("%s: notification failed: %v", account.DisplayName(), err)
	}
}

func startConfiguredCommand(argv []string, logger *log.Logger) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("notification open command is empty")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = sessionenv.WithSystemdUserLocale(os.Environ(), logger)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func loadState(path string) (stateFile, error) {
	state := stateFile{Accounts: map[string]mailboxState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(data) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return stateFile{}, err
	}
	if state.Accounts == nil {
		state.Accounts = map[string]mailboxState{}
	}
	return state, nil
}

func loadConfig(path string) (appConfig, error) {
	config := appConfig{TLSOverrides: map[string]imapcheck.TLSOverride{}}
	if path == "" {
		return config, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if len(data) == 0 {
		return config, nil
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return appConfig{}, err
	}
	if config.TLSOverrides == nil {
		config.TLSOverrides = map[string]imapcheck.TLSOverride{}
	}
	return config, nil
}

func saveState(path string, state stateFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaultConfigPath() string {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "mail-notify", "config.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "mail-notify", "config.json")
	}
	return ""
}

func flagWasSet(name string) bool {
	wasSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func resolveStatePath(statePath string, runtimeState bool, stateExplicit bool) (string, error) {
	if !runtimeState || stateExplicit {
		return statePath, nil
	}
	runtimePath := runtimeStatePath()
	if runtimePath == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set")
	}
	return runtimePath, nil
}

func defaultStatePath() string {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		return filepath.Join(stateHome, "mail-notify", "state.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "mail-notify", "state.json")
	}
	return filepath.Join(os.TempDir(), "mail-notify-state.json")
}

func runtimeStatePath() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "mail-notify", "state.json")
	}
	return ""
}
