package main

import (
	"context"
	"fmt"
	"time"
)

/*
每日签到：按账号拉取活动列表，领取可领的积分奖励。

结果码：
 0 未处理 / 1 已领取 / 2 今日已签 / 3 无可领活动 / 4 失败
*/

const (
	checkinPending = 0
	checkinClaimed = 1
	checkinAlready = 2
	checkinNone    = 3
	checkinFailed  = 4
)

// checkinAccount 对单个账号执行一次签到。
func (a *App) checkinAccount(ctx context.Context, acc Account) (int, float64, string) {
	ed := editionOf(acc.Edition)
	campaigns, err := ListCampaigns(ctx, ed, acc)
	if err != nil {
		return checkinFailed, 0, err.Error()
	}
	if TodayCheckedIn(campaigns) {
		return checkinAlready, 0, "今日已签到"
	}
	claimable := FindClaimableCheckins(campaigns, time.Now())
	if len(claimable) == 0 {
		return checkinNone, 0, "没有可领取的签到奖励"
	}
	var total float64
	var last string
	for _, c := range claimable {
		res, err := ClaimCampaign(ctx, ed, acc, c.ID)
		if err != nil {
			last = err.Error()
			continue
		}
		if res.Replayed {
			last = "今日已签到"
			continue
		}
		total += res.RewardCredit
		if res.RewardCredit > 0 {
			last = fmt.Sprintf("%s +%.0f", c.Name, res.RewardCredit)
		} else {
			last = c.Name
		}
	}
	if total > 0 {
		return checkinClaimed, total, last
	}
	if last == "今日已签到" {
		return checkinAlready, 0, last
	}
	if last == "" {
		last = "没有可领取的签到奖励"
		return checkinNone, 0, last
	}
	return checkinFailed, 0, last
}

// runCheckin 遍历所有账号签到，并把结果写入状态与日志。
func (a *App) runCheckin(ctx context.Context, onlyID string) {
	accounts := []Account{}
	for _, edition := range editionList {
		for _, acc := range a.store.Accounts(edition) {
			if onlyID != "" && acc.ID != onlyID {
				continue
			}
			if acc.CheckinOff {
				continue
			}
			accounts = append(accounts, acc)
		}
	}
	day := localDayKey(time.Now())
	for _, acc := range accounts {
		if onlyID == "" && acc.CheckinDay == day && acc.CheckinResult == checkinClaimed {
			continue
		}
		result, credits, message := a.checkinAccount(ctx, acc)
		retryAt := int64(0)
		if result == checkinFailed {
			// 失败退避 15 分钟，避免风控期反复打点。
			retryAt = time.Now().Add(15 * time.Minute).UnixMilli()
		}
		a.store.SetCheckinState(acc.ID, day, result, retryAt)
		a.store.AppendCheckin(CheckinLogEntry{
			Time:    time.Now().UnixMilli(),
			Account: accountDisplayName(acc),
			UID:     acc.UID,
			Edition: acc.Edition,
			Success: result == checkinClaimed || result == checkinAlready,
			Credits: credits,
			Message: message,
		})
		logf("签到 %s（%s）: %s", accountDisplayName(acc), acc.Edition, message)
	}
}

// startCheckinScheduler 每天在配置的时间点跑一次签到。
func (a *App) startCheckinScheduler(ctx context.Context) {
	if !a.cfg.CheckinEnabled {
		return
	}
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), a.cfg.CheckinHour, a.cfg.CheckinMinute, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			a.runCheckin(ctx, "")
			// 错开一点，避免整点并发。
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
}
