package events

import "sort"

// TraderBill 是贸易方能看到的自己那份提单的权利与状态。
type TraderBill struct {
	BillID         string
	IsHolder       bool // 本方是否当前控制权人
	Active         bool
	CargoCode      string
	Quantity       string
	PledgedTo      string // 非空表示质押中
	Frozen         bool
	Paid           bool
	TransportNodes []string // 运输节点代码（已按序号归并，含晚到回补）
	Endorsements   int
}

// Todo 是贸易方的一项待办。
type Todo struct {
	BillID string
	Kind   string // incoming_sign / outgoing_pending / pledge_release / dispute
	Detail string
}

// TraderView 汇总某贸易方的全部权利与待办，不包含其他贸易方的单据。
type TraderView struct {
	Party string
	Bills []TraderBill
	Todos []Todo
}

// ViewForTrader 按参与方过滤：只能看到自己持有或曾经持有的提单。
func (s *Store) ViewForTrader(party string) TraderView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := TraderView{Party: party}
	for _, st := range s.states {
		heldBefore := false
		for _, en := range st.EndorsementSeq {
			if en.From == party || en.To == party {
				heldBefore = true
				break
			}
		}
		if st.Holder != party && !heldBefore {
			continue
		}
		nodes := make([]string, 0, len(st.Transport))
		for _, n := range st.Transport {
			nodes = append(nodes, n.Code)
		}
		v.Bills = append(v.Bills, TraderBill{
			BillID: st.BillID, IsHolder: st.Holder == party, Active: st.Active,
			CargoCode: st.Batch.CargoCode, Quantity: st.Batch.Quantity,
			PledgedTo: st.Pledgee, Frozen: st.Frozen, Paid: st.Paid,
			TransportNodes: nodes, Endorsements: len(st.EndorsementSeq),
		})
		if st.Holder == party {
			if st.Frozen {
				v.Todos = append(v.Todos, Todo{st.BillID, "dispute", "提单争议冻结中，等待处理结果"})
			}
			if st.Pledgee != "" {
				v.Todos = append(v.Todos, Todo{st.BillID, "pledge_release", "提单质押中，等待 " + st.Pledgee + " 解除质押"})
			}
		}
	}
	for _, p := range s.proposals {
		switch {
		case p.To == party:
			v.Todos = append(v.Todos, Todo{p.BillID, "incoming_sign", p.From + " 发起转让，等待本方会签"})
		case p.From == party:
			v.Todos = append(v.Todos, Todo{p.BillID, "outgoing_pending", "已发起转让给 " + p.To + "，等待对方会签"})
		}
	}
	sort.Slice(v.Bills, func(i, j int) bool { return v.Bills[i].BillID < v.Bills[j].BillID })
	sort.Slice(v.Todos, func(i, j int) bool {
		if v.Todos[i].BillID != v.Todos[j].BillID {
			return v.Todos[i].BillID < v.Todos[j].BillID
		}
		return v.Todos[i].Kind < v.Todos[j].Kind
	})
	return v
}

// PaymentProof 是银行履约所需的最小证明：只保留放款判定需要的字段，
// 不暴露背书明细、改单历史等无关信息。控制权是否匹配某笔支付指令，
// 由银行在放款事件里携带支付条件、由日志在入账时做权威核对。
type PaymentProof struct {
	BillID         string
	CurrentHolder  string // 当前控制权人（银行将其与指令收款人比对）
	RequiredNode   string // 条件要求的运输节点
	NodeReported   bool   // 该运输事实是否已发生（含离线回补后归并）
	PledgeReleased bool   // 质押是否已解除
	Frozen         bool
	ReadyToPay     bool
	AlreadyPaid    bool
	PaidTo         string
}

// BankView 是银行可见的证明清单，范围限于本行质押或本行放款过的提单。
type BankView struct {
	Bank   string
	Proofs []PaymentProof
}

// ViewForBank 采用字段白名单 + 范围限定，银行读不到与本行履约无关的单据。
// 可见范围：本行当前持有质押权、或本行已经放款的提单。
func (s *Store) ViewForBank(bank string, requiredNode string) BankView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := BankView{Bank: bank}
	for _, st := range s.states {
		if st.Pledgee != bank && st.Financier != bank {
			continue
		}
		nodeReported := requiredNode == ""
		for _, n := range st.Transport {
			if n.Code == requiredNode {
				nodeReported = true
				break
			}
		}
		pledgeReleased := st.Pledgee == ""
		proof := PaymentProof{
			BillID: st.BillID, CurrentHolder: st.Holder,
			RequiredNode: requiredNode, NodeReported: nodeReported,
			PledgeReleased: pledgeReleased, Frozen: st.Frozen,
			AlreadyPaid: st.Paid, PaidTo: st.PaidTo,
			ReadyToPay: !st.Frozen && !st.Paid && nodeReported && pledgeReleased,
		}
		v.Proofs = append(v.Proofs, proof)
	}
	sort.Slice(v.Proofs, func(i, j int) bool { return v.Proofs[i].BillID < v.Proofs[j].BillID })
	return v
}

// PaymentAudit 是针对一笔放款动作的审计结论。
type PaymentAudit struct {
	PaymentEventID string
	StoredSeq      int
	ControlHolder  string // 放款时点控制权人
	Endorsements   []Endorsement
	TransportNodes []string // 放款时点已知运输事实（含晚到回补）
	LateEventsUsed []string // 放款依据中包含的晚到事件
	Condition      *PaymentCondition
	PayTo          string
	Accepted       bool
	// 放款之前该提单上被拦截的异常：重复回调、旧持有人指令、冻结期操作等。
	RejectedBefore []RuleCode
	ChainIntact    bool
}

// AuditPayment 从任一支付动作回溯：重放到该支付事件入账为止的全部历史，
// 确认当时控制权、运输事实与异常补偿结果，并校验哈希链完整性。
func (s *Store) AuditPayment(paymentEventID string) (PaymentAudit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	paySeq := 0
	for _, rec := range s.records {
		if rec.Event.ID == paymentEventID {
			if rec.Event.Type != EvPaid {
				return PaymentAudit{}, &RuleError{Code: RuleUnknownEvent, Message: "目标事件不是放款: " + paymentEventID}
			}
			paySeq = rec.StoredSeq
		}
	}
	if paySeq == 0 {
		// 可能是一笔被拒绝的放款：仍可审计其拒绝原因。
		for _, r := range s.rejections {
			if r.Event.ID == paymentEventID && r.Event.Type == EvPaid {
				return PaymentAudit{
					PaymentEventID: paymentEventID,
					Accepted:       false,
					RejectedBefore: []RuleCode{r.Code},
					ChainIntact:    s.chainOKLocked(),
				}, nil
			}
		}
		return PaymentAudit{}, &RuleError{Code: RuleBillNotFound, Message: "找不到支付事件: " + paymentEventID}
	}

	// 用前缀重放，重建放款时点的现状，而不是读取今天的现状。
	replay := NewStore()
	for _, rec := range s.records {
		if rec.StoredSeq > paySeq {
			break
		}
		if err := replay.appendLocked(rec.Event); err != nil {
			return PaymentAudit{}, err
		}
	}
	st := replay.states[s.records[paySeq-1].Event.BillID]

	// 沿合单/拆单血缘收集全部祖先，祖先单上的攻击尝试也属于本笔支付的证据。
	lineage := map[string]bool{st.BillID: true}
	queue := append([]string(nil), st.Parents...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if lineage[id] {
			continue
		}
		lineage[id] = true
		if p, ok := replay.states[id]; ok {
			queue = append(queue, p.Parents...)
		}
	}

	// 只把血缘上的晚到回补列为支付依据。
	var lateUsed []string
	for _, rec := range s.records {
		if rec.StoredSeq > paySeq {
			break
		}
		if rec.Late && lineage[rec.Event.BillID] {
			lateUsed = append(lateUsed, rec.Event.ID)
		}
	}

	report := PaymentAudit{
		PaymentEventID: paymentEventID,
		StoredSeq:      paySeq,
		ControlHolder:  st.Holder,
		Endorsements:   append([]Endorsement(nil), st.EndorsementSeq...),
		Condition:      s.records[paySeq-1].Event.Condition,
		PayTo:          s.records[paySeq-1].Event.PayTo,
		Accepted:       true,
		LateEventsUsed: lateUsed,
		ChainIntact:    s.chainOKLocked(),
	}
	for _, n := range st.Transport {
		report.TransportNodes = append(report.TransportNodes, n.Code)
	}
	for _, r := range s.rejections {
		if r.AfterSeq <= paySeq && lineage[r.Event.BillID] {
			report.RejectedBefore = append(report.RejectedBefore, r.Code)
		}
	}
	return report, nil
}

func (s *Store) chainOKLocked() bool {
	prev := ""
	for _, rec := range s.records {
		if rec.PrevHash != prev || rec.Hash != hashRecord(prev, rec.Event) {
			return false
		}
		prev = rec.Hash
	}
	return true
}
