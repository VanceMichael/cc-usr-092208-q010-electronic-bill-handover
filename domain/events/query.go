package events

import "errors"

// State 返回提单现状的副本。
func (s *Store) State(billID string) (BillState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[billID]
	if !ok {
		return BillState{}, false
	}
	return *st, true
}

// Records 返回全部已生效事件（按入账序号），历史只增不改。
func (s *Store) Records() []EventRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EventRecord, len(s.records))
	copy(out, s.records)
	return out
}

// Rejections 返回被规则拒绝的异常台账（含重复回调尝试次数）。
func (s *Store) Rejections() []Rejection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rejection, len(s.rejections))
	copy(out, s.rejections)
	return out
}

// VerifyChain 重放哈希链，任何对历史的覆盖/删除都会导致断链。
func (s *Store) VerifyChain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := ""
	for _, rec := range s.records {
		if rec.PrevHash != prev {
			return errors.New("哈希链断裂于事件 " + rec.Event.ID)
		}
		if rec.Hash != hashRecord(prev, rec.Event) {
			return errors.New("事件内容与哈希不符: " + rec.Event.ID)
		}
		prev = rec.Hash
	}
	return nil
}

// ProposalsOf 返回某参与方相关的待会签提案（作为发起方或受让方）。
func (s *Store) ProposalsOf(party string) []Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Proposal
	for _, p := range s.proposals {
		if p.From == party || p.To == party {
			out = append(out, *p)
		}
	}
	return out
}
