// gate_queue.go: 两层队列的入队、出队、迁移与清理。

package main

// enqueueLocked 把一项挂到某条队列的队尾, 并记下它在哪条队列 (调用方持有 c.mu)。
func (c *cand) enqueueLocked(t *ticket, kind queueKind) {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	c.seen[t.model] = true // 模型视角只列真正进过队列的 上游/模型
	switch kind {
	case queueFlow:
		c.flow = append(c.flow, t)
	case queueModel:
		if c.modelQ == nil {
			c.modelQ = map[string][]*ticket{}
		}
		c.modelQ[t.model] = append(c.modelQ[t.model], t)
	}
	t.where = kind
}

// dequeueLocked 把一项从某条队列的队首摘掉 (调用方持有 c.mu)。
func (c *cand) dequeueLocked(t *ticket, kind queueKind) {
	switch kind {
	case queueFlow:
		if c.flowHead(t) {
			c.flow = c.flow[1:]
		}
	case queueModel:
		if q := c.modelQ[t.model]; len(q) > 0 && q[0] == t {
			c.modelQ[t.model] = q[1:]
		}
	}
	t.where = queueNone
}

// removeTicketLocked 把一项从它此刻所在的队列里摘掉 (调用方持有 c.mu)。
func (c *cand) removeTicketLocked(t *ticket) {
	switch t.where {
	case queueFlow:
		for i, x := range c.flow {
			if x == t {
				c.flow = append(c.flow[:i], c.flow[i+1:]...)
				break
			}
		}
	case queueModel:
		q := c.modelQ[t.model]
		for i, x := range q {
			if x == t {
				c.modelQ[t.model] = append(q[:i], q[i+1:]...)
				break
			}
		}
	}
	t.where = queueNone
}

// queued 是还在两层队列里等的请求数 (观测面用; 调用方持有 c.mu)。
func (c *cand) queued() int {
	n := len(c.flow)
	for _, q := range c.modelQ {
		n += len(q)
	}
	return n
}

// dropTicket 把一项从队列里摘掉 (客户端断开或等待超时)。
func (c *cand) dropTicket(t *ticket) {
	c.mu.Lock()
	c.removeTicketLocked(t)
	c.mu.Unlock()
	c.signal()
}

// finish 结束一笔发送: 正常占用的归还槽位, 试探按结果放宽上限。
func (c *cand) finish(kind slotKind, ok bool) {
	c.mu.Lock()
	if kind == slotProbe {
		c.probing = false
		if ok {
			c.softCap++ // 试探成功 → 上限 +1, 不设天花板也不落盘
		}
	} else {
		c.inFlight--
	}
	c.mu.Unlock()
	c.signal()
}
