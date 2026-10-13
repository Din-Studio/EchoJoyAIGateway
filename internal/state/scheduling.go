package state

import (
	"cmp"
	"math/bits"
	"math/rand/v2"
	"sync"
	"time"
)

// SchedulingProgress 使用固定 128 位坐标，避免长期运行时浮点小步长失效。
// 整数部分每次最多加一；百万次分配/秒也需要超过五十万年才耗尽范围。
type SchedulingProgress struct {
	Whole    uint64 `json:"whole"`
	Fraction uint64 `json:"fraction"`
}

func (p SchedulingProgress) Compare(other SchedulingProgress) int {
	if order := cmp.Compare(p.Whole, other.Whole); order != 0 {
		return order
	}
	return cmp.Compare(p.Fraction, other.Fraction)
}

func (p SchedulingProgress) Advance(weight uint64) (SchedulingProgress, bool) {
	if weight == 0 {
		return p, false
	}
	var carry uint64
	if weight == 1 {
		carry = 1
	} else {
		step, _ := bits.Div64(1, 0, weight)
		p.Fraction, carry = bits.Add64(p.Fraction, step, 0)
	}
	p.Whole, carry = bits.Add64(p.Whole, carry, 0)
	return p, carry == 0
}

type SchedulingMember struct {
	ID                 uint
	GroupID            uint
	IdentityGeneration uint64
	Progress           SchedulingProgress
	LastSelected       uint64
	Pending            bool
	suspended          bool
	staggered          bool
	cooldownUntil      time.Time
}

func (m *SchedulingMember) Admit(baseline SchedulingProgress) {
	if m.Progress.Compare(baseline) < 0 {
		m.Progress = baseline
	}
	// 中途加入与恢复的成员必须精确对齐领先进度，不再随机错开。
	m.Pending, m.suspended, m.staggered = false, false, true
}

// SchedulingLedger 只在 SchedulingState.WithLock 回调内访问。
// 凭据事实属于 Registry；本表独立保存分配历史，不随凭据配置替换而回退。
type SchedulingLedger struct {
	Members       map[uint]*SchedulingMember
	Groups        map[uint]bool
	GroupRevision uint64
	GroupsKnown   bool
	Started       bool
	Watermark     SchedulingProgress
	Sequence      uint64
	LastMember    uint
	Consecutive   uint64
	// jitter 返回 [0, n) 内的随机数；nil 表示不错开，保持确定性分配顺序。
	jitter func(n uint64) uint64
}

// Stagger 给启动时已存在的成员一次步长内的随机起点：账本按副本独立，
// 同起点会让各副本按相同顺序选中同一凭据。偏移小于一个步长，长期比例不变。
func (d *SchedulingLedger) Stagger(member *SchedulingMember, weight uint64) {
	if d.jitter == nil || member.staggered || member.Pending || weight == 0 {
		return
	}
	step := ^uint64(0)
	if weight > 1 {
		step, _ = bits.Div64(1, 0, weight)
	}
	var carry uint64
	member.Progress.Fraction, carry = bits.Add64(member.Progress.Fraction, d.jitter(step), 0)
	member.Progress.Whole += carry
	member.staggered = true
}

type SchedulingState struct {
	mu     sync.Mutex
	ledger SchedulingLedger
}

func NewSchedulingState() *SchedulingState {
	return &SchedulingState{ledger: SchedulingLedger{
		Members: make(map[uint]*SchedulingMember), Groups: make(map[uint]bool),
	}}
}

// NewStaggeredSchedulingState 用于多副本生产装配，各副本从随机相位开始轮询。
func NewStaggeredSchedulingState() *SchedulingState {
	s := NewSchedulingState()
	s.ledger.jitter = rand.Uint64N
	return s
}

func (s *SchedulingState) WithLock(fn func(*SchedulingLedger)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.ledger)
}

func (s *SchedulingState) syncCredentialLocked(view CredentialRuntimeView) {
	d := &s.ledger
	m := d.Members[view.ID]
	if m == nil || m.GroupID != view.GroupID || m.IdentityGeneration != view.IdentityGeneration {
		// 启动先发布分组再加载凭据，尚未分配也要保留停用组的恢复边界。
		m = &SchedulingMember{ID: view.ID, GroupID: view.GroupID,
			IdentityGeneration: view.IdentityGeneration,
			Pending:            d.Started || d.GroupsKnown && !d.Groups[view.GroupID]}
		d.Members[view.ID] = m
		if d.LastMember == view.ID {
			d.LastMember, d.Consecutive = 0, 0
		}
	}
	authUnavailable := view.AuthState != CredentialAuthStateReady &&
		view.AuthState != "" && view.AuthState != CredentialAuthStateRefreshing
	suspended := view.Status != CredentialStatusActive || view.Blacklisted || authUnavailable ||
		view.WeightManual != nil && *view.WeightManual <= 0 || view.CooldownUntil.After(time.Now())
	// 记录冷却事件本身，避免到期与下一次选择之间没有管理更新而丢失恢复边界。
	if suspended || m.suspended || !view.CooldownUntil.IsZero() && !view.CooldownUntil.Equal(m.cooldownUntil) {
		m.Pending = true
	}
	m.suspended, m.cooldownUntil = suspended, view.CooldownUntil
}

func (s *SchedulingState) SyncCredential(view CredentialRuntimeView) {
	s.WithLock(func(*SchedulingLedger) { s.syncCredentialLocked(view) })
}

// SyncCredentials 对齐成员集合；groupID=0 表示完整替换，其他值只协调该分组。
func (s *SchedulingState) SyncCredentials(groupID uint, views []CredentialRuntimeView) {
	s.WithLock(func(d *SchedulingLedger) {
		seen := make(map[uint]struct{}, len(views))
		for _, view := range views {
			seen[view.ID] = struct{}{}
			s.syncCredentialLocked(view)
		}
		for id, member := range d.Members {
			if groupID != 0 && member.GroupID != groupID {
				continue
			}
			if _, exists := seen[id]; !exists {
				s.removeLocked(id)
			}
		}
	})
}

func (s *SchedulingState) Remove(id uint) {
	s.WithLock(func(*SchedulingLedger) { s.removeLocked(id) })
}

func (s *SchedulingState) removeLocked(id uint) {
	delete(s.ledger.Members, id)
	if s.ledger.LastMember == id {
		s.ledger.LastMember, s.ledger.Consecutive = 0, 0
	}
}

// SyncGroups 由配置发布路径调用，避免两次请求之间禁用再启用时漏掉校准。
func (s *SchedulingState) SyncGroups(snapshot *ConfigSnapshot) {
	if snapshot == nil {
		return
	}
	s.WithLock(func(d *SchedulingLedger) {
		if d.GroupsKnown && snapshot.Revision <= d.GroupRevision {
			return
		}
		groups := make(map[uint]bool, len(snapshot.GroupCatalog))
		for id, group := range snapshot.GroupCatalog {
			// 与路由编译保持一致：空模型分组整体暂停，普通候选变化仍保留历史。
			groups[id] = group.Enabled && len(snapshot.Groups[id].Models) > 0 &&
				(group.WeightManual == nil || *group.WeightManual > 0)
		}
		for _, member := range d.Members {
			if !groups[member.GroupID] {
				member.Pending = true
			}
		}
		d.Groups, d.GroupRevision, d.GroupsKnown = groups, snapshot.Revision, true
	})
}

func (r *CredentialRegistry) SchedulingState() *SchedulingState { return r.scheduling }

func (r *CredentialRegistry) syncSchedulingGroupLocked(groupID uint) {
	views := make([]CredentialRuntimeView, 0, len(r.buckets[groupID]))
	for _, entry := range r.buckets[groupID] {
		views = append(views, runtimeView(entry))
	}
	r.scheduling.SyncCredentials(groupID, views)
}

// WithCredentialCandidates 固定本次凭据身份与权重，回调中只允许内存调度。
// 锁顺序为 Registry 读锁 -> SchedulingState；不得反向获取 Registry 锁。
func (r *CredentialRegistry) WithCredentialCandidates(groupIDs []uint, excluded func(uint) bool,
	now time.Time, fn func([]CredentialMeta),
) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn(r.collectCredentialCandidatesLocked(groupIDs, excluded, now))
}
