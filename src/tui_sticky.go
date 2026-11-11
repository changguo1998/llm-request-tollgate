// tui_sticky.go: sticky 页 —— 按逻辑名列出候选链, 人工选择粘性偏好。
//
// 页面上看到的是配置里的候选顺序 (与运行中的别名一致), 当前偏好读观测面 pulse.Sticky,
// 写入走 POST /sticky (同进程与 -tui-state 远程模式同一条路)。语义是「置一次」: 设置后
// 立即生效, 但自动迁移规则照旧, 后续真实失败 / 长期锁仍可把流量迁走。

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// stickyCand 是偏好页的一个候选行。
type stickyCand struct {
	logical  string
	upstream string
	model    string
	pref     bool // 是观测面报告的本逻辑名当前偏好
}

// stickyLine 是页面上的一行: cand >= 0 时可选中 (cands 下标), -1 是逻辑名分组标题。
type stickyLine struct {
	text string
	cand int
}

// stickyCands 按配置里的候选人展开候选链 (别名 + 组, 组 = 成员候选链拼接),
// 并标出当前偏好 (pulse.Sticky)。逻辑名按字典序排列, 同一逻辑名的候选保持配置
// 顺序 (即 fallback 顺序)。组名对客户端与逻辑名无差别, 这里同样可置粘性偏好。
func (m *tuiModel) stickyCands() ([]stickyCand, error) {
	c, err := loadConfigOpt(m.cfgPath, true)
	if err != nil {
		return nil, err
	}
	aliases := c.expandedAliases()
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]stickyCand, 0)
	for _, name := range names {
		p := m.pulse.Sticky[name]
		for _, tgt := range aliases[name] {
			out = append(out, stickyCand{
				logical: name, upstream: tgt[0], model: tgt[1],
				pref: p.Upstream == tgt[0] && p.Model == tgt[1],
			})
		}
	}
	return out, nil
}

// drawSticky 画偏好页: 标题 / 消息行 / 分组候选列表。光标行反显, 当前偏好标 *,
// 窗口跟随光标 (上下移动时自动滚动)。
func (m *tuiModel) drawSticky(c *tuiCanvas, body int) {
	cands, err := m.stickyCands()
	if err != nil {
		c.line(tuiIndent + tuiYell + "配置读不到: " + err.Error() + tuiReset)
		return
	}
	c.line(tuiTitle + tuiIndent + "粘性偏好 (只置一次, 自动迁移照旧; Enter 置偏好, c 清除)" + tuiReset)
	if m.stickyMsg != "" {
		col := tuiGreen
		if m.stickyErr {
			col = tuiRed
		}
		c.line(tuiIndent + col + m.stickyMsg + tuiReset)
	} else {
		c.line("")
	}
	if len(cands) == 0 {
		c.line(tuiDim + tuiIndent + "(配置里没有别名)" + tuiReset)
		return
	}

	m.stickySel = clampInt(m.stickySel, 0, len(cands)-1)
	lines := make([]stickyLine, 0, len(cands)+8)
	selLine := 0
	last := ""
	for i, cand := range cands {
		if cand.logical != last {
			hdr := tuiBold + cand.logical + tuiReset
			// 倒计时列: 窗口内只显剩余时间 (mm:ss), 不带文字; 失效后该逻辑名已不在
			// 观测面里, 倒计时与星号都不会出现。
			if p, ok := m.pulse.Sticky[cand.logical]; ok {
				if rem := time.Duration(p.RemainingMs) * time.Millisecond; rem > 0 {
					hdr += "  " + tuiDim + durStr(rem) + tuiReset
				}
			}
			lines = append(lines, stickyLine{text: hdr, cand: -1})
			last = cand.logical
		}
		mark := " "
		if cand.pref {
			mark = "*"
		}
		text := "  " + mark + " " + m.upModel(cand.upstream, cand.model)
		if i == m.stickySel {
			// 反显整行; 不走 upModel 的上游专属色, 否则名字后的 reset 会提前清掉反显。
			text = tuiRev + "  " + mark + " " + cand.upstream + "/" + cand.model + tuiReset
			selLine = len(lines)
		}
		lines = append(lines, stickyLine{text: text, cand: i})
	}

	limit := max(1, body-2) // 标题 + 消息行
	if selLine < m.stickyTop {
		m.stickyTop = selLine
	}
	if selLine >= m.stickyTop+limit {
		m.stickyTop = selLine - limit + 1
	}
	m.stickyTop = clampScroll(m.stickyTop, len(lines), limit)
	end := min(m.stickyTop+limit, len(lines))
	for _, ln := range lines[m.stickyTop:end] {
		if ln.cand < 0 {
			c.line(tuiIndent + tuiTitle + ln.text + tuiReset)
			continue
		}
		c.line(tuiIndent + ln.text)
	}
}

// stickyBase 是写入地址: 远程看 -tui-state 给的 base, 同进程用自己的监听地址。
func (m *tuiModel) stickyBase() string {
	if m.stateURL != "" {
		return m.stateURL
	}
	if m.listen == "" {
		return ""
	}
	return "http://" + m.listen
}

// stickyPost 发 POST /sticky (参数走 query), 非 200 时带上响应片段。
func (m *tuiModel) stickyPost(q url.Values) error {
	base := m.stickyBase()
	if base == "" {
		return fmt.Errorf("没有服务地址 (配置缺 listen)")
	}
	req, err := http.NewRequest(http.MethodPost, base+"/sticky?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("状态码 %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// stickyMove 上下移动光标 (d = ±1), 位置夹在候选范围内。
func (m *tuiModel) stickyMove(d int) {
	cands, err := m.stickyCands()
	if err != nil || len(cands) == 0 {
		return
	}
	m.stickySel = clampInt(m.stickySel+d, 0, len(cands)-1)
	m.stickyMsg = ""
}

// stickyJump 跳到第一个 / 最后一个候选。
func (m *tuiModel) stickyJump(last bool) {
	cands, err := m.stickyCands()
	if err != nil || len(cands) == 0 {
		return
	}
	if last {
		m.stickySel = len(cands) - 1
	} else {
		m.stickySel = 0
	}
	m.stickyMsg = ""
}

// stickyApply 把光标处的候选置为该逻辑名的偏好 (置一次)。
func (m *tuiModel) stickyApply() {
	cands, err := m.stickyCands()
	if err != nil {
		m.stickyMsg, m.stickyErr = "配置读不到: "+err.Error(), true
		return
	}
	if len(cands) == 0 {
		m.stickyMsg, m.stickyErr = "没有可选的候选", true
		return
	}
	m.stickySel = clampInt(m.stickySel, 0, len(cands)-1)
	c := cands[m.stickySel]
	q := url.Values{}
	q.Set("logical", c.logical)
	q.Set("upstream", c.upstream)
	q.Set("model", c.model)
	if err := m.stickyPost(q); err != nil {
		m.stickyMsg, m.stickyErr = "置偏好失败: "+err.Error(), true
		return
	}
	m.stickyMsg, m.stickyErr = "已置 "+c.logical+" -> "+c.upstream+"/"+c.model, false
}

// stickyClear 清除光标所在逻辑名的偏好 (回落配置首候选)。
func (m *tuiModel) stickyClear() {
	cands, err := m.stickyCands()
	if err != nil {
		m.stickyMsg, m.stickyErr = "配置读不到: "+err.Error(), true
		return
	}
	if len(cands) == 0 {
		m.stickyMsg, m.stickyErr = "没有可选的候选", true
		return
	}
	m.stickySel = clampInt(m.stickySel, 0, len(cands)-1)
	logical := cands[m.stickySel].logical
	q := url.Values{}
	q.Set("logical", logical)
	if err := m.stickyPost(q); err != nil {
		m.stickyMsg, m.stickyErr = "清除失败: "+err.Error(), true
		return
	}
	m.stickyMsg, m.stickyErr = "已清除 "+logical+" 的偏好 (回落配置首候选)", false
}
