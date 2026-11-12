// tui_state.go: 观测面轮询与事件合并。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

// tuiEventKeep 是 TUI 事件历史的上限: 只留最近的, 够翻就行。
// 它只服务第 2 页 (翻看 / 同秒合并 / 算列宽), 不参与调度与记账。
const tuiEventKeep = 256

func (m *tuiModel) addEvent(e routeEvent) {
	m.events = append(m.events, e)
	if len(m.events) > tuiEventKeep {
		m.events = append(m.events[:0], m.events[len(m.events)-tuiEventKeep:]...)
	}
}

// pollState 同进程直接读内存; 另进程走 GET /state?since=。
func (m *tuiModel) pollState(app *App, since uint64) uint64 {
	if app != nil {
		p := app.snapshot(since)
		m.pulse, m.live, m.lastErr = p, true, ""
		now := time.Now()
		m.sampleFlowBytes(now)
		m.mergeEvents(p.Events)
		return p.MaxSeq
	}
	url := m.stateURL + "/state?since=" + strconv.FormatUint(since, 10)
	cl := http.Client{Timeout: 2 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		m.live, m.lastErr = false, "观测面不可达: "+err.Error()
		return since
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var p pulse
	if err := json.Unmarshal(b, &p); err != nil {
		m.live, m.lastErr = false, "观测面返回异常: "+err.Error()
		return since
	}
	if p.Ver != viewVer { // 对端不是这个接口时, 别把它的响应当成"没数据"
		m.live = false
		m.errf("对端没有观测面接口 (ver=%q): 那一边需要换成带 %s 的二进制并重启", p.Ver, viewVer)
		m.pulse = pulse{}
		return since
	}
	m.pulse, m.live, m.lastErr = p, true, ""
	now := time.Now()
	m.sampleFlowBytes(now)
	m.mergeEvents(p.Events)
	return p.MaxSeq
}

// sampleFlowBytes 把进行中请求已转发的字节换算成每秒速率: 与上一拍比, 只累加各笔请求
// 在这一个采样间隔里新增的字节, 再除以两拍的实际间隔。第一拍只记基线不算速率; 上一拍还
// 没有的请求说明它这期间才开始, 现在的累计字节全算这一拍; 已经结束的请求不再出现, 它的
// 字节也就不会拉出负数。每次取到观测面后调用。
func (m *tuiModel) sampleFlowBytes(now time.Time) {
	cur := make(map[uint64]int64, len(m.pulse.Active))
	var delta int64
	for _, f := range m.pulse.Active {
		cur[f.ID] = f.RecvBytes
		prev, seen := m.flowBytes[f.ID]
		switch {
		case !seen: // 这期间才开始的请求: 累计字节都在这一拍里
			delta += f.RecvBytes
		case f.RecvBytes > prev:
			delta += f.RecvBytes - prev
		}
	}
	m.flowBytes = cur
	if !m.bytesAt.IsZero() {
		if el := now.Sub(m.bytesAt); el > 0 {
			m.flowRate = float64(delta) / el.Seconds()
		}
	}
	m.bytesAt = now
}

// mergeEvents 把服务侧事件 (锁/恢复/请求结局) 并进事件流。
func (m *tuiModel) mergeEvents(evs []viewEvent) {
	for _, e := range evs {
		ts, _ := time.Parse(time.RFC3339Nano, e.Ts)
		m.addEvent(routeEvent{Ts: ts, Req: e.Req, Kind: e.Kind, Logical: e.Logical, Upstream: e.Upstream,
			Model: e.Model, Msg: e.Msg})
	}
}

// 页号与页签一一对应 (tuiTabs 的次序); 比较 m.tab 时用这几个名字, 别写裸数字。
const (
	tabActive   = 0 // 活动连接, 每笔请求按尝试一行、阶段成列; 下方接上游状态与模型/路由聚合
	tabEvents   = 1
	tabSettings = 2 // 设置: gate 参数 + 优先级 + 粘性偏好
)

// 时长一律走 durStr (12s / 02:03 / 01:02:03), 屏幕上不再出现"分 / 秒 / 小时"这类汉字写法。
var tuiTabs = []string{"active", "events", "settings"}
