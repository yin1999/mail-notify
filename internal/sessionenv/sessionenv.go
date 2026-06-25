package sessionenv

import (
	"fmt"
	"log"
	"strings"

	"github.com/godbus/dbus/v5"
)

const systemdUserBusName = "org.freedesktop.systemd1"
const systemdUserObjectPath = dbus.ObjectPath("/org/freedesktop/systemd1")
const systemdUserManagerIface = "org.freedesktop.systemd1.Manager"
const dbusPropertiesIface = "org.freedesktop.DBus.Properties"

var systemdUserManagerEnvironment = getSystemdUserManagerEnvironment

func WithSystemdUserLocale(base []string, logger *log.Logger) []string {
	systemdEnv, err := systemdUserManagerEnvironment()
	if err != nil {
		if logger != nil {
			logger.Printf("get systemd user manager environment: %v", err)
		}
		return base
	}
	return mergeEnv(base, localeEnv(systemdEnv))
}

func getSystemdUserManagerEnvironment() ([]string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	defer conn.Close()

	var env []string
	call := conn.Object(systemdUserBusName, systemdUserObjectPath).Call(dbusPropertiesIface+".Get", 0, systemdUserManagerIface, "Environment")
	if call.Err != nil {
		return nil, call.Err
	}
	var value dbus.Variant
	if err := call.Store(&value); err != nil {
		return nil, err
	}
	env, ok := value.Value().([]string)
	if !ok {
		return nil, fmt.Errorf("systemd manager Environment has unexpected type %T", value.Value())
	}
	return env, nil
}

func localeEnv(env []string) []string {
	out := make([]string, 0)
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !isLocaleEnvKey(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isLocaleEnvKey(key string) bool {
	return key == "LANG" || key == "LANGUAGE" || strings.HasPrefix(key, "LC_")
}

func mergeEnv(base []string, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}

	out := append([]string(nil), base...)
	positions := make(map[string]int, len(out))
	for i, entry := range out {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			positions[key] = i
		}
	}

	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if pos, ok := positions[key]; ok {
			out[pos] = entry
			continue
		}
		positions[key] = len(out)
		out = append(out, entry)
	}

	return out
}
