// Package events 用追加式事件日志描述电子提单控制权交接。
//
// 事件一经生效即不可覆盖、不可删除：任何“改单”都通过追加更正事件实现，
// 历史始终完整。现状由事件按“发生时间 + 入账序号”折叠得到，因此承运人
// 离线后回补的晚到事件只会归入正确的历史位置，不会撤销已经生效的控制权
// 转让，也不会让旧持有人重新获得放款或背书的权利。
package events

import "time"

// 角色标识。系统只认角色，不保存任何真实身份信息。
const (
	RoleCarrier  = "carrier"  // 承运人：签发、运输节点上报、改单
	RolePlatform = "platform" // 提单平台运营方：拆单/合单登记、争议冻结
	RoleBank     = "bank"     // 贸易融资银行：质押登记、放款
	RoleTrader   = "trader"   // 进出口企业：背书转让的任意一方
)

// 事件类型。每一种状态变化都对应且仅对应一种事件。
const (
	// EvIssued 承运人签发电子提单。
	EvIssued = "bill.issued"
	// EvEndorsed 当前持有人背书转让给新持有人，须双方签名。
	EvEndorsed = "bill.endorsed"
	// EvTransport 承运人上报运输节点（装船、到港、卸货……）。允许晚到回补。
	EvTransport = "transport.reported"
	// EvAmended 承运人改单：以更正后字段覆盖部分单据字段，原值保留在事件里。
	EvAmended = "bill.amended"
	// EvSplit 拆单：一份提单按数量拆成多份，数量守恒，记录血缘。
	EvSplit = "bill.split"
	// EvMerged 合单：多份提单合成一份，数量守恒，记录血缘。
	EvMerged = "bill.merged"
	// EvPledged 持有人将提单质押给银行。
	EvPledged = "bill.pledged"
	// EvReleased 银行解除质押。
	EvReleased = "pledge.released"
	// EvFrozen 争议冻结：冻结期间禁止转让与放款。
	EvFrozen = "bill.frozen"
	// EvUnfrozen 解除冻结。
	EvUnfrozen = "bill.unfrozen"
	// EvPaid 银行定向放款。携带支付条件快照，供事后审计。
	EvPaid = "payment.released"
)

// Signature 是参与方对事件的签名示意（仓库中不使用任何真实密码学材料）。
type Signature struct {
	Party string `json:"party"` // 角色或化名，如 carrier、trader:buyer
	Sig   string `json:"sig"`   // 占位签名摘要，禁止存放真实密钥
}

// Event 是事件日志里不可覆盖的一条记录。
type Event struct {
	ID         string      `json:"id"`          // 事件唯一标识，重复即拒绝
	BillID     string      `json:"bill_id"`     // 本事件作用的电子提单
	Type       string      `json:"type"`        // 事件类型
	OccurredAt string      `json:"occurred_at"` // 业务发生时间（RFC3339），晚到事件可能早于已入账事件
	Signatures []Signature `json:"signatures"`  // 双方签名（运输/质押/支付等按规则要求的双方）

	// IdempotencyKey 供外部系统重复回调时去重；同一提单下同键事件视为重复投递。
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// 各事件类型的载荷，按类型取用，未使用字段留空。
	Issuer        string            `json:"issuer,omitempty"`         // EvIssued
	InitialHolder string            `json:"initial_holder,omitempty"` // EvIssued 签发后的首个控制权人（托运方）
	FromHolder    string            `json:"from_holder,omitempty"`    // EvEndorsed
	ToHolder      string            `json:"to_holder,omitempty"`      // EvEndorsed
	Batch         *Batch            `json:"batch,omitempty"`          // EvIssued 初始批次
	Node          *TransportNode    `json:"node,omitempty"`           // EvTransport
	Amend         *Amendment        `json:"amend,omitempty"`          // EvAmended
	Children      []string          `json:"children,omitempty"`       // EvSplit 拆出的新提单
	Splits        []BatchAllocation `json:"splits,omitempty"`         // EvSplit 数量分配
	Parents       []string          `json:"parents,omitempty"`        // EvMerged 来源提单
	Pledgee       string            `json:"pledgee,omitempty"`        // EvPledged
	PayTo         string            `json:"pay_to,omitempty"`         // EvPaid 定向收款方
	Amount        string            `json:"amount,omitempty"`         // EvPaid 金额（示意字符串）
	Condition     *PaymentCondition `json:"condition,omitempty"`      // EvPaid 放款时校验的条件
	Reason        string            `json:"reason,omitempty"`         // EvFrozen/EvAmended 说明
}

// Batch 描述货物批次与数量。数量用字符串表示十进制数，示意资料不做真实计量。
type Batch struct {
	CargoCode string `json:"cargo_code"` // 货物标识
	Quantity  string `json:"quantity"`   // 数量
	Unit      string `json:"unit"`
}

// BatchAllocation 是拆单时一份子提单分得的批次数量。
type BatchAllocation struct {
	BillID   string `json:"bill_id"`
	Quantity string `json:"quantity"`
}

// TransportNode 是一个运输节点事实。
type TransportNode struct {
	Seq    int    `json:"seq"`  // 承运人侧的节点序号，用于识别乱序/回补
	Code   string `json:"code"` // 节点代码，如 loaded、discharged
	Place  string `json:"place"`
	AtTime string `json:"at_time"` // 该节点实际发生时间
}

// Amendment 是改单内容：只允许更正单据字段，不得触碰持有人与批次数量。
type Amendment struct {
	Fields map[string]string `json:"fields"` // 更正后的字段（如目的港、通知方）
}

// PaymentCondition 是放款必须满足的条件，由贸易合同/信用证约定。
type PaymentCondition struct {
	RequiredHolder        string `json:"required_holder"`         // 放款时控制权必须归属
	RequiredNode          string `json:"required_node"`           // 必须已上报的运输节点
	RequirePledgeReleased bool   `json:"require_pledge_released"` // 是否要求无未解除质押
}

// BillState 是一份电子提单折叠后的唯一现状。
type BillState struct {
	BillID           string
	Issuer           string
	Holder           string // 当前控制权人（唯一现状的核心字段）
	Batch            Batch
	Parents          []string        // 合单血缘
	Children         []string        // 拆单血缘（父单拆出后作废）
	Active           bool            // 是否仍为有效提单（拆单后父单失效）
	Transport        []TransportNode // 按节点序号归并后的完整运输事实
	AmendedFields    map[string]string
	AmendmentHistory []Amendment
	Pledgee          string // 非空表示质押中
	Frozen           bool
	Paid             bool
	PaidTo           string
	Financier        string        // 实际放款银行，放款后仍保留对该单的可见性
	EndorsementSeq   []Endorsement // 完整背书序列
}

// Endorsement 记录一次控制权交接。
type Endorsement struct {
	From       string
	To         string
	EventID    string
	OccurredAt string
}

// EventRecord 是已生效事件及其入账元数据。
type EventRecord struct {
	Event     Event
	StoredSeq int       // 入账序号（单调递增，决定折叠顺序中的平局）
	StoredAt  time.Time // 入账时间
	Late      bool      // 是否为晚到回补（发生时间早于同单已入账事件）
	PrevHash  string    // 上一条记录的哈希
	Hash      string    // 本条记录哈希：H(PrevHash || 事件规范 JSON)，链断即被篡改
}

// Rejection 记录一次被规则拒绝的入账尝试（例如旧持有人重复回调），
// 拒绝本身也留痕，供审计确认异常补偿结果。
type Rejection struct {
	Event    Event
	Code     RuleCode
	At       time.Time
	AfterSeq int // 拒绝发生时日志末尾的入账序号，用于按支付时点截取
	Attempts int // 同一幂等键第几次重复尝试
}

// RuleCode 标识被拒绝或被补偿的具体规则，便于审计按码检索。
type RuleCode string

const (
	RuleUnknownEvent      RuleCode = "unknown_event"
	RuleDuplicateEvent    RuleCode = "duplicate_event"
	RuleDuplicateCallback RuleCode = "duplicate_callback"
	RuleMissingSignature  RuleCode = "missing_signature"
	RuleBillNotFound      RuleCode = "bill_not_found"
	RuleBillInactive      RuleCode = "bill_inactive"
	RuleNotHolder         RuleCode = "not_holder"
	RuleFrozen            RuleCode = "frozen"
	RulePledgeBlock       RuleCode = "pledge_block"
	RuleBadEndorsement    RuleCode = "bad_endorsement"
	RuleSelfEndorsement   RuleCode = "self_endorsement"
	RuleNodeOutOfRange    RuleCode = "node_seq_out_of_range"
	RuleAmendForbidden    RuleCode = "amend_forbidden_field"
	RuleSplitQuantity     RuleCode = "split_quantity_mismatch"
	RuleSplitKnownChild   RuleCode = "split_known_child"
	RuleMergeInactive     RuleCode = "merge_inactive_parent"
	RuleMergeQuantity     RuleCode = "merge_quantity_mismatch"
	RuleMergeOverlap      RuleCode = "merge_overlap"
	RulePledgeState       RuleCode = "pledge_state"
	RulePaymentHolder     RuleCode = "payment_holder_mismatch"
	RulePaymentNode       RuleCode = "payment_node_missing"
	RulePaymentPledge     RuleCode = "payment_pledge_held"
	RulePaymentDuplicate  RuleCode = "payment_duplicate"
)

// RuleError 携带规则码，调用方可以据此区分拒绝原因。
type RuleError struct {
	Code    RuleCode
	Message string
}

func (e *RuleError) Error() string { return string(e.Code) + ": " + e.Message }

func ruleError(code RuleCode, msg string) *RuleError {
	return &RuleError{Code: code, Message: msg}
}
