# Discord Server Count Monitor Bot

指定したDiscord botのサーバー数を毎日定時に確認し、Discord上で通知するGoで書かれたbotです。

## 機能

- 指定したDiscord botのサーバー数を取得
- 毎日指定した時刻に自動通知
- 複数のAPIソースからの取得に対応（top.gg、Discord Bot List）
- エラー時の通知機能
- **[NEW]** API使用回数の計測機能
- **[NEW]** Framer Webhook転送機能
- **[NEW]** Jicoo Webhook → Discord通知機能

## セットアップ

### 1. 依存関係のインストール

```bash
go mod download
```

### 2. 環境変数の設定

`.env.example`を`.env`にコピーして、必要な情報を入力します：

```bash
cp .env.example .env
```

以下の環境変数を設定してください：

- `DISCORD_TOKEN`: 監視用botのトークン（必須）
- `CHANNEL_ID`: 通知を送信するチャンネルのID（必須）
- `TARGET_BOT_IDS`: 監視対象のbotのID（必須、カンマ区切りで複数指定可能）
- `TOPGG_TOKEN`: top.gg APIトークン（オプション）
- `BOT_TOKENS`: bot未登録サイトのボット用トークン（オプション、形式: BOT_ID:TOKEN）
- `CUSTOM_WEBHOOKS`: カスタム統計エンドポイント（オプション、形式: BOT_ID:URL）
- `NOTIFICATION_TIME`: 通知時刻（デフォルト: 09:00）
- `PORT`: HTTPサーバーのポート（デフォルト: 8080）
- `DISCORD_WEBHOOK_URL`: Jicoo通知用のDiscord Incoming Webhook URL（推奨）
- `JICOO_WEBHOOK_SECRET`: Jicoo Webhook署名シークレット（推奨）

### 3. Discord Botの作成

1. [Discord Developer Portal](https://discord.com/developers/applications)にアクセス
2. "New Application"をクリックしてアプリケーションを作成
3. "Bot"セクションに移動し、botを作成
4. トークンをコピーして`.env`の`DISCORD_TOKEN`に設定
5. botを通知したいサーバーに招待

### 4. top.gg APIトークンの取得（推奨）

正確なサーバー数を取得するために：

1. [top.gg](https://top.gg)にアクセス
2. 監視対象のbotページに移動
3. "Webhooks"セクションでAPIトークンを取得
4. `.env`の`TOPGG_TOKEN`に設定

## 実行

```bash
go run main.go
```

または、ビルドしてから実行：

```bash
go build -o statbot
./statbot
```

## Docker対応（オプション）

Dockerfileを作成して実行することも可能です：

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o statbot

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/statbot .
CMD ["./statbot"]
```

## 通知時刻の設定

`NOTIFICATION_TIME`は以下の形式で設定できます：

- `HH:MM`形式（例: `09:00`、`15:30`）
- Cron形式（例: `0 9 * * *`で毎日9時0分）

## Bot Listに登録されていないbotの監視方法

### 方法1: Botトークンを使用（最も正確）

監視したいbotのトークンがある場合、直接Discord APIを使用してサーバー数を取得できます：

```bash
# .envファイルに追加
BOT_TOKENS=123456789012345678:MTA2NzQ...,987654321098765432:MTI3ODk...
```

**shardされたbotについて：**
- Gateway接続を使用して正確なサーバー数を取得
- REST APIとGateway APIの両方を試行し、より大きい値を採用
- shardされたbotでも正確な総サーバー数を取得可能

### 方法2: カスタムWebhookエンドポイント

botが独自の統計APIを提供している場合：

```bash
# .envファイルに追加
CUSTOM_WEBHOOKS=123456789012345678:https://api.mybot.com/stats
```

Webhookは以下のようなJSONを返す必要があります：
```json
{
  "server_count": 1234  // または "guilds", "serverCount" など
}
```

### 方法3: 相互サーバーのみ（制限あり）

上記の方法が使用できない場合、監視botと同じサーバーにいるbotのみカウントされます（完全な数値ではありません）。

## トラブルシューティング

### サーバー数が取得できない場合

1. Bot Listに登録されていないbotの場合：
   - `BOT_TOKENS`にbotのトークンを設定
   - または`CUSTOM_WEBHOOKS`にカスタムエンドポイントを設定
2. `TOPGG_TOKEN`が正しく設定されているか確認
3. APIトークンの権限を確認

### 通知が送信されない場合

1. `CHANNEL_ID`が正しいか確認
2. botがそのチャンネルへのメッセージ送信権限を持っているか確認
3. ログでエラーメッセージを確認

## 追加機能 API & Webhooks

本botはHTTPサーバーとしても動作します。

### 1. API使用回数計測 (`/api/count`)

指定したラベルのAPI呼び出し回数を計測し、毎日09:00にDiscord Webhookへ通知します。

- **URL**: `/api/count?label=YOUR_LABEL`
- **Method**: GET
- **仕様**:
  - ラベルごとにカウント
  - 毎日00:00にリセット
  - 毎日09:00に`CHANNEL_ID`へ集計結果を送信

### 2. Framer Webhook転送 (`/api/framer`)

Framerのフォーム送信を受け取り、Botがメッセージとして転送します。Framerの仕様（即時2xx応答必須）に対応しています。

- **URL**: `/api/framer`
- **Method**: POST
- **仕様**:
  - リクエスト受信後、即座にHTTP 200 OKを返却（再試行防止）
  - 受け取ったJSONボディを整形して`CHANNEL_ID`へ非同期で送信

### 3. Jicoo Webhook転送 (`/api/jicoo`)

[Jicoo Webhook](https://developer.jicoo.com/doc-355758) を受け取り、Discord Incoming Webhookへ通知します。

- **URL**: `/api/jicoo`
- **Method**: POST
- **仕様**:
  - 受信後にHTTP 200を返却（Jicooの再送条件に対応）
  - `JICOO_WEBHOOK_SECRET` 設定時は `Jicoo-Webhook-Signature` をHMAC-SHA256で検証
  - 予約作成・変更・キャンセル等のイベントを埋め込みメッセージに整形
  - `DISCORD_WEBHOOK_URL` へ送信（未設定時は `CHANNEL_ID` へBot送信）

#### Jicoo側の設定手順

1. [Jicoo Webhook設定](https://www.jicoo.com/dashboard/webhooks) でエンドポイントを作成
2. URLに `https://YOUR_HOST/api/jicoo` を指定
3. 表示される署名シークレットを `JICOO_WEBHOOK_SECRET` に設定
4. Discordチャンネルの Integrations → Webhooks で作成したURLを `DISCORD_WEBHOOK_URL` に設定

## ライセンス

MIT