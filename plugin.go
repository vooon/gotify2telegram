package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"time"

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
	GotifyURL   string `json:"gotify_url" env:"GOTIFY_HOST" yaml:"gotify_url"`
	ClientToken string `json:"client_token" env:"GOTIFY_CLIENT_TOKEN" yaml:"client_token"`
	ChatID      int64  `json:"chat_id" env:"TELEGRAM_CHAT_ID" yaml:"chat_id"`
	BotToken    string `json:"bot_token" env:"TELEGRAM_BOT_TOKEN" yaml:"bot_token"`
	ParseMode   string `json:"parse_mode" env:"TELEGRAM_PARSE_MODE" yaml:"parse_mode"`
	WrapAsCode  bool   `json:"wrap_as_code" yaml:"wrap_as_code"`
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
	ID       uint32    `json:"id"`
	AppID    uint32    `json:"appid"`
	Message  string    `json:"message"`
	Title    string    `json:"title"`
	Priority uint32    `json:"priority"`
	Date     time.Time `json:"date"`
}

func (p *TelegramPlugin) forwardMessage(ctx context.Context, msg *GotifyMessage) {
	// message length limited to 4k
	const stepSize = 4090

	// TODO: templating?
	tmsg := fmt.Sprintf("Date: %s\nTitle: %s\n\n%s", msg.Date.Format(time.RFC822), msg.Title, msg.Message)
	msgLen := len(tmsg)

	// p.lg.Debug("forwarding...", "msg", msg)

	for i := 0; i < msgLen; i += stepSize {
		pmsg := func() string {
			if i+stepSize < msgLen {
				return tmsg[i : i+stepSize]
			}

			return tmsg[i:msgLen]
		}()

		mp := &botapi.SendMessageParams{
			ChatID:    p.config.ChatID,
			Text:      pmsg,
			ParseMode: models.ParseMode(p.config.ParseMode),
		}
		if p.config.WrapAsCode {
			mp.Entities = []models.MessageEntity{
				{Type: models.MessageEntityTypePre, Offset: 0, Length: len(pmsg)},
			}
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
	var ws *websocket.Conn

	for {
		u, err := url.Parse(p.config.GotifyURL)
		if err != nil {
			panic(err)
		}

		u = u.JoinPath("./stream")
		q := u.Query()
		q.Add("token", p.config.ClientToken)
		u.RawQuery = q.Encode()

		p.lg.InfoContext(ctx, "Dialing message stream", "url", u.String())

		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		ws, _, err = websocket.DefaultDialer.DialContext(ctx2, u.String(), nil)
		if err == nil {
			break
		}

		p.lg.ErrorContext(ctx, "Failed to connect to message stream. Retrying...", "error", err)
		time.Sleep(5 * time.Second)
	}

	p.lg.InfoContext(ctx, "Connected to message stream")

	go func() {
		for {
			select {
			case <-p.ctx.Done():
				p.lg.WarnContext(p.ctx, "Terminating message reader")
				_ = ws.Close()
				close(msgC)
				return

			default:
				msg := GotifyMessage{}

				err := ws.ReadJSON(&msg)
				if err != nil {
					p.lg.ErrorContext(p.ctx, "Failed to read message. Reconnecting...", "error", err)
					go p.connect(p.ctx, msgC)
					return
				}

				// p.lg.Debug("Got msg", "msg", msg)
				msgC <- msg
			}
		}
	}()
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
