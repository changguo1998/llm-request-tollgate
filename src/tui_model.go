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

	start   time.Time
	pulse   pulse
	live    bool
	lastErr string
	mu      sync.Mutex // 保护 lastErr (监听失败可能来自别的 goroutine)

	routes map[string]*routeRow
	events []routeEvent

	window       time.Duration     // 回看窗口 (行只算窗口内的记录)
	rateWin      time.Duration     // 速度平均/峰值窗口 (rate_window_ms)
	blockWin     time.Duration     // active 分块采样时间尺度 (block_window_ms)
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
	pageTotal int // 第 1 页 (分块 + 两张模型表) 的总行数 (滚动用)
	pageTop   int // 该页当前窗口的第一行
	pageWin   int // 该页当前窗口的数据行数 (提示行里的 a-b 按它算)

	prio     *tuiEditor   // settings 页 (调度参数 + 权重编辑), runTUI 里装载
	reloadFn func() error // 同进程模式的热加载回调; 空 = 走 deploy/tollgate.sh hup

	// sticky 页: 光标在候选行里的位置、窗口第一行, 以及最近一次操作结果。
	stickySel int
	stickyTop int
	stickyMsg string
	stickyErr bool

	// 第 1 页上游表的排序键 (provider 优先级), 按 store / 配置的 mtime 失效。
	provRank    map[string][2]int
	provRankMod [2]time.Time

	// 上游专属色 (每帧按上游表顺序重算), 各页的 上游/模型 单元格共用。
	upColors map[string]string

	// 第 1 页各分块的行数统计: 每次取到观测面后记一笔样本 (各块在飞笔数), 行数的参考值
	// 用窗口内的样本算; durSum / durN 是全部能读到的请求的耗时合计与笔数 (定权重的时间尺度)。
	rowHist []rowSample
	durSum  int64
	durN    int64
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
	}
	return m
}

// tuiWindowConfig 从配置文件的扁平顶层键读 TUI 窗口 (不依赖 key_env, TUI 只读 display 参数)。
// 读不到或非正数时用缺省: 速度窗 1h, 分块采样时间尺度 35s。
func tuiWindowConfig(cfgPath string) (rateWin, blockWin time.Duration) {
	rateWin, blockWin = time.Hour, 35*time.Second
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var raw struct {
		RateWindowSec  float64 `json:"rate_window_sec"`
		BlockWindowSec float64 `json:"block_window_sec"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	if raw.RateWindowSec > 0 {
		rateWin = time.Duration(secToMs(raw.RateWindowSec)) * time.Millisecond
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
