package imapcheck

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mail-notify/internal/goa"
)

type Status struct {
	UIDNext uint32
	Unseen  uint32
}

type Options struct {
	TLSOverrides map[string]TLSOverride `json:"tls_overrides"`
}

type TLSOverride struct {
	ServerName       string   `json:"server_name,omitempty"`
	AllowedCertNames []string `json:"allowed_cert_names,omitempty"`
}

type conn struct {
	netConn net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	nextTag int
}

var statusItemRE = regexp.MustCompile(`(?i)\b(UIDNEXT|UNSEEN)\s+([0-9]+)\b`)

func Check(ctx context.Context, settings goa.IMAPSettings, credentials goa.Credentials, options Options) (Status, error) {
	address := imapAddress(settings)
	tlsOverride := options.tlsOverride(settings.Host)
	dialer := net.Dialer{Timeout: 20 * time.Second}

	var networkConn net.Conn
	var err error
	if settings.UseSSL {
		networkConn, err = tls.DialWithDialer(&dialer, "tcp", address, tlsConfig(settings.Host, tlsOverride))
	} else {
		networkConn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return Status{}, fmt.Errorf("connect %s: %w", address, err)
	}
	defer networkConn.Close()

	client := &conn{
		netConn: networkConn,
		reader:  bufio.NewReader(networkConn),
		writer:  bufio.NewWriter(networkConn),
	}

	if err := client.expectGreeting(); err != nil {
		return Status{}, err
	}

	if settings.UseTLS && !settings.UseSSL {
		if err := client.startTLS(settings.Host, tlsOverride); err != nil {
			return Status{}, err
		}
	}

	if credentials.OAuth2AccessToken != "" {
		if err := client.authenticateXOAUTH2(settings.User, credentials.OAuth2AccessToken); err != nil {
			return Status{}, fmt.Errorf("xoauth2 auth: %w", err)
		}
	} else if credentials.Password != "" {
		if _, err := client.command("LOGIN %s %s", quote(settings.User), quote(credentials.Password)); err != nil {
			return Status{}, fmt.Errorf("login: %w", err)
		}
	} else {
		return Status{}, errors.New("missing IMAP credentials")
	}

	status, err := client.status("INBOX")
	if err != nil {
		return Status{}, err
	}

	_, _ = client.command("LOGOUT")
	return status, nil
}

func (c *conn) expectGreeting() error {
	line, err := c.readLine()
	if err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if !strings.HasPrefix(strings.ToUpper(line), "* OK") {
		return fmt.Errorf("unexpected IMAP greeting: %s", line)
	}
	return nil
}

func (c *conn) startTLS(host string, override TLSOverride) error {
	if _, err := c.command("STARTTLS"); err != nil {
		return err
	}
	tlsConn := tls.Client(c.netConn, tlsConfig(host, override))
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("tls handshake: %w", err)
	}
	c.netConn = tlsConn
	c.reader = bufio.NewReader(tlsConn)
	c.writer = bufio.NewWriter(tlsConn)
	return nil
}

func (c *conn) authenticateXOAUTH2(user, token string) error {
	tag := c.tag()
	if err := c.writeLine(fmt.Sprintf("%s AUTHENTICATE XOAUTH2", tag)); err != nil {
		return err
	}

	line, err := c.readLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "+") {
		return fmt.Errorf("expected continuation, got %s", line)
	}

	payload := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", user, token)
	if err := c.writeLine(base64.StdEncoding.EncodeToString([]byte(payload))); err != nil {
		return err
	}

	for {
		line, err := c.readLine()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		if isOK(line) {
			return nil
		}
		return fmt.Errorf("%s", line)
	}
}

func (c *conn) status(mailbox string) (Status, error) {
	lines, err := c.command("STATUS %s (UIDNEXT UNSEEN)", quoteMailbox(mailbox))
	if err != nil {
		return Status{}, fmt.Errorf("status %s: %w", mailbox, err)
	}

	var status Status
	var hasUIDNext, hasUnseen bool
	for _, line := range lines {
		parsed, parsedUIDNext, parsedUnseen := parseStatusLine(line)
		if parsedUIDNext {
			status.UIDNext = parsed.UIDNext
			hasUIDNext = true
		}
		if parsedUnseen {
			status.Unseen = parsed.Unseen
			hasUnseen = true
		}
	}

	var fallbackErr error
	if !hasUIDNext || !hasUnseen {
		examineStatus, examineUIDNext, examineUnseen, err := c.examineStatus(mailbox)
		if err == nil {
			if examineUIDNext {
				status.UIDNext = examineStatus.UIDNext
				hasUIDNext = true
			}
			if !hasUnseen && examineUnseen {
				status.Unseen = examineStatus.Unseen
				hasUnseen = true
			}
		} else {
			fallbackErr = err
		}

		if !hasUnseen && err == nil {
			unseen, err := c.unseenSearchCount()
			if err == nil {
				status.Unseen = unseen
				hasUnseen = true
			} else {
				fallbackErr = err
			}
		}
	}

	if hasUnseen {
		return status, nil
	}
	if fallbackErr != nil {
		return Status{}, fmt.Errorf("STATUS response did not include UNSEEN and fallback failed: %w; status response: %v", fallbackErr, lines)
	}
	return Status{}, fmt.Errorf("STATUS response did not include UNSEEN: %v", lines)
}

func (c *conn) examineStatus(mailbox string) (Status, bool, bool, error) {
	lines, err := c.command("EXAMINE %s", quoteMailbox(mailbox))
	if err != nil {
		return Status{}, false, false, err
	}

	var status Status
	var hasUIDNext, hasUnseen bool
	for _, line := range lines {
		parsed, parsedUIDNext, parsedUnseen := parseStatusLine(line)
		if parsedUIDNext {
			status.UIDNext = parsed.UIDNext
			hasUIDNext = true
		}
		if parsedUnseen {
			status.Unseen = parsed.Unseen
			hasUnseen = true
		}
	}

	return status, hasUIDNext, hasUnseen, nil
}

func (c *conn) unseenSearchCount() (uint32, error) {
	lines, err := c.command("UID SEARCH UNSEEN")
	if err != nil {
		return 0, err
	}

	for _, line := range lines {
		if count, ok := parseSearchCount(line); ok {
			return count, nil
		}
	}

	return 0, fmt.Errorf("SEARCH response did not include untagged SEARCH line: %v", lines)
}

func parseStatusLine(line string) (Status, bool, bool) {
	matches := statusItemRE.FindAllStringSubmatch(line, -1)
	if len(matches) == 0 {
		return Status{}, false, false
	}

	isStatusLine := strings.HasPrefix(strings.ToUpper(line), "* STATUS ")
	var status Status
	var hasUIDNext, hasUnseen bool
	for _, match := range matches {
		value, _ := strconv.ParseUint(match[2], 10, 32)
		switch strings.ToUpper(match[1]) {
		case "UIDNEXT":
			status.UIDNext = uint32(value)
			hasUIDNext = true
		case "UNSEEN":
			if !isStatusLine {
				continue
			}
			status.Unseen = uint32(value)
			hasUnseen = true
		}
	}

	return status, hasUIDNext, hasUnseen
}

func parseSearchCount(line string) (uint32, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}
	if fields[0] != "*" || !strings.EqualFold(fields[1], "SEARCH") {
		return 0, false
	}
	return uint32(len(fields) - 2), true
}

func (c *conn) command(format string, args ...any) ([]string, error) {
	tag := c.tag()
	if err := c.writeLine(tag + " " + fmt.Sprintf(format, args...)); err != nil {
		return nil, err
	}

	lines := make([]string, 0, 4)
	for {
		line, err := c.readLine()
		if err != nil {
			return lines, err
		}
		lines = append(lines, line)
		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		if isOK(line) {
			return lines, nil
		}
		return lines, fmt.Errorf("%s", line)
	}
}

func (c *conn) tag() string {
	c.nextTag++
	return fmt.Sprintf("A%04d", c.nextTag)
}

func (c *conn) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *conn) writeLine(line string) error {
	if _, err := c.writer.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return c.writer.Flush()
}

func imapAddress(settings goa.IMAPSettings) string {
	host := settings.Host
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}

	port := settings.Port
	if port == 0 {
		if settings.UseSSL {
			port = 993
		} else {
			port = 143
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (o Options) tlsOverride(host string) TLSOverride {
	if len(o.TLSOverrides) == 0 {
		return TLSOverride{}
	}
	if override, ok := o.TLSOverrides[normalizeHost(host)]; ok {
		return override
	}
	return o.TLSOverrides[host]
}

func tlsConfig(host string, override TLSOverride) *tls.Config {
	serverName := normalizeHost(host)
	if override.ServerName != "" {
		serverName = normalizeHost(override.ServerName)
	}

	config := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if len(override.AllowedCertNames) == 0 {
		return config
	}

	allowedNames := make([]string, 0, len(override.AllowedCertNames))
	for _, name := range override.AllowedCertNames {
		if normalized := normalizeHost(name); normalized != "" {
			allowedNames = append(allowedNames, normalized)
		}
	}
	if len(allowedNames) == 0 {
		return config
	}

	config.InsecureSkipVerify = true
	config.VerifyConnection = func(state tls.ConnectionState) error {
		return verifyCertificateOverride(state, allowedNames)
	}
	return config
}

func verifyCertificateOverride(state tls.ConnectionState, allowedNames []string) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("server did not provide a certificate")
	}

	leaf := state.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	_, err := leaf.Verify(x509.VerifyOptions{
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime:   time.Now(),
	})
	if err != nil {
		return fmt.Errorf("verify certificate chain: %w", err)
	}

	for _, name := range allowedNames {
		if certificateMatchesAllowedName(leaf, name) {
			return nil
		}
	}

	return fmt.Errorf("certificate names %v are not allowed by override %v", certificateNames(leaf), allowedNames)
}

func certificateMatchesAllowedName(cert *x509.Certificate, allowedName string) bool {
	if strings.Contains(allowedName, "*") {
		for _, name := range cert.DNSNames {
			if normalizeHost(name) == allowedName {
				return true
			}
		}
		return false
	}
	return cert.VerifyHostname(allowedName) == nil
}

func certificateNames(cert *x509.Certificate) []string {
	names := make([]string, 0, len(cert.DNSNames)+len(cert.IPAddresses))
	names = append(names, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		names = append(names, ip.String())
	}
	return names
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

func quote(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func quoteMailbox(value string) string {
	if value == "INBOX" {
		return value
	}
	return quote(value)
}

func isOK(line string) bool {
	upper := strings.ToUpper(line)
	return strings.Contains(upper, " OK ")
}
