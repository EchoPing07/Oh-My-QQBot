package qq

import (
	"context"
	"fmt"

	"github.com/ohmyqqbot/oh-my-qqbot/internal/domain"
)

// SendText 实现 agent.Sender 接口。
func (a *Adapter) SendText(ctx context.Context, scene, target string, text string, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	if text == "" {
		return domain.SendResult{}, nil
	}
	return a.SendSegments(ctx, scene, target, []domain.Segment{domain.Text(text)}, passiveTo, quote)
}

// SendSegments 发送一段由消息段组成的内容。
//
// 发送层自己会按「一条消息只能一种内容」拆条（见 Sender.splitOutbound），
// 所以这里把段原样交给它就好。
func (a *Adapter) SendSegments(ctx context.Context, scene, target string, segments []domain.Segment, passiveTo string, quote domain.Reference) (domain.SendResult, error) {
	if len(segments) == 0 {
		return domain.SendResult{}, nil
	}
	return a.sender.Send(ctx, scene, target, segments, passiveTo, quote)
}

// SendImage 发送一张图片。
func (a *Adapter) SendImage(ctx context.Context, scene, target string, binary []byte, passiveTo string) ([]string, error) {
	res, err := a.sender.Send(ctx, scene, target, []domain.Segment{
		{Type: domain.SegmentImage, Binary: binary},
	}, passiveTo, domain.Reference{})
	if err != nil {
		return nil, err
	}
	return res.MessageIDs, nil
}

// Mute 禁言群成员。
func (a *Adapter) Mute(ctx context.Context, groupOpenID, memberOpenID string, seconds int) error {
	if seconds <= 0 {
		return fmt.Errorf("禁言时长必须为正整数")
	}
	return a.sender.MuteUser(ctx, groupOpenID, memberOpenID, secondsDuration(seconds))
}

// Recall 撤回消息。
func (a *Adapter) Recall(ctx context.Context, scene, target, messageID string) error {
	return a.sender.RecallMessage(ctx, scene, target, messageID)
}
