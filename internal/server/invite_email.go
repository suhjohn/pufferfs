package server

import (
	"context"
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/pufferfs/pufferfs/pkg/models"
)

type orgInviteResponse struct {
	models.OrgInvite
	EmailSent  bool   `json:"email_sent"`
	EmailError string `json:"email_error,omitempty"`
}

// OrgInviteEmail is the data needed to notify a person about a pending invite.
type OrgInviteEmail struct {
	To           string
	Role         string
	OrgName      string
	InviterID    string
	InviterEmail string
}

// TransactionalEmailSender sends best-effort product emails.
type TransactionalEmailSender interface {
	SendOrgInvite(ctx context.Context, invite OrgInviteEmail) error
	SendLoginCode(ctx context.Context, login LoginCodeEmail) error
}

// InviteEmailSender is kept as a compatibility alias for older server wiring.
type InviteEmailSender = TransactionalEmailSender

// LoginCodeEmail is the data needed to send a one-time email login code.
type LoginCodeEmail struct {
	To        string
	Code      string
	ExpiresIn time.Duration
	AppURL    string
}

type SESTransactionalEmailConfig struct {
	Region              string
	EndpointURL         string
	FromEmail           string
	FromName            string
	ReplyTo             []string
	AppURL              string
	ConfigurationSet    string
	FromIdentityARN     string
	FeedbackEmail       string
	FeedbackIdentityARN string
}

type SESTransactionalEmailSender struct {
	client *sesv2.Client
	cfg    SESTransactionalEmailConfig
}

// SESInviteEmailConfig and SESInviteEmailSender are compatibility aliases.
type SESInviteEmailConfig = SESTransactionalEmailConfig
type SESInviteEmailSender = SESTransactionalEmailSender

func NewSESTransactionalEmailSender(ctx context.Context, cfg SESTransactionalEmailConfig) (*SESTransactionalEmailSender, error) {
	cfg.FromEmail = strings.TrimSpace(cfg.FromEmail)
	cfg.FromName = strings.TrimSpace(cfg.FromName)
	cfg.AppURL = strings.TrimRight(strings.TrimSpace(cfg.AppURL), "/")
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.EndpointURL = strings.TrimSpace(cfg.EndpointURL)
	cfg.ConfigurationSet = strings.TrimSpace(cfg.ConfigurationSet)
	cfg.FromIdentityARN = strings.TrimSpace(cfg.FromIdentityARN)
	cfg.FeedbackEmail = strings.TrimSpace(cfg.FeedbackEmail)
	cfg.FeedbackIdentityARN = strings.TrimSpace(cfg.FeedbackIdentityARN)

	if cfg.FromEmail == "" {
		return nil, fmt.Errorf("INVITE_EMAIL_FROM is required")
	}
	fromAddress, err := mail.ParseAddress(cfg.FromEmail)
	if err != nil {
		return nil, fmt.Errorf("parsing INVITE_EMAIL_FROM: %w", err)
	}
	if cfg.FromName == "" {
		cfg.FromName = fromAddress.Name
	}
	cfg.FromEmail = fromAddress.Address
	if cfg.AppURL == "" {
		return nil, fmt.Errorf("INVITE_EMAIL_APP_URL or FRONTEND_URL is required")
	}
	if parsed, err := url.Parse(cfg.AppURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if err != nil {
			return nil, fmt.Errorf("parsing invite app URL: %w", err)
		}
		return nil, fmt.Errorf("invite app URL must include scheme and host")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for SES: %w", err)
	}
	client := sesv2.NewFromConfig(awsCfg, func(o *sesv2.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		}
	})
	return &SESTransactionalEmailSender{client: client, cfg: cfg}, nil
}

func NewSESInviteEmailSender(ctx context.Context, cfg SESInviteEmailConfig) (*SESInviteEmailSender, error) {
	return NewSESTransactionalEmailSender(ctx, cfg)
}

func NewTransactionalEmailSenderFromEnv(ctx context.Context) (*SESTransactionalEmailSender, error) {
	from := firstEnv("TRANSACTIONAL_EMAIL_FROM", "INVITE_EMAIL_FROM")
	if from == "" {
		return nil, nil
	}
	appURL := firstEnv("TRANSACTIONAL_EMAIL_APP_URL", "INVITE_EMAIL_APP_URL")
	if appURL == "" {
		appURL = strings.TrimSpace(os.Getenv("FRONTEND_URL"))
	}
	region := strings.TrimSpace(os.Getenv("SES_REGION"))
	if region == "" {
		region = strings.TrimSpace(os.Getenv("AWS_REGION"))
	}
	if region == "" {
		region = strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION"))
	}
	return NewSESTransactionalEmailSender(ctx, SESTransactionalEmailConfig{
		Region:              region,
		EndpointURL:         os.Getenv("SES_ENDPOINT_URL"),
		FromEmail:           from,
		FromName:            firstEnv("TRANSACTIONAL_EMAIL_FROM_NAME", "INVITE_EMAIL_FROM_NAME"),
		ReplyTo:             splitEmailList(firstEnv("TRANSACTIONAL_EMAIL_REPLY_TO", "INVITE_EMAIL_REPLY_TO")),
		AppURL:              appURL,
		ConfigurationSet:    os.Getenv("SES_CONFIGURATION_SET"),
		FromIdentityARN:     os.Getenv("SES_FROM_IDENTITY_ARN"),
		FeedbackEmail:       os.Getenv("SES_FEEDBACK_EMAIL"),
		FeedbackIdentityARN: os.Getenv("SES_FEEDBACK_IDENTITY_ARN"),
	})
}

func NewInviteEmailSenderFromEnv(ctx context.Context) (*SESInviteEmailSender, error) {
	return NewTransactionalEmailSenderFromEnv(ctx)
}

func (s *SESTransactionalEmailSender) SendOrgInvite(ctx context.Context, invite OrgInviteEmail) error {
	to := strings.TrimSpace(invite.To)
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("parsing invite recipient: %w", err)
	}

	subject, textBody, htmlBody := s.renderInvite(invite)
	charset := "UTF-8"
	from := s.fromAddress()
	input := &sesv2.SendEmailInput{
		FromEmailAddress: &from,
		Destination: &types.Destination{
			ToAddresses: []string{to},
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: &subject, Charset: &charset},
				Body: &types.Body{
					Text: &types.Content{Data: &textBody, Charset: &charset},
					Html: &types.Content{Data: &htmlBody, Charset: &charset},
				},
			},
		},
	}
	if len(s.cfg.ReplyTo) > 0 {
		input.ReplyToAddresses = s.cfg.ReplyTo
	}
	if s.cfg.ConfigurationSet != "" {
		input.ConfigurationSetName = &s.cfg.ConfigurationSet
	}
	if s.cfg.FromIdentityARN != "" {
		input.FromEmailAddressIdentityArn = &s.cfg.FromIdentityARN
	}
	if s.cfg.FeedbackEmail != "" {
		input.FeedbackForwardingEmailAddress = &s.cfg.FeedbackEmail
	}
	if s.cfg.FeedbackIdentityARN != "" {
		input.FeedbackForwardingEmailAddressIdentityArn = &s.cfg.FeedbackIdentityARN
	}

	if _, err := s.client.SendEmail(ctx, input); err != nil {
		return fmt.Errorf("sending SES invite email: %w", err)
	}
	return nil
}

func (s *SESTransactionalEmailSender) SendLoginCode(ctx context.Context, login LoginCodeEmail) error {
	to := strings.TrimSpace(login.To)
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("parsing login recipient: %w", err)
	}
	subject, textBody, htmlBody := s.renderLoginCode(login)
	charset := "UTF-8"
	from := s.fromAddress()
	input := &sesv2.SendEmailInput{
		FromEmailAddress: &from,
		Destination: &types.Destination{
			ToAddresses: []string{to},
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: &subject, Charset: &charset},
				Body: &types.Body{
					Text: &types.Content{Data: &textBody, Charset: &charset},
					Html: &types.Content{Data: &htmlBody, Charset: &charset},
				},
			},
		},
	}
	if len(s.cfg.ReplyTo) > 0 {
		input.ReplyToAddresses = s.cfg.ReplyTo
	}
	if s.cfg.ConfigurationSet != "" {
		input.ConfigurationSetName = &s.cfg.ConfigurationSet
	}
	if s.cfg.FromIdentityARN != "" {
		input.FromEmailAddressIdentityArn = &s.cfg.FromIdentityARN
	}
	if s.cfg.FeedbackEmail != "" {
		input.FeedbackForwardingEmailAddress = &s.cfg.FeedbackEmail
	}
	if s.cfg.FeedbackIdentityARN != "" {
		input.FeedbackForwardingEmailAddressIdentityArn = &s.cfg.FeedbackIdentityARN
	}
	if _, err := s.client.SendEmail(ctx, input); err != nil {
		return fmt.Errorf("sending SES login code email: %w", err)
	}
	return nil
}

func (s *SESTransactionalEmailSender) fromAddress() string {
	if s.cfg.FromName == "" {
		return s.cfg.FromEmail
	}
	return (&mail.Address{Name: s.cfg.FromName, Address: s.cfg.FromEmail}).String()
}

func (s *SESTransactionalEmailSender) renderInvite(invite OrgInviteEmail) (subject, textBody, htmlBody string) {
	orgName := strings.TrimSpace(invite.OrgName)
	if orgName == "" {
		orgName = "a PufferFS organization"
	}
	role := strings.TrimSpace(invite.Role)
	if role == "" {
		role = "viewer"
	}
	loginURL := s.cfg.AppURL + "/login"
	inviter := strings.TrimSpace(invite.InviterEmail)
	if inviter == "" {
		inviter = strings.TrimSpace(invite.InviterID)
	}

	subject = fmt.Sprintf("You are invited to join %s", orgName)
	if inviter != "" {
		textBody = fmt.Sprintf("%s invited you to join %s on PufferFS as %s.\n\nSign in with %s to accept the invite:\n%s\n\nIf you were not expecting this invite, you can ignore this email.\n", inviter, orgName, role, invite.To, loginURL)
	} else {
		textBody = fmt.Sprintf("You were invited to join %s on PufferFS as %s.\n\nSign in with %s to accept the invite:\n%s\n\nIf you were not expecting this invite, you can ignore this email.\n", orgName, role, invite.To, loginURL)
	}

	escapedOrg := html.EscapeString(orgName)
	escapedRole := html.EscapeString(role)
	escapedTo := html.EscapeString(invite.To)
	escapedURL := html.EscapeString(loginURL)
	escapedInviter := html.EscapeString(inviter)
	intro := fmt.Sprintf("You were invited to join <strong>%s</strong> on PufferFS as <strong>%s</strong>.", escapedOrg, escapedRole)
	if escapedInviter != "" {
		intro = fmt.Sprintf("<strong>%s</strong> invited you to join <strong>%s</strong> on PufferFS as <strong>%s</strong>.", escapedInviter, escapedOrg, escapedRole)
	}
	htmlBody = fmt.Sprintf(`<!doctype html>
<html>
  <body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#111827;line-height:1.5">
    <p>%s</p>
    <p>Sign in with <strong>%s</strong> to accept the invite.</p>
    <p><a href="%s" style="display:inline-block;background:#111827;color:#ffffff;text-decoration:none;padding:10px 14px;border-radius:6px">Accept invite</a></p>
    <p style="color:#6b7280;font-size:13px">If you were not expecting this invite, you can ignore this email.</p>
  </body>
</html>`, intro, escapedTo, escapedURL)
	return subject, textBody, htmlBody
}

func (s *SESTransactionalEmailSender) renderLoginCode(login LoginCodeEmail) (subject, textBody, htmlBody string) {
	expires := login.ExpiresIn
	if expires <= 0 {
		expires = 10 * time.Minute
	}
	minutes := int(expires.Round(time.Minute).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	appURL := strings.TrimRight(strings.TrimSpace(login.AppURL), "/")
	if appURL == "" {
		appURL = s.cfg.AppURL
	}
	loginURL := appURL + "/login"
	subject = "Your PufferFS login code"
	textBody = fmt.Sprintf("Your PufferFS login code is %s.\n\nIt expires in %d minutes. If you did not request this code, you can ignore this email.\n\n%s\n", login.Code, minutes, loginURL)

	escapedCode := html.EscapeString(login.Code)
	escapedURL := html.EscapeString(loginURL)
	htmlBody = fmt.Sprintf(`<!doctype html>
<html>
  <body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#111827;line-height:1.5">
    <p>Your PufferFS login code is:</p>
    <p style="font-size:24px;font-weight:700;letter-spacing:4px">%s</p>
    <p>This code expires in %d minutes.</p>
    <p><a href="%s" style="display:inline-block;background:#111827;color:#ffffff;text-decoration:none;padding:10px 14px;border-radius:6px">Open PufferFS</a></p>
    <p style="color:#6b7280;font-size:13px">If you did not request this code, you can ignore this email.</p>
  </body>
</html>`, escapedCode, minutes, escapedURL)
	return subject, textBody, htmlBody
}

func splitEmailList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
