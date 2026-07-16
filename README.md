# Gotify 2 Telegram
This Gotify plugin forwards all received messages to Telegram through the Telegram bot.

## Prerequisite
- A Telegram bot, bot token, and chat ID from bot conversation. You can get that information by following this [blog](https://medium.com/linux-shots/setup-telegram-bot-to-get-alert-notifications-90be7da4444).
- Golang, Docker, wget (If you want to build the binary from source).

## Installation
* **By shared object**

    1. Get the compatible shared object from [release](https://github.com/anhbh310/gotify2telegram/releases).

    2. Put it into Gotify plugin folder.

    3. Open the plugin settings in the Gotify UI and configure the plugin fields.

    4. Restart gotify.

* **Build from source**

    1. `Makefile` defaults to `GOTIFY_VERSION=v2.9.1`. Change it only if you need another server version.

    2. Build the binary.

    ```
    make build
    ```

    3. Follow instructions from step 2 in the shared object installation.

## Troubleshooting
1. When only the Gotify dashboard receives your message, but not Telegram:

    If, when making the API call to get your bot's chat ID, no data is returned, you may need to change the bot's privacy settings.

    - In the BotFather chat, list your created bots and select the respective bot for which you want to change the Group Privacy setting.
    - Turn off the Group Privacy setting.

## Configuration
This plugin can be configured from the Gotify UI (recommended).

Required fields in UI:
- `gotify_url`
- `client_token`
- `chat_id`
- `bot_token`

Optional fields in UI:
- `parse_mode`
- `wrap_as_code`
- `disable_notification_priority`

## Appendix
Environment variables are supported as defaults (or for fully env-driven deployments).

```(shell)
GOTIFY_HOST=ws://YOUR_GOTIFY_IP (depending on your setup, "ws://localhost:80" will likely work by default)
GOTIFY_CLIENT_TOKEN=YOUR_CLIENT_TOKEN (create a new Client in Gotify and use the Token from there, or you can use an existing client)
TELEGRAM_CHAT_ID=YOUR_TELEGRAM_CHAT_ID (conversation ID from the Telegram API call above)
TELEGRAM_BOT_TOKEN=YOUR_TELEGRAM_BOT_TOKEN (API token provided by BotFather)
```
