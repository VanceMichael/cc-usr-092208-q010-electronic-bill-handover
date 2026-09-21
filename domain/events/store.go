package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proposal 是一方发起、等待对方会签的转让意向。它尚未进入事件日志，
// 不改变控制权；会签成功时才追加 EvEndorsed。会签前提单若被质押或
// 冻结，会签将按当时现状被拒绝，从而杜绝过期指令生效。
type Proposal struct {
	ID         string
	BillID     string
	From       string
	To         string
	OccurredAt string
	FromSig    Signature
}

// Store 是追加式事件日志及其折叠出的各提单现状。
type Store struct {
	mu         sync.Mutex
	seq        int
	records    []EventRecord
	rejections []Rejection
	byID       map[string]int        // 事件 ID -> 入账序号
	idem       map[string]string     // 提单|幂等键 -> 已生效事件 ID
	attempts   map[string]int        // 幂等键重复尝试次数
	states     map[string]*BillState // 每份提单的唯一现状
	maxOccur   map[string]time.Time  // 提单最近业务发生时间，用于识别晚到
	proposals  map[string]*Proposal
	lastHash   string
}

// NewStore 创建空日志。
func NewStore() *Store {
	return &Store{
		byID:      map[string]int{},
		idem:      map[string]string{},
		attempts:  map[string]int{},
		states:    map[string]*BillState{},
		maxOccur:  map[string]time.Time{},
		proposals: map[string]*Proposal{},
	}
}

// Append 校验并追加一条事件。校验失败时事件不留入正文，但会记入拒绝台账，
// 返回 *RuleError 说明规则码。事件生效后不可覆盖、不可删除。
func (s *Store) Append(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(e)
}

func (s *Store) appendLocked(e Event) error {
	if e.ID == "" || e.BillID == "" {
		return s.reject(e, RuleUnknownEvent, "事件缺少标识")
	}
	if _, known := eventTypes[e.Type]; !known {
		return s.reject(e, RuleUnknownEvent, "未知事件类型: "+e.Type)
	}
	if _, dup := s.byID[e.ID]; dup {
		return s.reject(e, RuleDuplicateEvent, "事件标识重复: "+e.ID)
	}
	occ, err := time.Parse(time.RFC3339, e.OccurredAt)
	if err != nil {
		return s.reject(e, RuleUnknownEvent, "发生时间不是 RFC3339: "+e.OccurredAt)
	}
	if key := e.IdempotencyKey; key != "" {
		k := e.BillID + "|" + key
		if _, seen := s.idem[k]; seen {
			s.attempts[k]++
			s.rejections = append(s.rejections, Rejection{
				Event: e, Code: RuleDuplicateCallback, At: time.Now(), AfterSeq: s.seq, Attempts: s.attempts[k],
			})
			return &RuleError{Code: RuleDuplicateCallback, Message: "重复回调，幂等键: " + key}
		}
	}

	createsBill := e.Type == EvIssued || e.Type == EvMerged
	var st *BillState
	if createsBill {
		if _, exists := s.states[e.BillID]; exists {
			return s.reject(e, RuleBillInactive, "提单标识已存在: "+e.BillID)
		}
	} else {
		var ok bool
		st, ok = s.states[e.BillID]
		if !ok {
			return s.reject(e, RuleBillNotFound, "提单不存在: "+e.BillID)
		}
		if !st.Active {
			// 已拆单/合单的原单只接受承运人晚到回补的运输事实，
			// 该事实会沿血缘传播给仍有效的后代提单；其余操作一律拒绝。
			if e.Type != EvTransport {
				return s.reject(e, RuleBillInactive, "提单已失效（拆单/合单后）: "+e.BillID)
			}
		}
	}

	if err := s.validate(e, st, occ); err != nil {
		return err // validate 已记录拒绝台账
	}

	// 校验通过：生效、折叠进现状、挂哈希链。
	s.seq++
	rec := EventRecord{Event: e, StoredSeq: s.seq, StoredAt: time.Now()}
	// 业务生效时间：运输事件取节点实际发生时间，其余取事件发生时间。
	// 承运人离线回补时，节点发生时间早于水位线，即标记为晚到。
	eff := occ
	if e.Type == EvTransport && e.Node != nil {
		if at, perr := time.Parse(time.RFC3339, e.Node.AtTime); perr == nil {
			eff = at
		}
	}
	if last, ok := s.maxOccur[e.BillID]; ok && eff.Before(last) {
		rec.Late = true
	}
	rec.PrevHash = s.lastHash
	rec.Hash = hashRecord(rec.PrevHash, e)
	s.lastHash = rec.Hash

	s.records = append(s.records, rec)
	s.byID[e.ID] = s.seq
	if e.IdempotencyKey != "" {
		s.idem[e.BillID+"|"+e.IdempotencyKey] = e.ID
	}
	if cur, ok := s.maxOccur[e.BillID]; !ok || eff.After(cur) {
		s.maxOccur[e.BillID] = eff
	}
	s.apply(e, st)
	return nil
}

var eventTypes = map[string]bool{
	EvIssued: true, EvEndorsed: true, EvTransport: true, EvAmended: true,
	EvSplit: true, EvMerged: true, EvPledged: true, EvReleased: true,
	EvFrozen: true, EvUnfrozen: true, EvPaid: true,
}

func (s *Store) reject(e Event, code RuleCode, msg string) error {
	s.rejections = append(s.rejections, Rejection{Event: e, Code: code, At: time.Now(), AfterSeq: s.seq})
	return &RuleError{Code: code, Message: msg}
}

func hasSig(e Event, party string) bool {
	for _, sg := range e.Signatures {
		if sg.Party == party && sg.Sig != "" {
			return true
		}
	}
	return false
}

func (s *Store) validate(e Event, st *BillState, occ time.Time) error {
	switch e.Type {
	case EvIssued:
		if e.Issuer == "" || !hasSig(e, e.Issuer) {
			return s.reject(e, RuleMissingSignature, "签发须由签发方签名")
		}
		if e.InitialHolder == "" {
			return s.reject(e, RuleBadEndorsement, "签发缺少首个控制权人")
		}
		if e.Batch == nil || e.Batch.CargoCode == "" {
			return s.reject(e, RuleUnknownEvent, "签发缺少货物批次")
		}
		if _, err := parseQty(e.Batch.Quantity); err != nil {
			return s.reject(e, RuleSplitQuantity, "批次数量非法: "+e.Batch.Quantity)
		}

	case EvEndorsed:
		if !hasSig(e, e.FromHolder) || !hasSig(e, e.ToHolder) {
			return s.reject(e, RuleMissingSignature, "转让须转让双方签名")
		}
		if e.FromHolder == "" || e.ToHolder == "" {
			return s.reject(e, RuleBadEndorsement, "背书双方不能为空")
		}
		if e.FromHolder == e.ToHolder {
			return s.reject(e, RuleSelfEndorsement, "不能向自己背书")
		}
		// 核心防线：只有“入账当时”的持有人才可背书。旧持有人在货权转移后
		// 无论重发多旧的回调，都会在这里被拒绝。
		if st.Holder != e.FromHolder {
			return s.reject(e, RuleNotHolder, "转让方不是当前持有人: "+e.FromHolder)
		}
		if st.Frozen {
			return s.reject(e, RuleFrozen, "提单争议冻结中，禁止转让")
		}
		if st.Pledgee != "" {
			return s.reject(e, RulePledgeBlock, "提单质押中，须先解除质押")
		}
		if len(st.EndorsementSeq) > 0 {
			last := st.EndorsementSeq[len(st.EndorsementSeq)-1]
			if lt, err := time.Parse(time.RFC3339, last.OccurredAt); err == nil && occ.Before(lt) {
				return s.reject(e, RuleBadEndorsement, "背书发生时间早于现有持有链，禁止插队")
			}
		}

	case EvTransport:
		if !hasSig(e, RoleCarrier) {
			return s.reject(e, RuleMissingSignature, "运输节点须由承运人签名")
		}
		if e.Node == nil || e.Node.Seq < 1 || e.Node.Code == "" {
			return s.reject(e, RuleNodeOutOfRange, "运输节点缺少序号或代码")
		}
		// 原单失效时，回补事实要与所有有效后代一起查重，避免重复传播。
		targets := []*BillState{st}
		if !st.Active {
			targets = s.activeDescendants(st.BillID)
		}
		for _, t := range targets {
			for _, n := range t.Transport {
				if n.Seq == e.Node.Seq {
					if n.Code == e.Node.Code && n.AtTime == e.Node.AtTime && n.Place == e.Node.Place {
						return s.reject(e, RuleDuplicateCallback, "运输节点重复上报")
					}
					return s.reject(e, RuleNodeOutOfRange, "同一节点序号内容冲突")
				}
			}
		}

	case EvAmended:
		if !hasSig(e, RoleCarrier) {
			return s.reject(e, RuleMissingSignature, "改单须由承运人签名")
		}
		if st.Frozen {
			return s.reject(e, RuleFrozen, "争议冻结中不得改单")
		}
		if e.Amend == nil || len(e.Amend.Fields) == 0 {
			return s.reject(e, RuleUnknownEvent, "改单内容为空")
		}
		for k := range e.Amend.Fields {
			if k == "holder" || k == "quantity" || k == "cargo_code" {
				return s.reject(e, RuleAmendForbidden, "改单不得触碰控制权或批次核心字段: "+k)
			}
		}

	case EvSplit:
		if !hasSig(e, st.Holder) || !hasSig(e, RoleCarrier) {
			return s.reject(e, RuleMissingSignature, "拆单须持有人与承运人共同签名")
		}
		if st.Frozen {
			return s.reject(e, RuleFrozen, "争议冻结中不得拆单")
		}
		if len(e.Children) == 0 || len(e.Children) != len(e.Splits) {
			return s.reject(e, RuleSplitQuantity, "拆单子提单与分配数量不一致")
		}
		seen := map[string]bool{}
		total := new(big.Rat)
		for _, a := range e.Splits {
			if seen[a.BillID] {
				return s.reject(e, RuleSplitQuantity, "拆单分配重复: "+a.BillID)
			}
			seen[a.BillID] = true
			q, err := parseQty(a.Quantity)
			if err != nil {
				return s.reject(e, RuleSplitQuantity, "拆单数量非法: "+a.Quantity)
			}
			total.Add(total, q)
		}
		for _, c := range e.Children {
			if _, exists := s.states[c]; exists {
				return s.reject(e, RuleSplitKnownChild, "子提单标识已存在: "+c)
			}
			if !seen[c] {
				return s.reject(e, RuleSplitQuantity, "子提单缺少数量分配: "+c)
			}
		}
		if total.Cmp(mustQty(st.Batch.Quantity)) != 0 {
			return s.reject(e, RuleSplitQuantity, "拆单数量之和不等于原批次，数量不守恒")
		}

	case EvMerged:
		if len(e.Parents) < 2 {
			return s.reject(e, RuleMergeQuantity, "合单至少需要两份提单")
		}
		parents := make([]*BillState, 0, len(e.Parents))
		seenP := map[string]bool{}
		holder := ""
		for _, pid := range e.Parents {
			if seenP[pid] {
				return s.reject(e, RuleMergeOverlap, "合单来源重复: "+pid)
			}
			seenP[pid] = true
			p, ok := s.states[pid]
			if !ok {
				return s.reject(e, RuleBillNotFound, "合单来源不存在: "+pid)
			}
			if !p.Active {
				return s.reject(e, RuleMergeInactive, "合单来源已失效: "+pid)
			}
			if holder == "" {
				holder = p.Holder
			} else if p.Holder != holder {
				return s.reject(e, RuleBadEndorsement, "合单来源控制权人不一致")
			}
			if p.Frozen {
				return s.reject(e, RuleFrozen, "合单来源处于冻结中: "+pid)
			}
			parents = append(parents, p)
		}
		if !hasSig(e, holder) || !hasSig(e, RoleCarrier) {
			return s.reject(e, RuleMissingSignature, "合单须共同持有人与承运人签名")
		}
		pledgee := parents[0].Pledgee
		for _, p := range parents[1:] {
			if p.Pledgee != pledgee {
				return s.reject(e, RulePledgeState, "合单来源质押状态不一致")
			}
		}
		total := new(big.Rat)
		cargo := parents[0].Batch.CargoCode
		unit := parents[0].Batch.Unit
		for _, p := range parents {
			if p.Batch.CargoCode != cargo || p.Batch.Unit != unit {
				return s.reject(e, RuleMergeQuantity, "合单来源货物批次不一致")
			}
			total.Add(total, mustQty(p.Batch.Quantity))
		}
		if e.Batch == nil {
			return s.reject(e, RuleMergeQuantity, "合单缺少结果批次")
		}
		if got, err := parseQty(e.Batch.Quantity); err != nil || got.Cmp(total) != 0 {
			return s.reject(e, RuleMergeQuantity, "合单数量不等于来源之和，数量不守恒")
		}

	case EvPledged:
		if !hasSig(e, st.Holder) || !hasSig(e, e.Pledgee) {
			return s.reject(e, RuleMissingSignature, "质押须持有人与质权人共同签名")
		}
		if e.Pledgee == "" {
			return s.reject(e, RulePledgeState, "质权人为空")
		}
		if st.Pledgee != "" {
			return s.reject(e, RulePledgeState, "提单已处于质押中")
		}
		if st.Frozen {
			return s.reject(e, RuleFrozen, "争议冻结中不得设立质押")
		}

	case EvReleased:
		if !hasSig(e, st.Pledgee) {
			return s.reject(e, RuleMissingSignature, "解除质押须质权人签名")
		}
		if st.Pledgee == "" {
			return s.reject(e, RulePledgeState, "提单未处于质押中")
		}

	case EvFrozen, EvUnfrozen:
		if !hasSig(e, RolePlatform) {
			return s.reject(e, RuleMissingSignature, "冻结/解冻须平台运营方签名")
		}
		if e.Type == EvFrozen && st.Frozen {
			return s.reject(e, RuleFrozen, "提单已处于冻结中")
		}
		if e.Type == EvUnfrozen && !st.Frozen {
			return s.reject(e, RuleFrozen, "提单未处于冻结中")
		}

	case EvPaid:
		if !hasSig(e, RoleBank) {
			return s.reject(e, RuleMissingSignature, "放款须银行签名")
		}
		c := e.Condition
		if c == nil {
			return s.reject(e, RulePaymentHolder, "放款缺少支付条件快照")
		}
		// 先核对控制权：伪造收款人的旧持有人指令即便撞在已放款单上，
		// 也要报控制权不符而不是被“重复放款”掩盖攻击性质。
		if c.RequiredHolder != st.Holder || e.PayTo != st.Holder {
			return s.reject(e, RulePaymentHolder, "放款时控制权与支付条件不一致")
		}
		if st.Frozen {
			return s.reject(e, RuleFrozen, "争议冻结中禁止放款")
		}
		if st.Paid {
			return s.reject(e, RulePaymentDuplicate, "该提单已放款，重复回调不得再次放款")
		}
		if c.RequiredNode != "" {
			found := false
			for _, n := range st.Transport {
				if n.Code == c.RequiredNode {
					found = true
					break
				}
			}
			if !found {
				return s.reject(e, RulePaymentNode, "支付条件要求的运输事实尚未发生/回补: "+c.RequiredNode)
			}
		}
		if c.RequirePledgeReleased && st.Pledgee != "" {
			return s.reject(e, RulePaymentPledge, "质押尚未解除，不得放款")
		}
	}
	return nil
}

// apply 把已生效事件折叠进现状。
func (s *Store) apply(e Event, st *BillState) {
	switch e.Type {
	case EvIssued:
		s.states[e.BillID] = &BillState{
			BillID: e.BillID, Issuer: e.Issuer, Holder: e.InitialHolder,
			Batch:         *e.Batch,
			Active:        true,
			AmendedFields: map[string]string{},
		}

	case EvEndorsed:
		st.Holder = e.ToHolder
		st.EndorsementSeq = append(st.EndorsementSeq, Endorsement{
			From: e.FromHolder, To: e.ToHolder, EventID: e.ID, OccurredAt: e.OccurredAt,
		})

	case EvTransport:
		// 原单已失效时，节点并入其全部有效后代（拆出的子单或合单后的新单）。
		targets := []*BillState{st}
		if !st.Active {
			targets = s.activeDescendants(st.BillID)
		}
		for _, t := range targets {
			t.Transport = append(t.Transport, *e.Node)
			sort.Slice(t.Transport, func(i, j int) bool { return t.Transport[i].Seq < t.Transport[j].Seq })
		}

	case EvAmended:
		if st.AmendedFields == nil {
			st.AmendedFields = map[string]string{}
		}
		for k, v := range e.Amend.Fields {
			st.AmendedFields[k] = v
		}
		st.AmendmentHistory = append(st.AmendmentHistory, *e.Amend)

	case EvSplit:
		alloc := map[string]string{}
		for _, a := range e.Splits {
			alloc[a.BillID] = a.Quantity
		}
		st.Active = false
		st.Children = append(st.Children, e.Children...)
		for _, cid := range e.Children {
			child := &BillState{
				BillID: cid, Issuer: st.Issuer, Holder: st.Holder,
				Batch:         Batch{CargoCode: st.Batch.CargoCode, Quantity: alloc[cid], Unit: st.Batch.Unit},
				Parents:       []string{e.BillID},
				Active:        true,
				Transport:     append([]TransportNode(nil), st.Transport...),
				AmendedFields: cloneFields(st.AmendedFields),
				Pledgee:       st.Pledgee, // 质权随货延续，拆单不能解除银行权利
				Financier:     st.Financier,
				Frozen:        st.Frozen,
			}
			s.states[cid] = child
			s.maxOccur[cid] = s.maxOccur[e.BillID]
		}

	case EvMerged:
		parents := make([]string, 0, len(e.Parents))
		holder, pledgee, financier := "", "", ""
		nodes := map[string]TransportNode{}
		for _, pid := range e.Parents {
			p := s.states[pid]
			parents = append(parents, pid)
			holder = p.Holder
			pledgee = p.Pledgee
			financier = p.Financier
			p.Active = false
			p.Children = append(p.Children, e.BillID)
			for _, n := range p.Transport {
				nodes[nodeKey(n)] = n
			}
		}
		merged := &BillState{
			BillID: e.BillID, Issuer: s.states[e.Parents[0]].Issuer, Holder: holder,
			Batch:         *e.Batch,
			Parents:       parents,
			Active:        true,
			AmendedFields: map[string]string{},
			Pledgee:       pledgee,
			Financier:     financier,
		}
		for _, n := range nodes {
			merged.Transport = append(merged.Transport, n)
		}
		sort.Slice(merged.Transport, func(i, j int) bool { return merged.Transport[i].Seq < merged.Transport[j].Seq })
		s.states[e.BillID] = merged

	case EvPledged:
		st.Pledgee = e.Pledgee
	case EvReleased:
		st.Pledgee = ""
	case EvFrozen:
		st.Frozen = true
	case EvUnfrozen:
		st.Frozen = false
	case EvPaid:
		st.Paid = true
		st.PaidTo = e.PayTo
		st.Financier = RoleBank
	}
}

func nodeKey(n TransportNode) string { return n.Code + "@" + n.AtTime }

// activeDescendants 沿拆单/合单血缘找出当前仍有效的后代提单。
func (s *Store) activeDescendants(billID string) []*BillState {
	var out []*BillState
	queue := append([]string(nil), s.states[billID].Children...)
	seen := map[string]bool{billID: true}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		d, ok := s.states[id]
		if !ok {
			continue
		}
		if d.Active {
			out = append(out, d)
			continue
		}
		queue = append(queue, d.Children...)
	}
	return out
}

func cloneFields(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Propose 发起一笔等待会签的转让，仅记入待办，不改变控制权。
func (s *Store) Propose(p Proposal, sig Signature) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" || p.BillID == "" {
		return errors.New("提案缺少标识")
	}
	if _, dup := s.proposals[p.ID]; dup {
		return errors.New("提案标识重复")
	}
	st, ok := s.states[p.BillID]
	if !ok {
		return &RuleError{Code: RuleBillNotFound, Message: p.BillID}
	}
	if p.From != st.Holder {
		return &RuleError{Code: RuleNotHolder, Message: "提案发起方不是当前持有人"}
	}
	if p.From == p.To {
		return &RuleError{Code: RuleSelfEndorsement, Message: "不能向自己背书"}
	}
	if sig.Party != p.From || sig.Sig == "" {
		return &RuleError{Code: RuleMissingSignature, Message: "提案须发起方签名"}
	}
	p.FromSig = sig
	s.proposals[p.ID] = &p
	return nil
}

// CounterSign 由受让方会签提案；会签时按当前现状重新校验，
// 期间发生的质押、冻结或控制权变化都会让旧提案失效。
func (s *Store) CounterSign(proposalID string, sig Signature) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[proposalID]
	if !ok {
		return errors.New("提案不存在或已处理")
	}
	if sig.Party != p.To || sig.Sig == "" {
		return &RuleError{Code: RuleMissingSignature, Message: "会签须受让方签名"}
	}
	err := s.appendLocked(Event{
		ID:             "end-" + p.ID,
		BillID:         p.BillID,
		Type:           EvEndorsed,
		OccurredAt:     p.OccurredAt,
		Signatures:     []Signature{p.FromSig, sig},
		FromHolder:     p.From,
		ToHolder:       p.To,
		IdempotencyKey: "proposal-" + p.ID,
	})
	if err != nil {
		return err
	}
	delete(s.proposals, proposalID)
	return nil
}

// WithdrawProposal 撤回待会签提案。
func (s *Store) Withdraw(proposalID, party string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proposals[proposalID]
	if !ok {
		return errors.New("提案不存在或已处理")
	}
	if party != p.From {
		return &RuleError{Code: RuleMissingSignature, Message: "只有发起方可撤回"}
	}
	delete(s.proposals, proposalID)
	return nil
}

func hashRecord(prev string, e Event) string {
	canonical, _ := json.Marshal(e)
	sum := sha256.Sum256([]byte(prev + "\n" + string(canonical)))
	return hex.EncodeToString(sum[:])
}

func parseQty(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() <= 0 {
		return nil, errors.New("非法数量: " + s)
	}
	return r, nil
}

func mustQty(s string) *big.Rat {
	r, err := parseQty(s)
	if err != nil {
		panic(err)
	}
	return r
}
