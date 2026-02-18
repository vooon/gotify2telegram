package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/caarlos0/env/v11"
	botapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/gorilla/websocket"
	"github.com/gotify/plugin-api"
)

// GetGotifyPluginInfo returns gotify plugin info
func GetGotifyPluginInfo() plugin.Info {
	return plugin.Info{
		Version:     "1.0",
		Author:      "Anh Bui",
		Name:        "Gotify 2 Telegram",
		Description: "Telegram message fowarder for gotify",
		ModulePath:  "https://github.com/vooon/gotify2telegram",
		License:     "MIT",
	}
}

type TelegramConfig struct {
	GotifyURL                   string `json:"gotify_url" env:"GOTIFY_HOST" yaml:"gotify_url"`
	ClientToken                 string `json:"client_token" env:"GOTIFY_CLIENT_TOKEN" yaml:"client_token"`
	ChatID                      int64  `json:"chat_id" env:"TELEGRAM_CHAT_ID" yaml:"chat_id"`
	BotToken                    string `json:"bot_token" env:"TELEGRAM_BOT_TOKEN" yaml:"bot_token"`
	ParseMode                   string `json:"parse_mode" env:"TELEGRAM_PARSE_MODE" yaml:"parse_mode"`
	WrapAsCode                  bool   `json:"wrap_as_code" yaml:"wrap_as_code"`
	DisableNotificationPriority int    `json:"disable_notification_priority" yaml:"disable_notification_priority"`
}

// Plugin is the plugin instance
type TelegramPlugin struct {
	msgHandler plugin.MessageHandler
	ctx        context.Context
	cancel     context.CancelFunc
	lg         *slog.Logger
	config     *TelegramConfig
	bot        *botapi.Bot
}

type GotifyMessage struct {
	ID       uint32         `json:"id"`
	AppID    uint32         `json:"appid"`
	Message  string         `json:"message"`
	Title    string         `json:"title"`
	Priority uint32         `json:"priority"`
	Date     time.Time      `json:"date"`
	Extras   map[string]any `json:"extras"`
}

func splitMessage(text string, limit int) []string {
	if limit <= 0 || text == "" {
		return nil
	}

	runes := []rune(text)
	parts := make([]string, 0, len(runes)/limit+1)

	for start := 0; start < len(runes); {
		end := start + limit
		if end >= len(runes) {
			parts = append(parts, string(runes[start:]))
			break
		}

		split := end
		for i := end - 1; i > start; i-- {
			if runes[i] == '\n' {
				split = i + 1
				break
			}
		}
		if split == end {
			for i := end - 1; i > start; i-- {
				if unicode.IsSpace(runes[i]) {
					split = i + 1
					break
				}
			}
		}
		if split <= start {
			split = end
		}

		parts = append(parts, string(runes[start:split]))
		start = split
	}

	return parts
}

func splitMarkdownMessage(text string, limit int) []string {
	const fence = "```"
	const fencePadding = len("```\n\n```")

	if limit <= fencePadding {
		return splitMessage(text, limit)
	}

	// Keep room for temporary fence close/open when chunk boundaries fall inside a fenced block.
	rawParts := splitMessage(text, limit-fencePadding)
	parts := make([]string, 0, len(rawParts))
	inFence := false

	for _, raw := range rawParts {
		if raw == "" {
			continue
		}

		part := raw
		if inFence {
			part = fence + "\n" + part
		}

		nextInFence := inFence != (strings.Count(raw, fence)%2 == 1)
		if nextInFence {
			part += "\n" + fence
		}

		parts = append(parts, part)
		inFence = nextInFence
	}

	return parts
}

func splitForParseMode(text string, limit int, parseMode models.ParseMode) []string {
	switch parseMode {
	case models.ParseModeMarkdown, models.ParseModeMarkdownV1:
		return splitMarkdownMessage(text, limit)
	default:
		return splitMessage(text, limit)
	}
}

func telegramEntityTextLength(text string) int {
	return len(utf16.Encode([]rune(text)))
}

func parseModeFromContentType(contentType string) models.ParseMode {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "" {
		return ""
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}

	switch ct {
	case "text/html":
		return models.ParseModeHTML
	case "text/markdown", "text/x-markdown":
		return models.ParseModeMarkdown
	default:
		return ""
	}
}

func parseModeFromExtras(msg *GotifyMessage) models.ParseMode {
	if msg == nil || msg.Extras == nil {
		return ""
	}

	displayRaw, ok := msg.Extras["client::display"]
	if !ok {
		return ""
	}
	display, ok := displayRaw.(map[string]any)
	if !ok {
		return ""
	}
	contentType, ok := display["contentType"].(string)
	if !ok {
		return ""
	}

	return parseModeFromContentType(contentType)
}

func (p *TelegramPlugin) forwardMessage(ctx context.Context, msg *GotifyMessage) {
	// message length limited to 4k
	const stepSize = 4090

	parseMode := models.ParseMode(p.config.ParseMode)
	if parseMode == "" {
		parseMode = parseModeFromExtras(msg)
	}

	// TODO: templating?
	tmsg := fmt.Sprintf("Date: %s\nTitle: %s\n\n%s", msg.Date.Format(time.RFC822), msg.Title, msg.Message)
	parts := splitForParseMode(tmsg, stepSize, parseMode)

	for _, pmsg := range parts {
		if pmsg == "" {
			continue
		}

		mp := &botapi.SendMessageParams{
			ChatID:    p.config.ChatID,
			Text:      pmsg,
			ParseMode: parseMode,
		}
		if p.config.WrapAsCode {
			mp.Entities = []models.MessageEntity{
				{Type: models.MessageEntityTypePre, Offset: 0, Length: telegramEntityTextLength(pmsg)},
			}
		}
		if int(msg.Priority) <= p.config.DisableNotificationPriority {
			mp.DisableNotification = true
		}

		m, err := p.bot.SendMessage(ctx, mp)
		if err != nil {
			p.lg.ErrorContext(ctx, "Failed to send message", "msg_id", msg.ID, "error", err)
		} else {
			p.lg.InfoContext(ctx, "Message forwarded", "msg_id", msg.ID, "tmsg_id", m.ID)
		}
	}
}

func (p *TelegramPlugin) connect(ctx context.Context, msgC chan<- GotifyMessage) {
	defer close(msgC)

	for {
		if ctx.Err() != nil {
			return
		}

		u, err := url.Parse(p.config.GotifyURL)
		if err != nil {
			p.lg.ErrorContext(ctx, "Invalid Gotify URL", "url", p.config.GotifyURL, "error", err)
			return
		}

		u = u.JoinPath("./stream")
		q := u.Query()
		q.Add("token", p.config.ClientToken)
		u.RawQuery = q.Encode()

		p.lg.InfoContext(ctx, "Dialing message stream", "url", u.String())

		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		ws, _, err := websocket.DefaultDialer.DialContext(ctx2, u.String(), nil)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			p.lg.ErrorContext(ctx, "Failed to connect to message stream. Retrying...", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		p.lg.InfoContext(ctx, "Connected to message stream")

		connClosed := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = ws.Close()
			case <-connClosed:
			}
		}()

		for {
			msg := GotifyMessage{}

			err := ws.ReadJSON(&msg)
			if err != nil {
				close(connClosed)
				_ = ws.Close()
				if ctx.Err() != nil {
					p.lg.WarnContext(ctx, "Terminating message reader")
					return
				}
				p.lg.ErrorContext(ctx, "Failed to read message. Reconnecting...", "error", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				break
			}

			select {
			case <-ctx.Done():
				close(connClosed)
				_ = ws.Close()
				p.lg.WarnContext(ctx, "Terminating message reader")
				return
			// p.lg.Debug("Got msg", "msg", msg)
			case msgC <- msg:
			}
		}
	}
}

func (p *TelegramPlugin) startForwarder() {
	p.lg.Info("Starting message forwarder", "url", p.config.GotifyURL, "chat_id", p.config.ChatID)

	msgC := make(chan GotifyMessage, 100)
	go p.connect(p.ctx, msgC)

	for msg := range msgC {
		p.forwardMessage(p.ctx, &msg)
	}

	p.lg.Warn("Forwarder terminated")
}

func (p *TelegramPlugin) SetMessageHandler(h plugin.MessageHandler) {
	p.msgHandler = h
}

func (p *TelegramPlugin) DefaultConfig() any {
	return p.config
}

func (p *TelegramPlugin) ValidateAndSetConfig(c any) error {
	cm, ok := c.(*TelegramConfig)
	if !ok {
		return fmt.Errorf("unexpected config type: %T", c)
	}

	var err error
	if cm.GotifyURL == "" {
		err = errors.Join(err, fmt.Errorf("Gotify URL must be set")) // nolint:staticcheck
	}
	if cm.ClientToken == "" {
		err = errors.Join(err, fmt.Errorf("Gotify Client Token must be set")) // nolint:staticcheck
	}

	if cm.ChatID == 0 {
		err = errors.Join(err, fmt.Errorf("ChatID must be set")) // nolint:staticcheck
	}
	if cm.BotToken == "" {
		err = errors.Join(err, fmt.Errorf("Bot Token must be set")) // nolint:staticcheck
	}
	if !slices.Contains([]models.ParseMode{"", models.ParseModeHTML, models.ParseModeMarkdown, models.ParseModeMarkdownV1}, models.ParseMode(cm.ParseMode)) {
		err = errors.Join(err, fmt.Errorf("Unknown parse mode: %s", cm.ParseMode)) // nolint:staticcheck
	}

	if err != nil {
		p.lg.Warn("Config validation failed", "new_cfg", cm, "error", err)
		return err
	}

	p.config = cm
	return nil
}

func (p *TelegramPlugin) Enable() (err error) {
	cctx, cancel := context.WithCancel(context.Background())

	p.ctx = cctx
	p.cancel = cancel

	p.bot, err = botapi.New(p.config.BotToken)
	if err != nil {
		p.lg.Error("Failed to create telegram bot", "error", err)
		return
	}

	go p.startForwarder()

	return nil
}

func (p *TelegramPlugin) Disable() (err error) {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.bot != nil {
		_, err = p.bot.Close(context.Background())
	}

	return
}

type pluginInterface interface {
	plugin.Plugin
	plugin.Messenger
	plugin.Configurer
}

// NewGotifyPluginInstance creates a plugin instance for a user context.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {

	lg := slog.New(slog.NewTextHandler(os.Stdout, nil))
	lg = lg.With("plugin", "telegram", "user_id", ctx.ID, "user_name", ctx.Name)

	cfg, err := env.ParseAs[TelegramConfig]()
	if err != nil {
		panic(err)
	}

	// verify interface implementation
	var p pluginInterface = &TelegramPlugin{
		lg:     lg,
		config: &cfg,
	}

	return p
}
