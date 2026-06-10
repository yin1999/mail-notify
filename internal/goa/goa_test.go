package goa

import (
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestIsConnectionClosedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "dbus closed", err: dbus.ErrClosed, want: true},
		{name: "wrapped dbus closed", err: fmt.Errorf("get GOA objects: %w", dbus.ErrClosed), want: true},
		{name: "other", err: fmt.Errorf("get GOA objects: other error"), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isConnectionClosedError(test.err); got != test.want {
				t.Fatalf("isConnectionClosedError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
