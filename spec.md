# 追加集計機能

APIを叩かれた回数を記録して、毎朝9時にDiscordのWebhookに送信する

機能1
APIを叩かれた回数を記録する
0時（TIMEZONE、既定 Asia/Tokyo）に集計を締め切り、前日分として保持したうえでカウンターをリセットする
apiにはラベルをつけ、ラベルごとにカウントを取る



機能2
毎朝9時に前日の分をDiscordのWebhookに送信する

📊 カウンター (YYYY-MM-DD)
ラベル1 : 0
ラベル2 : 0
ラベル3 : 0 ...

のように続ける（ラベルはアルファベット順）
記録のなかったラベルは飛ばす
ラベルが1つもなければ「記録なし」と送る

機能3
framerのフォームのwebhookを受け取って、受け次第その内容をDiscordのwebhookに送信する

機能4
Jicooのwebhookを受け取って、受け次第その内容をDiscordのwebhookに送信する
- エンドポイント: POST /api/jicoo
- Jicoo-Webhook-Signature を検証（JICOO_WEBHOOK_SECRET設定時）
- DISCORD_WEBHOOK_URL へ整形した通知を送信

