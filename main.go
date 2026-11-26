package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"
)

const maxMessageBytes = 64 << 10

var (
	localPartPattern = regexp.MustCompile(`^[a-z0-9_-]{1,48}$`)
	topicPattern     = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	codePattern      = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)
)

type config struct {
	listenAddr    string
	allowedDomain string
	topicTmpl     string
	ntfyURL       *url.URL
	username      string
	password      string
	timeout       time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		listenAddr:    envOr("LISTEN_ADDR", ":2525"),
		allowedDomain: strings.ToLower(strings.TrimSpace(os.Getenv("ALLOWED_DOMAIN"))),
		topicTmpl:     envOr("TOPIC_TMPL", "multica-{recipient}"),
		username:      os.Getenv("NTFY_USERNAME"),
		password:      os.Getenv("NTFY_PASSWORD"),
	}

	_, topicTmplSet := os.LookupEnv("TOPIC_TMPL")
	if prefix, legacySet := os.LookupEnv("TOPIC_PREFIX"); legacySet && !topicTmplSet {
		replacement := cfg.topicTmpl
		if prefix != "" {
			replacement = prefix + "{recipient}"
		}
		return config{}, fmt.Errorf("TOPIC_PREFIX has been removed. Use TOPIC_TMPL=%s", replacement)
	}

	if cfg.allowedDomain == "" || strings.ContainsAny(cfg.allowedDomain, "@/ ") {
		return config{}, errors.New("ALLOWED_DOMAIN must be a single domain")
	}
	if !topicPattern.MatchString(renderTopic(cfg.topicTmpl, "a", "a")) {
		return config{}, errors.New("TOPIC_TMPL must use only lowercase letters, digits, underscores, hyphens, {sender}, or {recipient} and allow a 1-64 character topic")
	}
	if cfg.username == "" || cfg.password == "" {
		return config{}, errors.New("NTFY_USERNAME and NTFY_PASSWORD are required")
	}

	var err error
	cfg.ntfyURL, err = url.Parse(os.Getenv("NTFY_BASE_URL"))
	if err != nil || cfg.ntfyURL.Scheme != "https" || cfg.ntfyURL.Host == "" || cfg.ntfyURL.User != nil || cfg.ntfyURL.RawQuery != "" || cfg.ntfyURL.Fragment != "" {
		return config{}, errors.New("NTFY_BASE_URL must be an HTTPS URL without credentials, query, or fragment")
	}

	cfg.timeout, err = time.ParseDuration(envOr("HTTP_TIMEOUT", "3s"))
	if err != nil || cfg.timeout <= 0 {
		return config{}, errors.New("HTTP_TIMEOUT must be a positive duration")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func renderTopic(tmpl, sender, recipient string) string {
	return strings.NewReplacer("{sender}", sender, "{recipient}", recipient).Replace(tmpl)
}

type backend struct {
	domain    string
	topicTmpl string
	baseURL   *url.URL
	username  string
	password  string
	client    *http.Client
	logger    *log.Logger
}

func (b *backend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &session{backend: b}, nil
}

type session struct {
	backend *backend
	sender  string
	topic   string
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.Reset()
	if at := strings.LastIndexByte(from, '@'); at >= 0 {
		s.sender = strings.ToLower(from[:at])
	}
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.topic != "" {
		return &smtp.SMTPError{Code: 550, Message: "only one recipient is allowed"}
	}

	local, domain, ok := strings.Cut(to, "@")
	local = strings.ToLower(local)
	if !ok || strings.Contains(domain, "@") || !strings.EqualFold(domain, s.backend.domain) || !localPartPattern.MatchString(local) {
		return &smtp.SMTPError{Code: 550, Message: "recipient rejected"}
	}
	if s.sender == "" && strings.Contains(s.backend.topicTmpl, "{sender}") {
		return &smtp.SMTPError{Code: 550, Message: "sender required by TOPIC_TMPL"}
	}
	topic := renderTopic(s.backend.topicTmpl, s.sender, local)
	if !topicPattern.MatchString(topic) {
		return &smtp.SMTPError{Code: 550, Message: "invalid rendered topic"}
	}
	s.topic = topic
	return nil
}

func (s *session) Data(r io.Reader) error {
	content, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	code, err := extractCode(bytes.NewReader(content))
	if err != nil || code == "" || s.topic == "" {
		return nil
	}
	if err := s.backend.push(s.topic, code); err != nil {
		s.backend.logger.Print("ntfy push failed")
	}
	return nil
}

func (s *session) Reset() {
	s.sender = ""
	s.topic = ""
}

func (s *session) Logout() error { return nil }

func (b *backend) push(topic, code string) error {
	target := *b.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + topic
	req, err := http.NewRequest(http.MethodPost, target.String(), strings.NewReader(code))
	if err != nil {
		return err
	}
	req.SetBasicAuth(b.username, b.password)
	req.Header.Set("Cache", "no")
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	client := *b.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned HTTP %d", resp.StatusCode)
	}
	return nil
}

type mimeHeader interface {
	Get(string) string
}

func extractCode(r io.Reader) (string, error) {
	message, err := mail.ReadMessage(r)
	if err != nil {
		return "", err
	}
	return codeFromPart(message.Header, message.Body)
}

func codeFromPart(header mimeHeader, body io.Reader) (string, error) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}

	if disposition, _, _ := mime.ParseMediaType(header.Get("Content-Disposition")); disposition == "attachment" {
		return "", nil
	}
	if strings.EqualFold(header.Get("Content-Transfer-Encoding"), "quoted-printable") {
		body = quotedprintable.NewReader(body)
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", errors.New("multipart message has no boundary")
		}
		parts := multipart.NewReader(body, boundary)
		for {
			part, err := parts.NextPart()
			if errors.Is(err, io.EOF) {
				return "", nil
			}
			if err != nil {
				return "", err
			}
			code, err := codeFromPart(part.Header, part)
			part.Close()
			if err != nil || code != "" {
				return code, err
			}
		}
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		return "", nil
	}

	content, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	match := codePattern.FindSubmatch(content)
	if len(match) == 2 {
		return string(match[1]), nil
	}
	return "", nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "smtp2ntfy: ", log.LstdFlags)
	be := &backend{
		domain:    cfg.allowedDomain,
		topicTmpl: cfg.topicTmpl,
		baseURL:   cfg.ntfyURL,
		username:  cfg.username,
		password:  cfg.password,
		client:    &http.Client{Timeout: cfg.timeout},
		logger:    logger,
	}
	server := smtp.NewServer(be)
	server.Addr = cfg.listenAddr
	server.Domain = "smtp2ntfy"
	server.MaxRecipients = 1
	server.MaxMessageBytes = maxMessageBytes
	server.ReadTimeout = 30 * time.Second
	server.WriteTimeout = 30 * time.Second
	server.ErrorLog = logger

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s", cfg.listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
