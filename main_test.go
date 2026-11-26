package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	stdsmtp "net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

const testMessage = "From: sender@example.net\r\n" +
	"To: rogee@ipao.vip\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=UTF-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"<p>Your code is <strong>12=33=34=35=36</strong>.</p>\r\n"

func TestSMTPPublishesCode(t *testing.T) {
	for _, test := range []struct {
		name  string
		tmpl  string
		from  string
		topic string
	}{
		{name: "fixed", tmpl: "all-codes", from: "", topic: "all-codes"},
		{name: "recipient", tmpl: "multica-{recipient}", from: "first.last@example.net", topic: "multica-rogee"},
		{name: "sender", tmpl: "multica-{sender}", from: "Service@example.net", topic: "multica-service"},
		{name: "both", tmpl: "{sender}-{recipient}-{sender}", from: "Service@example.net", topic: "service-rogee-service"},
		{name: "missing sender", tmpl: "multica-{sender}", from: ""},
		{name: "invalid sender", tmpl: "multica-{sender}", from: "first.last@example.net"},
		{name: "too long after rendering", tmpl: "{sender}-{sender}", from: strings.Repeat("a", 32) + "@example.net"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var called atomic.Bool
			ntfy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Store(true)
				if r.URL.Path != "/"+test.topic {
					t.Errorf("path = %q, want /%s", r.URL.Path, test.topic)
				}
				username, password, ok := r.BasicAuth()
				if !ok || username != "ntfy-user" || password != "ntfy-pass" {
					t.Errorf("BasicAuth() = %q, %q, %v", username, password, ok)
				}
				if got := r.Header.Get("Cache"); got != "no" {
					t.Errorf("Cache = %q", got)
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != "123456" {
					t.Errorf("body = %q", body)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer ntfy.Close()

			be := testBackend(t, ntfy.URL, ntfy.Client())
			be.topicTmpl = test.tmpl
			addr := startSMTP(t, be)
			err := stdsmtp.SendMail(addr, nil, test.from, []string{"ROGEE@ipao.vip"}, []byte(testMessage))
			if test.topic == "" {
				if smtpErr, ok := err.(*textproto.Error); !ok || smtpErr.Code != 550 {
					t.Fatalf("SMTP error = %v, want 550", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if called.Load() != (test.topic != "") {
				t.Fatalf("ntfy called = %v, topic = %q", called.Load(), test.topic)
			}
		})
	}
}

func TestSMTPRejectsInvalidRecipientsAndLargeMessages(t *testing.T) {
	ntfy := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ntfy.Close()
	addr := startSMTP(t, testBackend(t, ntfy.URL, ntfy.Client()))

	for _, recipient := range []string{"rogee@example.com", "first.last@ipao.vip"} {
		client, err := stdsmtp.Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Mail("sender@example.net"); err != nil {
			t.Fatal(err)
		}
		if err := client.Rcpt(recipient); err == nil {
			t.Errorf("recipient %q was accepted", recipient)
		}
		_ = client.Quit()
	}

	client, err := stdsmtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Mail("sender@example.net"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("one@ipao.vip"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("two@ipao.vip"); err == nil {
		t.Error("second recipient was accepted")
	}
	_ = client.Quit()

	raw, err := textproto.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, _, err := raw.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	command(t, raw, 250, "HELO test")
	command(t, raw, 250, "MAIL FROM:<sender@example.net>") // Deliberately no SIZE parameter.
	command(t, raw, 250, "RCPT TO:<rogee@ipao.vip>")
	command(t, raw, 354, "DATA")
	writer := raw.DotWriter()
	large := "From: sender@example.net\r\nTo: rogee@ipao.vip\r\n\r\n" + strings.Repeat("x\r\n", maxMessageBytes/3)
	if n, err := io.WriteString(writer, large); err != nil || n != len(large) {
		t.Fatalf("write oversized message: wrote %d of %d bytes: %v", n, len(large), err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.ReadResponse(552); err != nil {
		t.Fatalf("oversized message was not rejected: %v", err)
	}
}

func TestNtfyFailureDoesNotFailSMTP(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
	}{
		{
			name: "HTTP 500",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			timeout: time.Second,
		},
		{
			name: "timeout",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(50 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
			},
			timeout: 5 * time.Millisecond,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ntfy := httptest.NewTLSServer(test.handler)
			defer ntfy.Close()
			client := ntfy.Client()
			client.Timeout = test.timeout
			addr := startSMTP(t, testBackend(t, ntfy.URL, client))
			if err := stdsmtp.SendMail(addr, nil, "sender@example.net", []string{"rogee@ipao.vip"}, []byte(testMessage)); err != nil {
				t.Fatalf("SMTP returned ntfy failure: %v", err)
			}
		})
	}
}

func TestNtfyRedirectDoesNotLeakCode(t *testing.T) {
	var leaked atomic.Bool
	leakTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		leaked.Store(true)
	}))
	defer leakTarget.Close()
	ntfy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", leakTarget.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer ntfy.Close()

	addr := startSMTP(t, testBackend(t, ntfy.URL, ntfy.Client()))
	if err := stdsmtp.SendMail(addr, nil, "sender@example.net", []string{"rogee@ipao.vip"}, []byte(testMessage)); err != nil {
		t.Fatalf("SMTP returned redirect failure: %v", err)
	}
	if leaked.Load() {
		t.Fatal("verification code was sent to redirect target")
	}
}

func TestExtractCodeFindsFirstIndependentCode(t *testing.T) {
	message := "Content-Type: multipart/alternative; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nignore 1234567, use 654321\r\n" +
		"--x--\r\n"
	code, err := extractCode(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if code != "654321" {
		t.Fatalf("code = %q", code)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("ALLOWED_DOMAIN", "IPAO.VIP")
	t.Setenv("NTFY_BASE_URL", "https://ntfy.example.com/base")
	t.Setenv("NTFY_USERNAME", "user")
	t.Setenv("NTFY_PASSWORD", "pass")
	t.Setenv("HTTP_TIMEOUT", "2s")
	t.Setenv("TOPIC_TMPL", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.allowedDomain != "ipao.vip" || cfg.topicTmpl != "multica-{recipient}" || cfg.timeout != 2*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	for _, tmpl := range []string{strings.Repeat("a", 64), "multica-{sender}-{recipient}"} {
		t.Run(tmpl, func(t *testing.T) {
			t.Setenv("TOPIC_TMPL", tmpl)
			cfg, err := loadConfig()
			if err != nil || cfg.topicTmpl != tmpl {
				t.Fatalf("TOPIC_TMPL = %q, error = %v, want %q", cfg.topicTmpl, err, tmpl)
			}
		})
	}
	for _, tmpl := range []string{"multica-{unknown}", "{sender", "bad/path", "MixedCase", strings.Repeat("a", 65)} {
		t.Run(tmpl, func(t *testing.T) {
			t.Setenv("TOPIC_TMPL", tmpl)
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "TOPIC_TMPL") {
				t.Fatalf("error = %v, want a TOPIC_TMPL error", err)
			}
		})
	}

	t.Setenv("NTFY_BASE_URL", "http://ntfy.example.com")
	if _, err := loadConfig(); err == nil {
		t.Fatal("insecure NTFY_BASE_URL was accepted")
	}

	t.Setenv("NTFY_BASE_URL", "https://ntfy.example.com")
	t.Setenv("HTTP_TIMEOUT", "0s")
	if _, err := loadConfig(); err == nil {
		t.Fatal("zero HTTP_TIMEOUT was accepted")
	}

	t.Setenv("HTTP_TIMEOUT", "2s")
	t.Setenv("ALLOWED_DOMAIN", "bad/domain")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid ALLOWED_DOMAIN was accepted")
	}
}

func testBackend(t *testing.T, rawURL string, client *http.Client) *backend {
	t.Helper()
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return &backend{
		domain:    "ipao.vip",
		topicTmpl: "multica-{recipient}",
		baseURL:   baseURL,
		username:  "ntfy-user",
		password:  "ntfy-pass",
		client:    client,
		logger:    log.New(io.Discard, "", 0),
	}
}

func startSMTP(t *testing.T, backend *backend) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := gosmtp.NewServer(backend)
	server.Domain = "smtp2ntfy.test"
	server.MaxRecipients = 1
	server.MaxMessageBytes = maxMessageBytes
	server.ReadTimeout = time.Second
	server.WriteTimeout = time.Second
	done := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return listener.Addr().String()
}

func command(t *testing.T, conn *textproto.Conn, code int, line string) {
	t.Helper()
	id, err := conn.Cmd("%s", line)
	if err != nil {
		t.Fatal(err)
	}
	conn.StartResponse(id)
	defer conn.EndResponse(id)
	if _, _, err := conn.ReadResponse(code); err != nil {
		t.Fatal(err)
	}
}
