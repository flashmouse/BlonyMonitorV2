package app

import (
	"log"
	"os"
	"path/filepath"
	"sync"
)

// 该文件仅用于定位“部分 Boss 持续时间虚长 / 结束时间被推迟”的问题，属于临时诊断。
// 定位完成后可整体删除，不影响业务逻辑。
//
// 为避免依赖 config.EnableFileLog（默认关闭，logger 会丢弃输出），这里单独写一个
// 始终开启的诊断文件：<用户数据目录>/boss_trace.log

const (
	bossDurationAuditMinSeconds = 300 // 超过该时长才进入审计（避免噪音）
	bossTraceFileName           = "boss_trace.log"
)

var (
	bossTraceOnce   sync.Once
	bossTraceLogger *log.Logger
)

// bossTrace 始终写入 <用户数据目录>/boss_trace.log，不受文件日志开关影响。
func bossTrace(format string, args ...interface{}) {
	bossTraceOnce.Do(func() {
		path := filepath.Join(ensureUserDataDirectory(), bossTraceFileName)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			bossTraceLogger = log.New(os.Stdout, "BossTrace ", log.LstdFlags)
			return
		}
		bossTraceLogger = log.New(f, "", log.LstdFlags)
	})
	bossTraceLogger.Printf(format, args...)
}

// bossDurationAuditLogged 记录每个目标已经打印过的时长（秒），用于去重，避免每帧刷屏。
var bossDurationAuditLogged sync.Map

// bossFinishCount 记录每个目标收到 finish 的次数，用于识别“同一目标重复下发”。
var bossFinishCount sync.Map

// bossHPZeroTimestampUnsafe 返回该目标 HP 首次归零的时间戳（厘秒），没有则返回 0。
// 调用方需持有 a.mu（读锁即可）。
func (a *App) bossHPZeroTimestampUnsafe(targetID string) int64 {
	history := a.bossHPHistory[targetID]
	for i := range history {
		if history[i].CurrentHP <= bossDamageAdjustEpsilon {
			return history[i].HpTimestamp
		}
	}
	return 0
}

// recordFinishTrace 记录每次 finish（含同一目标出现的次数），用于判断服务器是否对同一目标重复下发。
// 调用方需持有 a.mu。
func (a *App) recordFinishTrace(targetID, name string, now, firstHit, lastHit int64) {
	n := int64(1)
	if v, ok := bossFinishCount.Load(targetID); ok {
		n = v.(int64) + 1
	}
	bossFinishCount.Store(targetID, n)
	_, entityStillTracked := a.bossHP[targetID]
	bossTrace("[BossLife] finish #%d target=%s name=%q deathTime=%d durSec=%.1f gapSec=%.1f(结束比最后命中晚) entityStillTracked=%v(实体是否还在) hpZeroAt=%d",
		n, targetID, name, now,
		centisecondsToSeconds(now-firstHit), centisecondsToSeconds(now-lastHit),
		entityStillTracked, a.bossHPZeroTimestampUnsafe(targetID))
}

// auditBossDurationUnsafe 当目标“持续时间”异常偏长时打印一次诊断。
// 调用方需持有 a.mu（读锁即可）。endTime 与展示口径一致（dead → deathTime，否则 → lastHit）。
func (a *App) auditBossDurationUnsafe(targetID, name, status string, firstHit, lastHit, deathTime, endTime int64) {
	durSec := int64(centisecondsToSeconds(endTime - firstHit))
	if durSec < bossDurationAuditMinSeconds {
		return
	}
	if prev, ok := bossDurationAuditLogged.Load(targetID); ok && prev.(int64) >= durSec {
		return
	}
	bossDurationAuditLogged.Store(targetID, durSec)

	history := a.bossHPHistory[targetID]
	var histCount int
	var firstHpTs, lastHpTs int64
	if len(history) > 0 {
		histCount = len(history)
		firstHpTs = history[0].HpTimestamp
		lastHpTs = history[len(history)-1].HpTimestamp
	}
	zeroTs := a.bossHPZeroTimestampUnsafe(targetID)

	bossTrace("[BossAudit] target=%s name=%q status=%s durSec=%d "+
		"firstHit=%d lastHit=%d deathTime=%d endTime=%d "+
		"lastHitGapSec=%.1f(结束比最后命中晚) hpZeroAt=%d hpZeroGapSec=%.1f(结束比血量归零晚) "+
		"hpHistory=%d firstHpTs=%d lastHpTs=%d",
		targetID, name, status, durSec,
		firstHit, lastHit, deathTime, endTime,
		centisecondsToSeconds(endTime-lastHit), zeroTs,
		centisecondsToSeconds(endTime-zeroTs),
		histCount, firstHpTs, lastHpTs)
}

// logTargetDurationsUnsafe 在清空实时统计前打印每个目标的起止时间，用于对比“监控结束时间”与真实情况。
// 调用方需持有 a.mu（写锁）。
func (a *App) logTargetDurationsUnsafe(label string) {
	if len(a.takenStats) == 0 {
		return
	}
	for id, stat := range a.takenStats {
		if stat == nil {
			continue
		}
		end := stat.lastHit
		status := "idle"
		if stat.deathTime > 0 {
			end = stat.deathTime
			status = "dead"
		}
		bossTrace("[BossLife] %s target=%s name=%q status=%s durSec=%.1f first=%d last=%d death=%d hpZeroAt=%d",
			label, id, stat.name, status, centisecondsToSeconds(end-stat.firstHit),
			stat.firstHit, stat.lastHit, stat.deathTime, a.bossHPZeroTimestampUnsafe(id))
	}
}
