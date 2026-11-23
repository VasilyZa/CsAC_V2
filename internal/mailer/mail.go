// Package mailer sends account challenges over a verified TLS SMTP connection.
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type Sender struct {
	Host, From, Username, Password string
	Port                           int
}

func (s Sender) Send(ctx context.Context, recipient, subject, body string) error {
	if s.Host == "" || s.From == "" {
		return errors.New("SMTP is not configured")
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return errors.New("invalid SMTP sender")
	}
	to, err := mail.ParseAddress(recipient)
	if err != nil {
		return errors.New("invalid recipient address")
	}
	address := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	tlsConfig := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	if s.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: &dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if s.Port != 465 {
		if present, _ := client.Extension("STARTTLS"); !present {
			return errors.New("SMTP STARTTLS unavailable")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if s.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return err
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to.Address); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	messageBody := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	_, err = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", from, to, mime.QEncoding.Encode("UTF-8", strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)), messageBody)
	if err != nil {
		return err
	}
	return writer.Close()
}
