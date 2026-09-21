package events

import (
	"encoding/json"
	"errors"
)

// ErrBadScenario 表示场景资料缺少必要字段。
var ErrBadScenario = errors.New("场景资料缺少必要字段")

// ScenarioEntry 是场景资料中的一条流水：event 为待入账事件，
// Expect 为期望结果——"accepted" 或具体规则码。被拒绝的攻击尝试
// 同样写进资料，证明系统确实拦截并留痕，而不是把异常静默丢弃。
type ScenarioEntry struct {
	Expect string `json:"expect"`
	Note   string `json:"note,omitempty"`
	Event  Event  `json:"event"`
}

// Scenario 是一份可回放的全生命周期场景资料。
type Scenario struct {
	Domain  string          `json:"domain"`
	Version int             `json:"version"`
	Sample  string          `json:"sample_id"`
	Entries []ScenarioEntry `json:"entries"`
}

// EntryResult 是单条流水的实际入账结果。
type EntryResult struct {
	Expect string
	Got    string // "accepted" 或规则码
	OK     bool
}

// LoadScenario 解码 JSON 场景。
func LoadScenario(raw []byte) (Scenario, error) {
	var sc Scenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		return Scenario{}, err
	}
	if sc.Domain == "" || sc.Version < 1 || len(sc.Entries) == 0 {
		return Scenario{}, ErrBadScenario
	}
	return sc, nil
}

// Replay 按顺序把场景流水追加进新日志，逐条返回实际结果。
func (sc Scenario) Replay() (*Store, []EntryResult, error) {
	store := NewStore()
	results := make([]EntryResult, 0, len(sc.Entries))
	for _, en := range sc.Entries {
		err := store.Append(en.Event)
		got := "accepted"
		if err != nil {
			if re, ok := err.(*RuleError); ok {
				got = string(re.Code)
			} else {
				return nil, nil, err
			}
		}
		results = append(results, EntryResult{Expect: en.Expect, Got: got, OK: got == en.Expect})
	}
	return store, results, nil
}
