// recover_probe.go: 锁恢复探测 —— 锁到期进入恢复态后, 用一笔内置的最小请求验证可达性,
// 探测通过才解锁, 失败按原锁时长重新加锁。触发行: 锁到期定时器 + 命中恢复态的真实请求。

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// probeBody 是内置的最小探测请求 (固定, 不进配置): 一个 token 的对话, 只为探可达性。
// 发送前把 {model} 换成真实模型 id。探测直接走 transport, 不经 serve 主链路, 所以
// 不写请求日志、不进 flow 视图、也不进吞吐/性能样本。
const probeBody = `{"model":"{model}","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`

// gateSnap 取当前门控参数快照 (探测失败重锁 / 判定开关用)。
func (a *App) gateSnap() GateCfg {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.gate
}

// probeModel 给恢复探测定一个真实模型 id: 模型锁直接用 t.model; 额度组锁从该组的
// 模型里挑一个代表 (上游声明过 models 就查组表, 否则从别名里找任一指向该组的模型)。
func (a *App) probeModel(upID, group string, t recoverRef) string {
	if t.isModel {
		return t.model
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if by, ok := a.groups[upID]; ok {
		for m, g := range by {
			if g == group {
				return m
			}
		}
	}
	for _, list := range a.aliases {
		for _, at := range list {
			if at.Upstream == upID && at.Group == group {
				return at.Model
			}
		}
	}
	return ""
}

// armLockRecovery 按锁的到期时间挂一个一次性定时器, 到点进入恢复并发起探测。
// 每把 probeable 锁只在加锁/重锁时挂一次; 期间被请求抢先认领也不会重复探测。
func (a *App) armLockRecovery(t recoverRef, until time.Time) {
	d := time.Until(until)
	if d < 0 {
		d = 0
	}
	time.AfterFunc(d, func() {
		if _, claimed := t.c.beginRecovery(t.model, t.group); claimed {
			go a.runRecovery(t.c, t)
		}
	})
}

// kickRecovery 请求路径触发: 命中恢复态时认领一笔探测 (去重), 认领到就后台发。
func (a *App) kickRecovery(c *cand, model, group string) {
	r, claimed := c.beginRecovery(model, group)
	if claimed {
		go a.runRecovery(c, r)
	}
}

// runRecovery 发一笔探测并结算: 通过解锁, 失败重锁并重挂下一次。后台 goroutine 执行,
// 不阻塞请求与定时器。
func (a *App) runRecovery(c *cand, t recoverRef) {
	g := a.gateSnap()

	a.mu.RLock()
	enabled := a.cfg != nil && a.cfg.probeEnabled()
	up := a.upstreams[c.id]
	a.mu.RUnlock()
	modelID := a.probeModel(c.id, t.group, t)

	// 恢复已关掉 / 上游没了 / 探不到可用的真实模型: 按通过兜底直接解锁, 不拖成无限锁。
	if !enabled || up == nil || modelID == "" {
		c.settleRecovery(t, true, g)
		return
	}
	a.evAdd("probe", "", c.id, modelID, "锁到期, 开始探测")

	body := []byte(strings.ReplaceAll(probeBody, "{model}", modelID))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		up.BaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		// 构造失败极罕见: 按失败重锁重挂, 下次再探。
		until := c.settleRecovery(t, false, g)
		a.armLockRecovery(t, until)
		a.evAdd("probe", "", c.id, modelID, "探测失败 (请求无法构造), 重新加锁, "+durStr(recoverLockDur(t.reason, g)))
		return
	}
	req.ContentLength = int64(len(body))
	prepareOutboundHeaders(req.Header, http.Header{}, up)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.transport.RoundTrip(req)
	if err != nil {
		until := c.settleRecovery(t, false, g)
		a.armLockRecovery(t, until)
		a.evAdd("probe", "", c.id, modelID, "探测失败 (传输错误), 重新加锁, "+durStr(recoverLockDur(t.reason, g)))
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ok := resp.StatusCode == http.StatusOK
	if ok {
		c.settleRecovery(t, true, g)
		a.evAdd("probe", "", c.id, modelID, "探测通过, 解锁")
		return
	}
	until := c.settleRecovery(t, false, g)
	a.armLockRecovery(t, until)
	a.evAdd("probe", "", c.id, modelID,
		"探测失败 (HTTP "+strconv.Itoa(resp.StatusCode)+"), 重新加锁, "+durStr(recoverLockDur(t.reason, g)))
}
