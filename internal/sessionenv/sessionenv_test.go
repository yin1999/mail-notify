package sessionenv

import (
	"errors"
	"io"
	"log"
	"reflect"
	"testing"
)

var errTestSystemdUnavailable = errors.New("systemd unavailable")

func TestLocaleEnvFiltersLocaleVariables(t *testing.T) {
	got := localeEnv([]string{
		"PATH=/usr/bin",
		"LANG=zh_CN.UTF-8",
		"LANGUAGE=zh_CN",
		"LC_MESSAGES=zh_CN.UTF-8",
		"XDG_CURRENT_DESKTOP=GNOME",
		"INVALID",
	})
	want := []string{
		"LANG=zh_CN.UTF-8",
		"LANGUAGE=zh_CN",
		"LC_MESSAGES=zh_CN.UTF-8",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("locale env = %#v, want %#v", got, want)
	}
}

func TestMergeEnvOverridesAndAppends(t *testing.T) {
	got := mergeEnv(
		[]string{"PATH=/usr/bin", "LANG=en_US.UTF-8"},
		[]string{"LANG=zh_CN.UTF-8", "LC_MESSAGES=zh_CN.UTF-8"},
	)
	want := []string{"PATH=/usr/bin", "LANG=zh_CN.UTF-8", "LC_MESSAGES=zh_CN.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged env = %#v, want %#v", got, want)
	}
}

func TestWithSystemdUserLocaleFallsBack(t *testing.T) {
	original := systemdUserManagerEnvironment
	defer func() { systemdUserManagerEnvironment = original }()
	systemdUserManagerEnvironment = func() ([]string, error) {
		return nil, errTestSystemdUnavailable
	}

	base := []string{"PATH=/usr/bin", "LANG=en_US.UTF-8"}
	got := WithSystemdUserLocale(base, log.New(io.Discard, "", log.LstdFlags))
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("env = %#v, want fallback %#v", got, base)
	}
}

func TestWithSystemdUserLocaleOverridesLocale(t *testing.T) {
	original := systemdUserManagerEnvironment
	defer func() { systemdUserManagerEnvironment = original }()
	systemdUserManagerEnvironment = func() ([]string, error) {
		return []string{
			"LANG=zh_CN.UTF-8",
			"LC_MESSAGES=zh_CN.UTF-8",
			"PATH=/not/from/systemd",
		}, nil
	}

	got := WithSystemdUserLocale([]string{"PATH=/usr/bin", "LANG=en_US.UTF-8"}, log.New(io.Discard, "", log.LstdFlags))
	want := []string{"PATH=/usr/bin", "LANG=zh_CN.UTF-8", "LC_MESSAGES=zh_CN.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %#v, want %#v", got, want)
	}
}
