package imapcheck

import (
	"crypto/x509"
	"testing"
)

func TestParseStatusLine(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		want        Status
		wantUIDNext bool
		wantUnseen  bool
	}{
		{
			name:        "requested order",
			line:        "* STATUS INBOX (UIDNEXT 4242 UNSEEN 7)",
			want:        Status{UIDNext: 4242, Unseen: 7},
			wantUIDNext: true,
			wantUnseen:  true,
		},
		{
			name:        "server-chosen order",
			line:        "* STATUS INBOX (MESSAGES 30 UNSEEN 3 UIDNEXT 99)",
			want:        Status{UIDNext: 99, Unseen: 3},
			wantUIDNext: true,
			wantUnseen:  true,
		},
		{
			name:        "missing unseen",
			line:        "* STATUS INBOX (UIDNEXT 99)",
			want:        Status{UIDNext: 99},
			wantUIDNext: true,
		},
		{
			name:       "missing uidnext",
			line:       "* STATUS \"INBOX\" (UNSEEN 0)",
			want:       Status{Unseen: 0},
			wantUnseen: true,
		},
		{
			name:        "examine ok code",
			line:        "* OK [UIDNEXT 4243] Predicted next UID",
			want:        Status{UIDNext: 4243},
			wantUIDNext: true,
		},
		{
			name: "examine unseen is not count",
			line: "* OK [UNSEEN 4] Message 4 is first unseen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, hasUIDNext, hasUnseen := parseStatusLine(tt.line)
			if hasUIDNext != tt.wantUIDNext {
				t.Fatalf("hasUIDNext = %v, want %v", hasUIDNext, tt.wantUIDNext)
			}
			if hasUnseen != tt.wantUnseen {
				t.Fatalf("hasUnseen = %v, want %v", hasUnseen, tt.wantUnseen)
			}
			if got != tt.want {
				t.Fatalf("status = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseSearchCount(t *testing.T) {
	tests := []struct {
		name string
		line string
		want uint32
		ok   bool
	}{
		{
			name: "no unseen",
			line: "* SEARCH",
			ok:   true,
		},
		{
			name: "three unseen",
			line: "* SEARCH 74170 74172 74173",
			want: 3,
			ok:   true,
		},
		{
			name: "tagged completion",
			line: "A0004 OK SEARCH completed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseSearchCount(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("count = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTLSOverrideLookupNormalizesHost(t *testing.T) {
	options := Options{
		TLSOverrides: map[string]TLSOverride{
			"imap.corp.netease.com": {AllowedCertNames: []string{"*.netease.com"}},
		},
	}

	got := options.tlsOverride("imap.corp.netease.com:993")
	if len(got.AllowedCertNames) != 1 || got.AllowedCertNames[0] != "*.netease.com" {
		t.Fatalf("override = %+v", got)
	}
}

func TestCertificateMatchesAllowedWildcardSAN(t *testing.T) {
	cert := &x509.Certificate{
		DNSNames: []string{"*.netease.com", "netease.com"},
	}

	if !certificateMatchesAllowedName(cert, "*.netease.com") {
		t.Fatal("expected exact wildcard SAN override to match")
	}
	if !certificateMatchesAllowedName(cert, "imap.netease.com") {
		t.Fatal("expected normal hostname to match wildcard SAN")
	}
	if certificateMatchesAllowedName(cert, "imap.corp.netease.com") {
		t.Fatal("expected nested hostname not to match wildcard SAN")
	}
}
