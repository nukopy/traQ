package model

import (
	"fmt"
	"time"

	"github.com/satori/go.uuid"
)

// UserSubscribeChannel ユーザー・通知チャンネル対構造体
type UserSubscribeChannel struct {
	UserID    string    `xorm:"char(36) pk not null"`
	ChannelID string    `xorm:"char(36) pk not null"`
	CreatedAt time.Time `xorm:"created not null"`
}

// TableName UserNotifiedChannel構造体のテーブル名
func (*UserSubscribeChannel) TableName() string {
	return "users_subscribe_channels"
}

// Create DBに登録
func (s *UserSubscribeChannel) Create() error {
	if s.UserID == "" {
		return fmt.Errorf("UserID is empty")
	}
	if s.ChannelID == "" {
		return fmt.Errorf("ChannelID is empty")
	}

	if _, err := db.Insert(s); err != nil {
		return fmt.Errorf("failed to create user_notified_channel: %v", err)
	}

	return nil
}

// Delete DBから削除
func (s *UserSubscribeChannel) Delete() error {
	if s.UserID == "" {
		return fmt.Errorf("UserID is empty")
	}
	if s.ChannelID == "" {
		return fmt.Errorf("ChannelID is empty")
	}

	if _, err := db.Delete(s); err != nil {
		return fmt.Errorf("failed to delete user_notified_channel: %v", err)
	}

	return nil
}

// GetSubscribingUser 指定したチャンネルの通知をつけているユーザーを取得
func GetSubscribingUser(channelID uuid.UUID) ([]uuid.UUID, error) {
	var arr []string
	if err := db.Table(&UserSubscribeChannel{}).Where("channel_id = ?", channelID.String()).Cols("user_id").Find(&arr); err != nil {
		return nil, fmt.Errorf("failed to get user_subscribe_channel: %v", err)
	}

	result := make([]uuid.UUID, len(arr))
	for i, v := range arr {
		result[i] = uuid.FromStringOrNil(v)
	}

	return result, nil
}