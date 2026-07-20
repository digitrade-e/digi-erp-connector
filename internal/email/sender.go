package email

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"

	mail "github.com/wneessen/go-mail"
)

// Sender handles SMTP email delivery with PDF attachments.
type Sender struct {
	cfg  config.SMTPConfig
	pass string
}

// NewSender creates a new email Sender.
func NewSender(cfg config.SMTPConfig, password string) *Sender {
	return &Sender{cfg: cfg, pass: password}
}

// SendInvoice sends an invoice PDF to the specified recipient.
func (s *Sender) SendInvoice(ctx context.Context, recipientEmail string, pdfBytes []byte, invoiceNumber string) error {
	if recipientEmail == "" {
		return fmt.Errorf("recipient email is required")
	}

	m := mail.NewMsg()
	if err := m.From(s.cfg.FromAddress); err != nil {
		return fmt.Errorf("set from address: %w", err)
	}
	if err := m.To(recipientEmail); err != nil {
		return fmt.Errorf("set to address: %w", err)
	}

	m.Subject(fmt.Sprintf("מסמך מספר %s", invoiceNumber))
	m.SetBodyString(mail.TypeTextHTML, fmt.Sprintf(
		`<div dir="rtl" style="font-family: Arial, sans-serif;">
			<p>שלום,</p>
			<p>מצורף מסמך מספר %s.</p>
			<p>בברכה</p>
		</div>`, invoiceNumber))

	filename := fmt.Sprintf("invoice_%s.pdf", invoiceNumber)
	m.AttachReader(filename, bytes.NewReader(pdfBytes))

	return s.send(ctx, m)
}

// SendTestEmail sends a test email with a sample PDF to the from address.
func (s *Sender) SendTestEmail(ctx context.Context, pdfBytes []byte) error {
	m := mail.NewMsg()
	if err := m.From(s.cfg.FromAddress); err != nil {
		return fmt.Errorf("set from address: %w", err)
	}
	if err := m.To(s.cfg.FromAddress); err != nil {
		return fmt.Errorf("set to address: %w", err)
	}

	m.Subject("[TEST] בדיקת חיבור דוא\"ל - ERP Connector")
	m.SetBodyString(mail.TypeTextHTML,
		`<div dir="rtl" style="font-family: Arial, sans-serif;">
			<p>זוהי הודעת בדיקה מ-ERP Connector.</p>
			<p>אם אתה מקבל הודעה זו, חיבור הדוא"ל פועל כראוי.</p>
		</div>`)

	if pdfBytes != nil {
		m.AttachReader("test_invoice.pdf", bytes.NewReader(pdfBytes))
	}

	return s.send(ctx, m)
}

// TestConnection verifies the SMTP connection without sending an email.
func (s *Sender) TestConnection(ctx context.Context) error {
	c, err := s.newClient()
	if err != nil {
		return err
	}
	defer c.Close()

	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = sendCtx // go-mail client doesn't use context for dial

	return c.DialWithContext(sendCtx)
}

func (s *Sender) send(ctx context.Context, m *mail.Msg) error {
	c, err := s.newClient()
	if err != nil {
		return err
	}

	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return c.DialAndSendWithContext(sendCtx, m)
}

func (s *Sender) newClient() (*mail.Client, error) {
	opts := []mail.Option{
		mail.WithPort(s.cfg.Port),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithUsername(s.cfg.User),
		mail.WithPassword(s.pass),
	}

	if s.cfg.UseTLS {
		opts = append(opts, mail.WithTLSPortPolicy(mail.TLSMandatory))
	}

	return mail.NewClient(s.cfg.Host, opts...)
}

