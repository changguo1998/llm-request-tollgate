// tui_state.go: 观测面轮询与事件合并。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

func (m *tuiModel) addEvent(e routeEvent) {
	m.events = append(m.events, e)
	if len(m.events) > 4000 { // 只留最近的, 够翻就行
		m.events = append(m.events[:0], m.events[len(m.events)-2000:]...)
	}
}

// pollState 同进程直接读内存; 另进程走 GET /v1/state?since=。
func (m *tuiModel) pollState(app *App, since uint64) uint64 {
	if app != nil {
		p := app.snapshot(since)
		m.pulse, m.live, m.lastErr = p, true, ""
		m.noteRows(time.Now())
		m.mergeEvents(p.Events)
		return p.MaxSeq
	}
	url := m.stateURL + "/v1/state?since=" + strconv.FormatUint(since, 10)
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
	m.noteRows(time.Now())
	m.mergeEvents(p.Events)
	return p.MaxSeq
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
	tabActive   = 0 // 活动连接, 按流程阶段分块; 下方接上游状态与模型/路由聚合
	tabEvents   = 1
	tabSettings = 2 // 设置: gate 参数 + 优先级
)

// 时长一律走 durStr (12s / 02:03 / 01:02:03), 屏幕上不再出现"分 / 秒 / 小时"这类汉字写法。
var tuiTabs = []string{"active", "events", "settings"}
