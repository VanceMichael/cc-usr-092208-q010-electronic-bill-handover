package events

import (
	"os"
	"strings"
	"testing"
)

func replayFixture(t *testing.T) (*Store, []EntryResult, Scenario) {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/events.json")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := LoadScenario(raw)
	if err != nil {
		t.Fatal(err)
	}
	store, results, err := sc.Replay()
	if err != nil {
		t.Fatal(err)
	}
	return store, results, sc
}

func TestFixtureExpectations(t *testing.T) {
	_, results, sc := replayFixture(t)
	for i, r := range results {
		if !r.OK {
			t.Errorf("第 %d 条流水 [%s] 期望 %s，实际 %s",
				i+1, sc.Entries[i].Note, r.Expect, r.Got)
		}
	}
}

func TestUniqueCurrentState(t *testing.T) {
	store, _, _ := replayFixture(t)

	bl01, ok := store.State("BL-01")
	if !ok || bl01.Active {
		t.Fatal("拆单后原单 BL-01 应失效但保留历史")
	}
	if bl01.Holder != "buyer" {
		t.Fatalf("BL-01 历史最后持有人应为 buyer，实际 %s", bl01.Holder)
	}
	if len(bl01.EndorsementSeq) != 1 || bl01.EndorsementSeq[0].From != "shipper" ||
		bl01.EndorsementSeq[0].To != "buyer" {
		t.Fatal("背书序列与真实交接不符")
	}

	bl02, ok := store.State("BL-02")
	if !ok || !bl02.Active {
		t.Fatal("合单后的 BL-02 应有效")
	}
	if bl02.Holder != "buyer" {
		t.Fatalf("唯一现状持有人应为 buyer，实际 %s", bl02.Holder)
	}
	if bl02.Batch.Quantity != "100" {
		t.Fatalf("合单数量守恒失败: %s", bl02.Batch.Quantity)
	}
	if !bl02.Paid || bl02.PaidTo != "buyer" {
		t.Fatal("放款状态或收款人错误")
	}
	if bl02.Frozen {
		t.Fatal("解冻后 Frozen 应为 false")
	}
}

func TestLateCarrierEventsDoNotRevertControl(t *testing.T) {
	store, _, _ := replayFixture(t)

	bl02, _ := store.State("BL-02")
	wantNodes := map[string]bool{
		"loaded": true, "discharged": true, "delivered": true, "customs_released": true,
	}
	got := map[string]bool{}
	for _, n := range bl02.Transport {
		got[n.Code] = true
	}
	for code := range wantNodes {
		if !got[code] {
			t.Errorf("运输事实 %s 未沿血缘回补到 BL-02（实际: %v）", code, got)
		}
	}
	// 节点按承运人序号归并，顺序唯一。
	seqs := []int{}
	for _, n := range bl02.Transport {
		seqs = append(seqs, n.Seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("运输节点未按序号归并: %v", seqs)
		}
	}
	// 晚到回补发生多次，持有人始终是买方。
	if bl02.Holder != "buyer" {
		t.Fatal("晚到回补不得改变控制权")
	}
}

func TestOldHolderAttacksRejected(t *testing.T) {
	store, _, _ := replayFixture(t)

	codes := map[RuleCode]bool{}
	for _, r := range store.Rejections() {
		codes[r.Code] = true
	}
	for _, want := range []RuleCode{
		RuleNotHolder, RulePaymentHolder, RuleFrozen, RulePledgeBlock,
		RuleDuplicateCallback, RulePaymentDuplicate, RuleAmendForbidden,
		RuleBillInactive, RulePaymentPledge,
	} {
		if !codes[want] {
			t.Errorf("异常台账缺少应被拦截的规则码: %s", want)
		}
	}
}

func TestOldHolderCannotEndorseAfterTransfer(t *testing.T) {
	// 脱离 fixture 的最小定向验证。
	s := NewStore()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Append(Event{
		ID: "i1", BillID: "B", Type: EvIssued, OccurredAt: "2026-09-01T00:00:00Z",
		Signatures: []Signature{{"carrier", "sig"}}, Issuer: "carrier", InitialHolder: "a",
		Batch: &Batch{CargoCode: "C", Quantity: "10", Unit: "box"},
	}))
	must(s.Append(Event{
		ID: "e1", BillID: "B", Type: EvEndorsed, OccurredAt: "2026-09-02T00:00:00Z",
		Signatures: []Signature{{"a", "sig"}, {"b", "sig"}}, FromHolder: "a", ToHolder: "b",
	}))
	err := s.Append(Event{
		ID: "e2", BillID: "B", Type: EvEndorsed, OccurredAt: "2026-09-03T00:00:00Z",
		Signatures: []Signature{{"a", "sig"}, {"x", "sig"}}, FromHolder: "a", ToHolder: "x",
	})
	if code := errCode(err); code != RuleNotHolder {
		t.Fatalf("旧持有人重复背书应判 not_holder，实际 %v", err)
	}
	st, _ := s.State("B")
	if st.Holder != "b" || len(st.EndorsementSeq) != 1 {
		t.Fatal("攻击失败后现状不得被污染")
	}
}

func TestTwoPartyProposalFlow(t *testing.T) {
	s := NewStore()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Append(Event{
		ID: "i1", BillID: "B", Type: EvIssued, OccurredAt: "2026-09-01T00:00:00Z",
		Signatures: []Signature{{"carrier", "sig"}}, Issuer: "carrier", InitialHolder: "a",
		Batch: &Batch{CargoCode: "C", Quantity: "10", Unit: "box"},
	}))
	p := Proposal{ID: "p1", BillID: "B", From: "a", To: "b", OccurredAt: "2026-09-02T00:00:00Z"}
	must(s.Propose(p, Signature{"a", "sig"}))

	// 会签前设立质押，过期提案必须在会签时失败。
	must(s.Append(Event{
		ID: "g1", BillID: "B", Type: EvPledged, OccurredAt: "2026-09-02T01:00:00Z",
		Signatures: []Signature{{"a", "sig"}, {"bank", "sig"}}, Pledgee: "bank",
	}))
	if err := s.CounterSign("p1", Signature{"b", "sig"}); errCode(err) != RulePledgeBlock {
		t.Fatalf("会签时应按当前现状拦截质押中的转让，实际 %v", err)
	}
	if _, ok := s.State("B"); !ok {
		t.Fatal("提单应仍存在")
	}
	if st, _ := s.State("B"); st.Holder != "a" {
		t.Fatal("会签失败不得改变控制权")
	}
}

func TestSplitConservation(t *testing.T) {
	s := NewStore()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Append(Event{
		ID: "i1", BillID: "B", Type: EvIssued, OccurredAt: "2026-09-01T00:00:00Z",
		Signatures: []Signature{{"carrier", "sig"}}, Issuer: "carrier", InitialHolder: "a",
		Batch: &Batch{CargoCode: "C", Quantity: "10", Unit: "box"},
	}))
	err := s.Append(Event{
		ID: "sp1", BillID: "B", Type: EvSplit, OccurredAt: "2026-09-02T00:00:00Z",
		Signatures: []Signature{{"a", "sig"}, {"carrier", "sig"}},
		Children:   []string{"B1", "B2"},
		Splits:     []BatchAllocation{{"B1", "7"}, {"B2", "4"}}, // 7+4 != 10
	})
	if errCode(err) != RuleSplitQuantity {
		t.Fatalf("拆单数量不守恒应拒绝，实际 %v", err)
	}
}

func TestBankViewLeastPrivilege(t *testing.T) {
	store, _, _ := replayFixture(t)

	bank := store.ViewForBank("bank", "delivered")
	found := false
	for _, p := range bank.Proofs {
		if p.BillID == "BL-02" {
			found = true
			if !p.AlreadyPaid || p.PaidTo != "buyer" {
				t.Fatal("银行应看到本行已放款单的结果")
			}
		}
	}
	if !found {
		t.Fatal("放款后银行应保留对 BL-02 的可见性")
	}

	// 无关银行看不到任何单据。
	if other := store.ViewForBank("other-bank", "delivered"); len(other.Proofs) != 0 {
		t.Fatal("字段/范围白名单失效：无关银行读到了单据")
	}
}

func TestTraderViewScopedRightsAndTodos(t *testing.T) {
	store, _, _ := replayFixture(t)

	shipper := store.ViewForTrader("shipper")
	for _, b := range shipper.Bills {
		if b.IsHolder {
			t.Fatal("shipper 已转出全部权利，不应再是任何提单的当前持有人")
		}
		if b.BillID == "BL-02" {
			t.Fatal("shipper 从未持有过 BL-02，无权看到该单")
		}
	}

	buyer := store.ViewForTrader("buyer")
	if len(buyer.Bills) == 0 {
		t.Fatal("买方应看到自己持有的提单")
	}
	holderSeen := false
	for _, b := range buyer.Bills {
		if b.BillID == "BL-02" && b.IsHolder {
			holderSeen = true
		}
	}
	if !holderSeen {
		t.Fatal("买方视图应显示 BL-02 的当前控制权")
	}
}

func TestAuditPaymentReconstructsPointInTime(t *testing.T) {
	store, _, _ := replayFixture(t)

	audit, err := store.AuditPayment("ev-24")
	if err != nil {
		t.Fatal(err)
	}
	if !audit.Accepted {
		t.Fatal("ev-24 应是一笔成功放款")
	}
	if audit.ControlHolder != "buyer" || audit.PayTo != "buyer" {
		t.Fatalf("放款时点控制权错误: holder=%s payTo=%s", audit.ControlHolder, audit.PayTo)
	}
	nodeSet := map[string]bool{}
	for _, n := range audit.TransportNodes {
		nodeSet[n] = true
	}
	if !nodeSet["delivered"] {
		t.Fatal("支付依据的交付事实未在审计结论中")
	}
	wantReject := map[RuleCode]bool{
		RuleNotHolder: true, RulePaymentHolder: true, RulePledgeBlock: true,
		RuleAmendForbidden: true, RulePaymentPledge: true,
	}
	got := map[RuleCode]bool{}
	for _, c := range audit.RejectedBefore {
		got[c] = true
	}
	for c := range wantReject {
		if !got[c] {
			t.Errorf("审计未确认支付前异常补偿结果: %s（实际 %v）", c, audit.RejectedBefore)
		}
	}
	if len(audit.LateEventsUsed) == 0 {
		t.Fatal("审计应列出放款依据中归并的晚到事件")
	}
	if !audit.ChainIntact {
		t.Fatal("哈希链校验失败")
	}
	if err := store.VerifyChain(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditRejectedPayment(t *testing.T) {
	store, _, _ := replayFixture(t)
	audit, err := store.AuditPayment("ev-27")
	if err != nil {
		t.Fatal(err)
	}
	if audit.Accepted {
		t.Fatal("ev-27 是被拒绝的伪造放款，审计应标记未生效")
	}
	if len(audit.RejectedBefore) != 1 || audit.RejectedBefore[0] != RulePaymentHolder {
		t.Fatalf("被拒绝支付的审计结论错误: %v", audit.RejectedBefore)
	}
}

func TestIdempotentCallbackTally(t *testing.T) {
	store, _, _ := replayFixture(t)
	dupes := 0
	for _, r := range store.Rejections() {
		if r.Code == RuleDuplicateCallback {
			dupes++
		}
	}
	if dupes < 2 {
		t.Fatalf("至少应有运输重复回调与支付重复回调两条记录，实际 %d", dupes)
	}
	bl02, _ := store.State("BL-02")
	if !bl02.Paid {
		t.Fatal("重复支付回调被拦截后，首笔放款状态应保持")
	}
	recs := store.Records()
	paid := 0
	for _, r := range recs {
		if r.Event.Type == EvPaid && strings.HasPrefix(r.Event.BillID, "BL-02") {
			paid++
		}
	}
	if paid != 1 {
		t.Fatalf("BL-02 只能有一笔生效放款，实际 %d", paid)
	}
}

func errCode(err error) RuleCode {
	if err == nil {
		return ""
	}
	if re, ok := err.(*RuleError); ok {
		return re.Code
	}
	return RuleCode("non-rule-error: " + err.Error())
}
