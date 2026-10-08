package billing

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Notifier delivers spend alerts to a tenant.
type Notifier interface {
	NotifySpend(ctx context.Context, alert SpendAlert) error
}

// SpendAlert is one notice that a tenant's spend this month has crossed one or
// more of its thresholds. Several crossed by one usage report are one notice.
type SpendAlert struct {
	Tenant Tenant
	Month  time.Time
	Spent  Micros
	// Threshold is the highest crossed; Thresholds is all of them, ascending.
	Threshold  Micros
	Thresholds []Micros
	// Limit is the tenant's spend limit, zero if it has none.
	Limit Micros
}

// LimitReached reports whether new sessions are now being refused.
func (a SpendAlert) LimitReached() bool { return a.Limit > 0 && a.Spent >= a.Limit }

// KeyLeakNotifier tells a tenant one of its keys was found in public and
// revoked. Separate from Notifier so a notifier that only does spend alerts
// still satisfies it.
type KeyLeakNotifier interface {
	NotifyLeakedKey(ctx context.Context, leak LeakedKey) error
}

// LeakedKey is a key a secret scanner reported, never including the secret.
type LeakedKey struct {
	Tenant    Tenant
	Key       Key
	URL       string
	Source    string
	RevokedAt time.Time
	// WasWorking is false for a key that had already stopped, which is not
	// noticed.
	WasWorking bool
}

// LogNotifier logs what it would have sent, for a deployment with no SMTP
// server. The events are the same ones an alert policy can match.
type LogNotifier struct {
	Log *slog.Logger
}

var (
	_ Notifier        = LogNotifier{}
	_ KeyLeakNotifier = LogNotifier{}
	_ KeyLeakNotifier = (*SMTPNotifier)(nil)
)

func (n LogNotifier) logger() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.Default()
}

func (n LogNotifier) NotifySpend(ctx context.Context, alert SpendAlert) error {
	n.logger().InfoContext(ctx, "spend alert not emailed: no SMTP server configured",
		"event", "billing_spend_alert_unsent", "tenant", alert.Tenant.ID, "threshold", alert.Threshold.String())
	return nil
}

func (n LogNotifier) NotifyLeakedKey(ctx context.Context, leak LeakedKey) error {
	n.logger().WarnContext(ctx, "leaked key notice not emailed: no SMTP server configured",
		"event", "api_key_leak_notice_unsent", "tenant", leak.Key.Tenant, "key", leak.Key.ID)
	return nil
}

// NotifyLeakedKey emails the tenant's billing address, which is the account's
// contact address.
func (n *SMTPNotifier) NotifyLeakedKey(ctx context.Context, leak LeakedKey) error {
	to, err := headerAddress(leak.Tenant.Email)
	if err != nil {
		return fmt.Errorf("tenant %s email: %w", leak.Tenant.ID, err)
	}
	from, err := headerAddress(n.From)
	if err != nil {
		return fmt.Errorf("sender address: %w", err)
	}
	message, err := leakedKeyMessage(leak, from, to, n.ConsoleURL, time.Now())
	if err != nil {
		return err
	}
	return n.send(ctx, from, to, message)
}

func leakedKeyMessage(leak LeakedKey, from, to, console string, now time.Time) ([]byte, error) {
	subject := fmt.Sprintf("MOQOM API key %q was found in public and has been revoked", leak.Key.Name)
	var body strings.Builder
	fmt.Fprintf(&body, "Tenant: %s (%s)\n", leak.Tenant.Name, leak.Tenant.ID)
	fmt.Fprintf(&body, "Key: %s (ID %s, kind %s)\n", leak.Key.Name, leak.Key.ID, leak.Key.Kind)
	fmt.Fprintf(&body, "Revoked: %s\n", leak.RevokedAt.UTC().Format(time.RFC1123))
	if leak.URL != "" {
		fmt.Fprintf(&body, "Found at: %s\n", leak.URL)
	}
	if leak.Source != "" {
		fmt.Fprintf(&body, "Found in: GitHub %s\n", strings.ReplaceAll(leak.Source, "_", " "))
	}
	body.WriteString("\nGitHub secret scanning reported this key in a public place. Anything public is copied " +
		"within minutes, so the key was revoked at once and no longer works.\n\n")
	if leak.Key.Kind == KeyBackend {
		body.WriteString("If this was your only backend key, your backend can no longer mint tokens: new sessions " +
			"are refused and live ones end within 15 minutes. Issue a new key (POST /v1/keys) with another backend " +
			"key or from the console, deploy it, and remove the old one from where it was published and from its history.\n")
	} else {
		body.WriteString("Issue a replacement (POST /v1/keys), deploy it, and remove the old key from where it " +
			"was published and from its history.\n")
	}
	body.WriteString("\nThe key's use, including this revocation, is in GET /v1/keys/" + leak.Key.ID + "/audit.\n")
	if console != "" {
		fmt.Fprintf(&body, "%s\n", console)
	}
	return plainMessage(from, to, subject, body.String(), now)
}

// plainMessage is a quoted-printable text/plain email.
func plainMessage(from, to, subject, text string, now time.Time) ([]byte, error) {
	if strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("subject contains a line break")
	}
	var encoded bytes.Buffer
	writer := quotedprintable.NewWriter(&encoded)
	if _, err := writer.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\n", from)
	fmt.Fprintf(&message, "To: %s\r\n", to)
	fmt.Fprintf(&message, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&message, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	message.WriteString("MIME-Version: 1.0\r\n")
	message.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	message.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	message.WriteString("Auto-Submitted: auto-generated\r\n")
	message.WriteString("\r\n")
	message.Write(encoded.Bytes())
	return message.Bytes(), nil
}

// SMTPNotifier emails a spend alert to the tenant's billing address.
type SMTPNotifier struct {
	Host string
	// Port is 587 by default, which must offer STARTTLS. 465 is implicit TLS.
	Port     string
	Username string
	Password string
	From     string
	// ConsoleURL is linked from the message, for changing the alerts.
	ConsoleURL string
	// AllowPlaintext sends without TLS to a server that does not offer
	// STARTTLS. For a relay on the same host; never set it for a remote one.
	AllowPlaintext bool
}

var _ Notifier = (*SMTPNotifier)(nil)

// NotifySpend sends one email, bounded by ctx.
func (n *SMTPNotifier) NotifySpend(ctx context.Context, alert SpendAlert) error {
	to, err := headerAddress(alert.Tenant.Email)
	if err != nil {
		return fmt.Errorf("tenant %s billing email: %w", alert.Tenant.ID, err)
	}
	from, err := headerAddress(n.From)
	if err != nil {
		return fmt.Errorf("sender address: %w", err)
	}
	message, err := spendAlertMessage(alert, from, to, n.ConsoleURL, time.Now())
	if err != nil {
		return err
	}
	return n.send(ctx, from, to, message)
}

// headerAddress refuses anything that could add a header.
func headerAddress(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("address contains a line break")
	}
	address, err := mail.ParseAddress(raw)
	if err != nil {
		return "", err
	}
	return address.Address, nil
}

// dollars is m in dollars and cents, rounded toward zero, for people.
func dollars(m Micros) string {
	cents := m.Cents()
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

func spendAlertMessage(alert SpendAlert, from, to, console string, now time.Time) ([]byte, error) {
	month := alert.Month.Format("January 2006")
	subject := fmt.Sprintf("MOQOM spend alert: %s of usage in %s", dollars(alert.Spent), month)
	if alert.LimitReached() {
		subject = fmt.Sprintf("MOQOM spend limit reached: new sessions are paused for %s", month)
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Tenant: %s (%s)\n", alert.Tenant.Name, alert.Tenant.ID)
	fmt.Fprintf(&body, "Usage charged in %s (UTC): %s\n", month, dollars(alert.Spent))
	crossed := make([]string, 0, len(alert.Thresholds))
	for _, threshold := range alert.Thresholds {
		crossed = append(crossed, dollars(threshold))
	}
	fmt.Fprintf(&body, "Alert threshold crossed: %s\n", strings.Join(crossed, ", "))
	if alert.Limit > 0 {
		fmt.Fprintf(&body, "Monthly spend limit: %s\n", dollars(alert.Limit))
	}
	body.WriteString("\n")
	switch {
	case alert.LimitReached():
		body.WriteString("The spend limit has been reached. New sessions and client tokens are refused until the " +
			"limit is raised or removed, or the month ends. Sessions already live continue until their current " +
			"token expires (at most 15 minutes) and its renewal is refused. Moderation and administration keep working.\n")
	case alert.Limit > 0:
		body.WriteString("When usage reaches the spend limit, new sessions will be refused. Raise or remove the " +
			"limit with PUT /v1/billing/spend-limit.\n")
	default:
		body.WriteString("No spend limit is set; sessions are unaffected. This is a notice only.\n")
	}
	body.WriteString("\nEach threshold alerts at most once per calendar month. Change thresholds with " +
		"PUT /v1/billing/spend-alerts.\n")
	if console != "" {
		fmt.Fprintf(&body, "%s\n", console)
	}

	var encoded bytes.Buffer
	writer := quotedprintable.NewWriter(&encoded)
	if _, err := writer.Write([]byte(strings.ReplaceAll(body.String(), "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\n", from)
	fmt.Fprintf(&message, "To: %s\r\n", to)
	fmt.Fprintf(&message, "Subject: %s\r\n", subject)
	fmt.Fprintf(&message, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	message.WriteString("MIME-Version: 1.0\r\n")
	message.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	message.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	message.WriteString("Auto-Submitted: auto-generated\r\n")
	message.WriteString("\r\n")
	message.Write(encoded.Bytes())
	return message.Bytes(), nil
}

func (n *SMTPNotifier) send(ctx context.Context, from, to string, message []byte) error {
	if n.Host == "" {
		return errors.New("no SMTP host configured")
	}
	port := n.Port
	if port == "" {
		port = "587"
	}
	address := net.JoinHostPort(n.Host, port)
	secure := &tls.Config{ServerName: n.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if port == "465" {
		conn, err = (&tls.Dialer{Config: secure}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, n.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()

	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(secure); err != nil {
				return err
			}
		} else if !n.AllowPlaintext {
			return fmt.Errorf("%s does not offer STARTTLS; refusing to send in plaintext", address)
		}
	}
	if n.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", n.Username, n.Password, n.Host)); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := data.Write(message); err != nil {
		return err
	}
	if err := data.Close(); err != nil {
		return err
	}
	return client.Quit()
}
