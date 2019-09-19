package main

import (
	"context"
	"firebase.google.com/go"
	"firebase.google.com/go/messaging"
	"fmt"
	"github.com/cenkalti/backoff"
	"github.com/gofrs/uuid"
	"github.com/leandro-lugaresi/hub"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/traPtitech/traQ/event"
	"github.com/traPtitech/traQ/model"
	"github.com/traPtitech/traQ/repository"
	"github.com/traPtitech/traQ/utils/message"
	"github.com/traPtitech/traQ/utils/set"
	"go.uber.org/zap"
	"golang.org/x/exp/utf8string"
	"google.golang.org/api/option"
	"strconv"
	"strings"
	"time"
)

const messageTTLSeconds = 60 * 60 * 24 * 2 // 2日
var messageTTL = messageTTLSeconds * time.Second

var fcmSendCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "firebase",
	Name:      "fcm_send_count_total",
}, []string{"result"})

// FCMManager Firebaseマネージャー構造体
type FCMManager struct {
	messaging *messaging.Client
	repo      repository.Repository
	hub       *hub.Hub
	logger    *zap.Logger
	origin    string
}

// NewFCMManager FCMManagerを生成します
func NewFCMManager(repo repository.Repository, hub *hub.Hub, logger *zap.Logger, serviceAccountFile, origin string) (*FCMManager, error) {
	manager := &FCMManager{
		repo:   repo,
		hub:    hub,
		logger: logger,
		origin: origin,
	}

	app, err := firebase.NewApp(context.Background(), nil, option.WithCredentialsFile(serviceAccountFile))
	if err != nil {
		return nil, err
	}

	manager.messaging, err = app.Messaging(context.Background())
	if err != nil {
		return nil, err
	}

	go func() {
		sub := hub.Subscribe(100, event.MessageCreated)
		for ev := range sub.Receiver {
			m := ev.Fields["message"].(*model.Message)
			p := ev.Fields["plain"].(string)
			e := ev.Fields["embedded"].([]*message.EmbeddedInfo)
			go manager.processMessageCreated(m, p, e)
		}
	}()
	return manager, nil
}

func (m *FCMManager) processMessageCreated(message *model.Message, plain string, embedded []*message.EmbeddedInfo) {
	logger := m.logger.With(zap.Stringer("messageId", message.ID))

	// チャンネル情報を取得
	ch, err := m.repo.GetChannel(message.ChannelID)
	if err != nil {
		logger.Error("failed to GetChannel", zap.Error(err), zap.Stringer("channelId", message.ChannelID)) // 失敗
		return
	}

	// 投稿ユーザー情報を取得
	mUser, err := m.repo.GetUser(message.UserID)
	if err != nil {
		logger.Error("failed to GetUser", zap.Error(err), zap.Stringer("userId", message.UserID)) // 失敗
		return
	}
	if len(mUser.DisplayName) == 0 {
		mUser.DisplayName = mUser.Name
	}

	// データ初期化
	data := map[string]string{
		"title":     "traQ",
		"icon":      fmt.Sprintf("%s/api/1.0/public/icon/%s", m.origin, strings.ReplaceAll(mUser.Name, "#", "%23")),
		"vibration": "[1000, 1000, 1000]",
		"tag":       fmt.Sprintf("c:%s", message.ChannelID),
		"badge":     fmt.Sprintf("%s/static/badge.png", m.origin),
	}

	// メッセージボディ作成
	body := ""
	if ch.IsDMChannel() {
		data["title"] = "@" + mUser.DisplayName
		data["path"] = "/users/" + mUser.Name
		body = plain
	} else {
		path, err := m.repo.GetChannelPath(message.ChannelID)
		if err != nil {
			logger.Error("failed to GetChannelPath", zap.Error(err), zap.Stringer("channelId", message.ChannelID))
			return
		}

		data["title"] = "#" + path
		data["path"] = "/channels/" + path
		body = fmt.Sprintf("%s: %s", mUser.DisplayName, plain)
	}

	if s := utf8string.NewString(body); s.RuneCount() > 100 {
		body = s.Slice(0, 97) + "..."
	}
	data["body"] = body

	for _, v := range embedded {
		if v.Type == "file" {
			if f, _ := m.repo.GetFileMeta(uuid.FromStringOrNil(v.ID)); f != nil && f.HasThumbnail {
				data["image"] = fmt.Sprintf("%s/api/1.0/files/%s/thumbnail", m.origin, v.ID)
				break
			}
		}
	}

	// 対象者計算
	targets := set.UUIDSet{}
	q := repository.UsersQuery{}.Active().NotBot()
	switch {
	case ch.IsForced: // 強制通知チャンネル
		users, err := m.repo.GetUserIDs(q)
		if err != nil {
			logger.Error("failed to GetUsers", zap.Error(err)) // 失敗
			return
		}
		targets.Add(users...)

	case !ch.IsPublic: // プライベートチャンネル
		pUsers, err := m.repo.GetUserIDs(q.CMemberOf(ch.ID))
		if err != nil {
			logger.Error("failed to GetPrivateChannelMemberIDs", zap.Error(err), zap.Stringer("channelId", message.ChannelID)) // 失敗
			return
		}
		targets.Add(pUsers...)

	default: // 通常チャンネルメッセージ
		users, err := m.repo.GetUserIDs(q.SubscriberOf(ch.ID))
		if err != nil {
			logger.Error("failed to GetSubscribingUserIDs", zap.Error(err), zap.Stringer("channelId", message.ChannelID)) // 失敗
			return
		}
		targets.Add(users...)

		// ユーザーグループ・メンションユーザー取得
		for _, v := range embedded {
			switch v.Type {
			case "user":
				if uid, err := uuid.FromString(v.ID); err == nil {
					// TODO 凍結ユーザーの除外
					// MEMO 凍結ユーザーはクライアント側で置換されないのでこのままでも問題はない
					targets.Add(uid)
				}
			case "group":
				gs, err := m.repo.GetUserIDs(q.GMemberOf(uuid.FromStringOrNil(v.ID)))
				if err != nil {
					logger.Error("failed to GetUserGroupMemberIDs", zap.Error(err), zap.String("groupId", v.ID)) // 失敗
					return
				}
				targets.Add(gs...)
			}
		}

		// ミュート除外
		muted, err := m.repo.GetMuteUserIDs(message.ChannelID)
		if err != nil {
			logger.Error("failed to GetMuteUserIDs", zap.Error(err), zap.Stringer("channelId", message.ChannelID)) // 失敗
			return
		}
		targets.Remove(muted...)
	}
	targets.Remove(message.UserID) // 自分を除外

	// 送信
	for u := range targets {
		go func(u uuid.UUID) {
			devs, err := m.repo.GetDeviceTokensByUserID(u)
			if err != nil {
				logger.Error("failed to GetDeviceTokensByUserID", zap.Error(err), zap.Stringer("userId", u)) // 失敗
				return
			}

			payload := &messaging.Message{
				Data: data,
				Android: &messaging.AndroidConfig{
					Priority: "high",
					TTL:      &messageTTL,
				},
				APNS: &messaging.APNSConfig{
					Headers: map[string]string{
						"apns-expiration": strconv.FormatInt(time.Now().Add(messageTTL).Unix(), 10),
					},
					Payload: &messaging.APNSPayload{
						Aps: &messaging.Aps{
							Alert: &messaging.ApsAlert{
								Title: data["title"],
								Body:  data["body"],
							},
							Sound:    "default",
							ThreadID: data["tag"],
						},
					},
				},
				Webpush: &messaging.WebpushConfig{
					Headers: map[string]string{
						"TTL": strconv.Itoa(messageTTLSeconds),
					},
				},
			}
			for _, token := range devs {
				payload.Token = token
				err := backoff.Retry(func() error {
					if _, err := m.messaging.Send(context.Background(), payload); err != nil {
						fcmSendCounter.WithLabelValues("error").Inc()
						switch {
						case messaging.IsRegistrationTokenNotRegistered(err):
							if err := m.repo.UnregisterDevice(token); err != nil {
								return backoff.Permanent(err)
							}
						case messaging.IsInvalidArgument(err):
							return backoff.Permanent(err)
						case messaging.IsServerUnavailable(err):
							fallthrough
						case messaging.IsInternal(err):
							fallthrough
						case messaging.IsMessageRateExceeded(err):
							fallthrough
						case messaging.IsUnknown(err):
							return err
						default:
							return err
						}
					}
					fcmSendCounter.WithLabelValues("ok").Inc()
					return nil
				}, backoff.NewExponentialBackOff())
				if err != nil {
					logger.Error("an error occurred in sending fcm", zap.Error(err), zap.String("deviceToken", token))
				}
			}
		}(u)
	}
}
