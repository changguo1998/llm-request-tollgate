// app.go: App 结构与装配 (热加载、连接池)。

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type App struct {
	mu        sync.RWMutex
	cfg       *Config
	upstreams map[string]*UpstreamCfg
	// groups 是「上游 -> 真实模型 -> 额度组名」的索引 (applyConfig 重建), 热路径
	// 据此把 (上游, 模型) 解析成组, 不必每次扫 models 表。
	groups map[string]map[string]string
	// groupOrder 是「上游 -> 组名列表」, 按配置里首次出现的顺序 (含默认组的空串),
	// 状态台用它铺记账列。
	groupOrder map[string][]string
	aliases    map[string][]aliasTarget
	// 调度: 每个上游一份队列与自适应并发上限, 门控参数对所有上游共用。
	cands map[string]*cand
	gate  GateCfg
	// 粘性偏好: 逻辑名 -> 最近成功候选, 让同逻辑名的流量粘在同一上游 (缓存热度)。
	// 纯内存, 不落盘; 重启从配置顺序重新积累。
	sticky *sticky
	// transport 启动时建一次 (连接池复用); 三项超时单独原子存, applyConfig 热加载时
	// 更新, 新连接 / 每次读写取当前值, 不必重启进程。
	transport   *http.Transport
	connectNs   atomic.Int64 // 建连超时 (纳秒)
	streamIdlNs atomic.Int64 // 流空闲超时 (纳秒)

	gatePath string // 定时锁状态文件 (var/gate-state.json)

	logMu  sync.Mutex
	logW   *os.File
	logger *log.Logger
	logDay string // 当前日志文件所属的本地日期 (按天轮转用)

	// 只读观测面 (TUI): 进行中的请求、最近事件、事件序号。纯内存、不落盘、不参与记账,
	// 服务重启即清空 —— 明细的事实来源仍是日志。
	flowMu   sync.Mutex
	flows    map[uint64]*flow
	nextFlow uint64
	evMu     sync.Mutex
	events   [viewEvents]viewEvent
	evNext   uint64 // 累计事件序号 (TUI 拿它当起点, 只取新增的那部分)
}

type aliasTarget struct {
	Upstream string
	Model    string
	Group    string // 该模型在上游 models 里声明的额度组 (空 = 默认组)
}

func newApp(c *Config) (*App, error) {
	a := &App{
		upstreams: map[string]*UpstreamCfg{},
		aliases:   map[string][]aliasTarget{},
		cands:     map[string]*cand{},
		flows:     map[uint64]*flow{},
		sticky:    newSticky(),
	}
	a.applyConfig(c)
	if err := a.openLog(); err != nil {
		return nil, err
	}
	// 锁状态放在日志同目录 (var/), 不进仓库。
	if c.LogPath != "" {
		a.gatePath = filepath.Join(filepath.Dir(c.LogPath), "gate-state.json")
	}
	a.loadGateState()
	return a, nil
}

// applyConfig 重建上游表与别名表; 限速器按上游 id 保留, 不因热加载被重置。
func (a *App) applyConfig(c *Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = c
	// 三项超时热加载: connect / stream_idle 原子存, 建连与每次读写取当前值;
	// response_header 写回已建 Transport (net/http 每笔请求开始时读该字段即生效)。
	to := c.timeouts()
	a.connectNs.Store(int64(time.Duration(to.ConnectMs) * time.Millisecond))
	a.streamIdlNs.Store(int64(time.Duration(to.StreamIdleMs) * time.Millisecond))
	rh := time.Duration(to.ResponseHeaderMs) * time.Millisecond
	// 门控参数是全局的, 热加载直接生效; 队列与自适应上限按上游 id 保留 (爬了半天的
	// 上限不该被 hup 清掉, 排队中的请求也不该被丢)。
	a.gate = normGate(c.gate())
	// settings 页离线编辑时 (tui_model) 也会拼一个 App 再 applyConfig, 表可能还没建。
	if a.cands == nil {
		a.cands = map[string]*cand{}
	}
	upstreams := map[string]*UpstreamCfg{}
	groups := map[string]map[string]string{} // 上游 -> 真实模型 -> 额度组名
	groupSeq := map[string][]string{}        // 上游 -> 组名 (按配置首次出现, 含默认组)
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		upstreams[u.ID] = u
		byModel := make(map[string]string, len(u.Models))
		var names []string
		seenGroup := map[string]bool{}
		for _, m := range u.Models {
			byModel[m.ID] = m.BillingGroup
			if !seenGroup[m.BillingGroup] {
				seenGroup[m.BillingGroup] = true
				names = append(names, m.BillingGroup)
			}
		}
		if len(names) == 0 {
			names = []string{defaultGroup} // 不写 models 的上游: 只有一个默认组
		}
		groupSeq[u.ID] = names
		groups[u.ID] = byModel
		// 调度状态按上游 id 保留不重建: 队列里排着的请求、爬到的并发上限、退避与锁
		// 都该跨热加载延续 (上限有意不落盘, 但 hup 不该把它清掉)。
		if _, ok := a.cands[u.ID]; !ok {
			a.cands[u.ID] = newCand(u.ID)
		}
		// 锁恢复探测开关热加载生效; 关闭时顺手清掉残留的恢复态, 让到期锁回到普通时间锁。
		cd := a.cands[u.ID]
		cd.recoverProbe = c.probeEnabled()
		if !cd.recoverProbe {
			cd.denyRecovery()
		}
	}
	a.upstreams = upstreams
	a.groups = groups
	a.groupOrder = groupSeq
	aliases := map[string][]aliasTarget{}
	for name, list := range c.Aliases {
		for _, pair := range list {
			aliases[name] = append(aliases[name], aliasTarget{
				Upstream: pair[0], Model: pair[1], Group: groups[pair[0]][pair[1]],
			})
		}
	}
	a.aliases = aliases
	// 粘性偏好跨热加载保留, 但候选集变化后越界的作废 (配置顺序重新积累)。
	if a.sticky != nil {
		a.sticky.prune(aliases)
	}
	// transport 启动时建一次 (不重建连接池); 热加载只更新 response_header 超时。
	if a.transport == nil {
		a.transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Duration(a.connectNs.Load())}
				conn, err := d.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				// 每次读写重设 idle 死线 (取自实时值) 实现流空闲超时。
				return &idleConn{Conn: conn, a: a}, nil
			},
			ResponseHeaderTimeout: rh,
		}
	} else {
		a.transport.ResponseHeaderTimeout = rh
	}
}

type idleConn struct {
	net.Conn
	a *App
}

func (c *idleConn) deadline() {
	ns := c.a.streamIdlNs.Load()
	if ns <= 0 {
		return // 未配置流空闲超时 (或旧配置没给): 不设死线, 0 不能当成"立即超时"
	}
	c.Conn.SetDeadline(time.Now().Add(time.Duration(ns)))
}

func (c *idleConn) Read(b []byte) (int, error) {
	c.deadline()
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	c.deadline()
	return c.Conn.Write(b)
}
