// tui_model.go: 状态台模型与配置加载。

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type tuiModel struct {
	cfgPath  string
	logPath  string
	listen   string
	stateURL string // 空 = 同进程直接读内存
	// perfIdx 是性能列的参数来源 (同进程 = app.perfIndex; 远端模式为空时退原始速率)。
	perfIdx func(up, model string, c, d, b int64) float64

	start   time.Time
	pulse   pulse
	live    bool
	lastErr string
	mu      sync.Mutex // 保护 lastErr (监听失败可能来自别的 goroutine)

	routes map[string]*routeRow
	events []routeEvent

	// events 页的筛选: 文本筛 编号/逻辑名/事件; 时间范围生效时改用日志回读的
	// 结果 (ev.rows), 不再受 256 条事件上限约束。
	ev       evFilter
	evPrompt *tuiPrompt // 第 2 页的行内输入态 (f 文本 / t 时间范围)
	evMsg    string     // 时间范围写法不对时的提示

	window       time.Duration     // 回看窗口 (行只算窗口内的记录)
	rateWin      time.Duration     // 速度平均/峰值窗口 (rate_window_ms)
	blockWin     time.Duration     // 兼容保留 (第 1 页改成平表后不再按块定行高)
	logPos       map[string]int64  // 每个日志文件读到哪
	logRest      map[string][]byte // 每文件各自的半行缓冲 (读到一半的行)
	scannedFiles map[string]bool   // 归档已整读, 不再重复扫
	scanned      int

	tab        int
	scroll     [4]int // 每页各自的滚动位置: m.scroll[m.tab]
	cols       int
	rows       int
	statusRows int       // 顶栏状态区固定占行数 (=2), 内容区高度按它扣
	refreshed  time.Time // 上次重读日志/取观测的时刻 (按键连发时别每次都重读)

	// 顶栏「流量」的每秒速率: 每次取到观测面时, 累加各笔请求已转发字节相比上一拍的增量,
	// 再除以采样间隔。flowBytes 是上一拍的基线 (请求编号 -> 当时已转发的累计字节),
	// bytesAt 是上一拍的采样时刻。
	flowBytes map[uint64]int64
	bytesAt   time.Time
	flowRate  float64
	pageTotal int // 第 1 页 (请求表 + 两张模型表) 的总行数 (滚动用)
	pageTop   int // 该页当前窗口的第一行
	pageWin   int // 该页当前窗口的数据行数 (提示行里的 a-b 按它算)

	prio     *tuiEditor   // settings 页 (调度参数 + 权重编辑), runTUI 里装载
	reloadFn func() error // 同进程模式的热加载回调; 空 = 走 deploy/tollgate.sh hup

	// sticky 页: 光标在候选行里的位置、窗口第一行, 以及最近一次操作结果。
	stickySel int
	stickyTop int
	stickyMsg string
	stickyErr bool

	// 第 1 页上游表的排序键 (provider 优先级), 按配置的 mtime 失效 (权重在配置文件里)。
	provRank    map[string][2]int
	provRankMod time.Time

	// 上游专属色 (每帧按上游表顺序重算), 各页的 上游/模型 单元格共用。
	upColors map[string]string

	// 逻辑名专属色 (配置里的逻辑名 + 组名, 按窗口内请求的衰减权重排序分配; 各页共用,
	// 名字后叠一层加粗, 与正文里的供应商名区分)。logicalNames 按配置 mtime 缓存, 变了才重读。
	logicalColors   map[string]string
	logicalNames    []string
	logicalNamesMod time.Time
	// nameEnds 是各逻辑名窗口内每笔请求的结束时刻 (配色权重用, 见 tui_weight.go)。
	nameEnds map[string][]time.Time
}

// errf 记下最近一次取数失败 (可能来自别的 goroutine, 加锁)。
func (m *tuiModel) errf(format string, args ...any) {
	m.mu.Lock()
	m.lastErr = fmt.Sprintf(format, args...)
	m.mu.Unlock()
}

func (m *tuiModel) lastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func newTUIModel(cfgPath string) *tuiModel {
	rateWin, blockWin := tuiWindowConfig(cfgPath)
	m := &tuiModel{
		cfgPath: cfgPath, start: time.Now(),
		routes:  map[string]*routeRow{},
		window:  tuiWindow,
		rateWin: rateWin, blockWin: blockWin,
		logPos: map[string]int64{}, logRest: map[string][]byte{}, scannedFiles: map[string]bool{},
		nameEnds: map[string][]time.Time{},
	}
	return m
}

// tuiWindowConfig 从配置文件的扁平顶层键读 TUI 窗口 (不依赖 key_env, TUI 只读 display 参数)。
// 读不到或非正数时用缺省: 性能窗 5min (与低性能锁共用), 分块采样时间尺度 35s (后者只为兼容保留)。
func tuiWindowConfig(cfgPath string) (rateWin, blockWin time.Duration) {
	rateWin, blockWin = 300*time.Second, 35*time.Second
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var raw struct {
		PerfWindowSec  float64 `json:"perf_window_sec"`
		BlockWindowSec float64 `json:"block_window_sec"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	if raw.PerfWindowSec > 0 {
		rateWin = time.Duration(secToMs(raw.PerfWindowSec)) * time.Millisecond
	}
	if raw.BlockWindowSec > 0 {
		blockWin = time.Duration(secToMs(raw.BlockWindowSec)) * time.Millisecond
	}
	return
}

// loadCfg 从配置里取 监听地址 / 日志路径 (TUI 只读这两个字段)。
func (m *tuiModel) loadCfg() {
	c, err := loadConfigOpt(m.cfgPath, true)
	if err != nil {
		m.lastErr = "配置读不到: " + err.Error()
		return
	}
	m.listen, m.logPath = c.Listen, c.LogPath
}
