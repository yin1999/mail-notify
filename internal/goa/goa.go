package goa

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	goaBusName     = "org.gnome.OnlineAccounts"
	goaObjectPath  = dbus.ObjectPath("/org/gnome/OnlineAccounts")
	objectManager  = "org.freedesktop.DBus.ObjectManager"
	accountIface   = "org.gnome.OnlineAccounts.Account"
	mailIface      = "org.gnome.OnlineAccounts.Mail"
	oauth2Iface    = "org.gnome.OnlineAccounts.OAuth2Based"
	passwordIface  = "org.gnome.OnlineAccounts.PasswordBased"
	accountMethod  = accountIface + ".EnsureCredentials"
	oauth2Method   = oauth2Iface + ".GetAccessToken"
	passwordMethod = passwordIface + ".GetPassword"
)

type Client struct {
	mu     sync.Mutex
	conn   *dbus.Conn
	closed bool
}

type MailAccount struct {
	Path                 dbus.ObjectPath
	ID                   string
	ProviderName         string
	ProviderType         string
	PresentationIdentity string
	EmailAddress         string
	Name                 string
	IMAP                 IMAPSettings
	SupportsOAuth2       bool
	SupportsPassword     bool
}

type IMAPSettings struct {
	Host   string
	Port   int
	User   string
	UseSSL bool
	UseTLS bool
}

type Credentials struct {
	OAuth2AccessToken string
	Password          string
}

func NewClient() (*Client, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
}

func (c *Client) MailAccounts(ctx context.Context) ([]MailAccount, error) {
	objects, conn, err := c.getManagedObjects(ctx)
	if c.shouldReconnect(ctx, err) {
		if reconnectErr := c.reconnect(conn); reconnectErr != nil {
			return nil, fmt.Errorf("get GOA objects: connection closed; reconnect session bus: %w", reconnectErr)
		}
		objects, _, err = c.getManagedObjects(ctx)
	}
	if err != nil {
		return nil, err
	}

	return mailAccountsFromObjects(objects), nil
}

func (c *Client) getManagedObjects(ctx context.Context) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Conn, error) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	conn, err := c.currentConn()
	if err != nil {
		return nil, conn, fmt.Errorf("get GOA objects: %w", err)
	}

	call := conn.Object(goaBusName, goaObjectPath).CallWithContext(ctx, objectManager+".GetManagedObjects", 0)
	if call.Err != nil {
		return nil, conn, fmt.Errorf("get GOA objects: %w", call.Err)
	}
	if err := call.Store(&objects); err != nil {
		return nil, conn, fmt.Errorf("decode GOA objects: %w", err)
	}
	return objects, conn, nil
}

func mailAccountsFromObjects(objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant) []MailAccount {
	accounts := make([]MailAccount, 0)
	for path, ifaces := range objects {
		accountProps, ok := ifaces[accountIface]
		if !ok {
			continue
		}
		mailProps, ok := ifaces[mailIface]
		if !ok {
			continue
		}
		if boolProp(accountProps, "MailDisabled") {
			continue
		}
		if !boolProp(mailProps, "ImapSupported") {
			continue
		}

		host := stringProp(mailProps, "ImapHost")
		user := stringProp(mailProps, "ImapUserName")
		if host == "" || user == "" {
			continue
		}

		account := MailAccount{
			Path:                 path,
			ID:                   stringProp(accountProps, "Id"),
			ProviderName:         stringProp(accountProps, "ProviderName"),
			ProviderType:         stringProp(accountProps, "ProviderType"),
			PresentationIdentity: stringProp(accountProps, "PresentationIdentity"),
			EmailAddress:         stringProp(mailProps, "EmailAddress"),
			Name:                 stringProp(mailProps, "Name"),
			IMAP: IMAPSettings{
				Host:   host,
				Port:   intProp(mailProps, "ImapPort"),
				User:   user,
				UseSSL: boolProp(mailProps, "ImapUseSsl"),
				UseTLS: boolProp(mailProps, "ImapUseTls"),
			},
			SupportsOAuth2:   ifaces[oauth2Iface] != nil,
			SupportsPassword: ifaces[passwordIface] != nil,
		}
		accounts = append(accounts, account)
	}

	return accounts
}

func (c *Client) Credentials(ctx context.Context, account MailAccount) (Credentials, error) {
	credentials, conn, err := c.credentials(ctx, account)
	if c.shouldReconnect(ctx, err) {
		if reconnectErr := c.reconnect(conn); reconnectErr != nil {
			return Credentials{}, fmt.Errorf("credentials: connection closed; reconnect session bus: %w", reconnectErr)
		}
		credentials, _, err = c.credentials(ctx, account)
	}
	return credentials, err
}

func (c *Client) credentials(ctx context.Context, account MailAccount) (Credentials, *dbus.Conn, error) {
	conn, err := c.currentConn()
	if err != nil {
		return Credentials{}, conn, fmt.Errorf("credentials: %w", err)
	}

	obj := conn.Object(goaBusName, account.Path)
	if err := obj.CallWithContext(ctx, accountMethod, 0).Err; err != nil {
		return Credentials{}, conn, fmt.Errorf("ensure credentials: %w", err)
	}

	var out Credentials
	if account.SupportsOAuth2 {
		var token string
		var expiresIn int32
		call := obj.CallWithContext(ctx, oauth2Method, 0)
		if call.Err == nil {
			if err := call.Store(&token, &expiresIn); err == nil && token != "" {
				out.OAuth2AccessToken = token
				return out, conn, nil
			}
		} else if isConnectionClosedError(call.Err) {
			return Credentials{}, conn, call.Err
		}
	}

	if account.SupportsPassword {
		for _, id := range []string{"imap-password", "password"} {
			var password string
			call := obj.CallWithContext(ctx, passwordMethod, 0, id)
			if call.Err != nil {
				if isConnectionClosedError(call.Err) {
					return Credentials{}, conn, call.Err
				}
				continue
			}
			if err := call.Store(&password); err == nil && password != "" {
				out.Password = password
				return out, conn, nil
			}
		}
	}

	return Credentials{}, conn, errors.New("no usable OAuth2 token or IMAP password returned by GOA")
}

func (c *Client) currentConn() (*dbus.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, dbus.ErrClosed
	}
	return c.conn, nil
}

func (c *Client) shouldReconnect(ctx context.Context, err error) bool {
	if !isConnectionClosedError(err) || ctx.Err() != nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *Client) reconnect(previous *dbus.Conn) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return dbus.ErrClosed
	}
	if previous != nil && c.conn != previous {
		c.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	old := c.conn
	c.conn = conn
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	return nil
}

func isConnectionClosedError(err error) bool {
	return errors.Is(err, dbus.ErrClosed)
}

func (a MailAccount) Key() string {
	if a.ID != "" {
		return a.ID
	}
	if a.EmailAddress != "" {
		return a.EmailAddress
	}
	return string(a.Path)
}

func (a MailAccount) DisplayName() string {
	for _, candidate := range []string{a.PresentationIdentity, a.EmailAddress, a.Name, a.ProviderName, a.ID} {
		if candidate != "" {
			return candidate
		}
	}
	return string(a.Path)
}

func (a MailAccount) IMAPAddress() string {
	host := a.IMAP.Host
	if a.IMAP.Port > 0 {
		host = fmt.Sprintf("%s:%d", strings.Trim(host, "[]"), a.IMAP.Port)
	}
	return host
}

func stringProp(props map[string]dbus.Variant, name string) string {
	value, ok := props[name]
	if !ok {
		return ""
	}
	if out, ok := value.Value().(string); ok {
		return out
	}
	return fmt.Sprint(value.Value())
}

func boolProp(props map[string]dbus.Variant, name string) bool {
	value, ok := props[name]
	if !ok {
		return false
	}
	switch typed := value.Value().(type) {
	case bool:
		return typed
	case string:
		return typed == "true" || typed == "1"
	default:
		return false
	}
}

func intProp(props map[string]dbus.Variant, name string) int {
	value, ok := props[name]
	if !ok {
		return 0
	}
	switch typed := value.Value().(type) {
	case int:
		return typed
	case int16:
		return int(typed)
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case uint16:
		return int(typed)
	case uint32:
		return int(typed)
	case uint64:
		return int(typed)
	case string:
		out, _ := strconv.Atoi(typed)
		return out
	default:
		return 0
	}
}
