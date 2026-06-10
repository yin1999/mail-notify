package notify

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
)

const (
	notificationBusName = "org.freedesktop.Notifications"
	notificationPath    = dbus.ObjectPath("/org/freedesktop/Notifications")
	notificationIface   = "org.freedesktop.Notifications"
)

type Client struct {
	appName string
	conn    *dbus.Conn
}

type Action struct {
	Key   string
	Label string
}

type ActionEvent struct {
	ID  uint32
	Key string
}

func NewClient(appName string) (*Client, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	return &Client{appName: appName, conn: conn}, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

func (c *Client) Notify(summary, body string, actions []Action) error {
	obj := c.conn.Object(notificationBusName, notificationPath)

	var notificationID uint32
	call := obj.Call(
		notificationIface+".Notify",
		0,
		c.appName,
		uint32(0),
		"mail-unread-symbolic",
		summary,
		body,
		actionList(actions),
		map[string]dbus.Variant{},
		int32(10_000),
	)
	if call.Err != nil {
		return call.Err
	}
	return call.Store(&notificationID)
}

func (c *Client) WatchActions(ctx context.Context, handler func(ActionEvent)) error {
	rule := "type='signal',interface='org.freedesktop.Notifications',member='ActionInvoked',path='/org/freedesktop/Notifications'"
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err; err != nil {
		return fmt.Errorf("add notification action match: %w", err)
	}

	signals := make(chan *dbus.Signal, 8)
	c.conn.Signal(signals)

	go func() {
		defer c.conn.RemoveSignal(signals)
		defer c.conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)

		for {
			select {
			case <-ctx.Done():
				return
			case signal := <-signals:
				if signal == nil || signal.Name != notificationIface+".ActionInvoked" || len(signal.Body) != 2 {
					continue
				}

				id, ok := signal.Body[0].(uint32)
				if !ok {
					continue
				}
				key, ok := signal.Body[1].(string)
				if !ok {
					continue
				}
				handler(ActionEvent{ID: id, Key: key})
			}
		}
	}()

	return nil
}

func actionList(actions []Action) []string {
	out := make([]string, 0, len(actions)*2)
	for _, action := range actions {
		if action.Key == "" || action.Label == "" {
			continue
		}
		out = append(out, action.Key, action.Label)
	}
	return out
}
