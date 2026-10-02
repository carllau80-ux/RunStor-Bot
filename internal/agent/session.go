package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-restore-agent/internal/fileio"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// Session 是一次多轮对话的状态。
type Session struct {
	ID      string            // 会话标识
	Kind    string            // backup | restore
	Text    string            // 原始需求文本 (界面始终回显这一段)
	Answers map[string]string // 已答槽位: slot -> 用户原话
	Asked   map[string]string // 已问槽位: slot -> 最近一次的问题文案
	QAPairs []llm.QAPair      // 累计的槽位问答

	// FollowUps 是用户在对话里补充的自由文本。
	//
	// 与 Answers 的区别: Answers 是"对某个具体槽位的精确回答", 走确定性回填;
	// FollowUps 是"随口补充的一段话" (如"再改成保留30天"), 没有槽位归属, 只作为
	// 追加的需求文本交给抽取流程理解。两条路并存, 对话式界面才不会退化成
	// "必须逐格填空"。
	FollowUps []string

	Plan   string // 最近一次的计划 JSON (草稿)
	Status string // 最近一次回合的状态
	// SubmittedAt 是用户点"提交"定稿的时间; 零值表示未提交。
	// 之后再修改计划会清空它 —— 交付物变了, 原来的确认就失效了。
	SubmittedAt time.Time
	Questions   []schema.Question
	Transcript  []Record
	Turns       int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Record 是对话记录里的一条, 供界面重新渲染历史。
//
// 刻意不存整份计划: 计划由 Session.Plan 单独保存, 每条记录都带一份会让会话文件
// 迅速膨胀。这里只保留渲染一轮对话真正需要的字段。
type Record struct {
	At     time.Time `json:"at"`
	Role   string    `json:"role"` // user | agent
	Text   string    `json:"text,omitempty"`
	Status string    `json:"status,omitempty"`
	Kind   string    `json:"kind,omitempty"`

	Answers    map[string]string `json:"answers,omitempty"`
	Questions  []schema.Question `json:"questions,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
	Notices    []string          `json:"notices,omitempty"`
	Errors     []string          `json:"errors,omitempty"`
	Confidence float64           `json:"confidence,omitempty"`
	Turns      int               `json:"turns,omitempty"`
	LLM        string            `json:"llm,omitempty"`
}

// 记录角色。
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Title 返回会话在侧栏里显示的名字: 取需求首行的前若干字。
func (s *Session) Title() string {
	text := strings.TrimSpace(s.Text)
	if text == "" {
		return "(空需求)"
	}
	if index := strings.IndexAny(text, "\n\r"); index != -1 {
		text = text[:index]
	}
	return truncateRunes(strings.TrimSpace(text), 24)
}

// QuestionRef 是记录到会话里的问题摘要。
type QuestionRef struct {
	Slot string
	Text string
}

// Ask 记录一次追问, 便于下一轮引用问题原文。
func (s *Session) Ask(questions []QuestionRef) {
	for _, question := range questions {
		s.Asked[question.Slot] = question.Text
	}
}

// Append 追加一条对话记录。
func (s *Session) Append(record Record) {
	if record.At.IsZero() {
		record.At = time.Now()
	}
	s.Transcript = append(s.Transcript, record)
}

// Requirement 返回送进抽取流程的需求文本: 原始需求 + 用户后续补充的自由文本。
func (s *Session) Requirement() string {
	if len(s.FollowUps) == 0 {
		return s.Text
	}
	return s.Text + "\n" + strings.Join(s.FollowUps, "\n")
}

// Clone 返回会话的深拷贝。
//
// 为什么需要它: Store 里的会话是**发布后即只读**的 —— 列表、详情、界面渲染都直接读
// 那个对象, 不取锁。所以任何修改都必须在副本上做, 结束时整体发布 (见 Store.Publish)。
// 一旦有人在已发布的对象上就地改 map, 并发的读就会撞上
// "concurrent map read and map write" —— 那是直接崩进程, 不是可忽略的告警。
//
// 必须深拷贝到嵌套的 map/slice: 浅拷贝会让副本与原件共享底层数组, 改副本等于改原件。
func (s *Session) Clone() *Session {
	if s == nil {
		return nil
	}
	next := *s
	next.Answers = copyStringMap(s.Answers)
	next.Asked = copyStringMap(s.Asked)
	next.QAPairs = append([]llm.QAPair(nil), s.QAPairs...)
	next.FollowUps = append([]string(nil), s.FollowUps...)
	next.Questions = copyQuestions(s.Questions)
	next.Transcript = make([]Record, len(s.Transcript))
	for index, record := range s.Transcript {
		next.Transcript[index] = record.Clone()
	}
	return &next
}

// Clone 深拷贝一条对话记录。
func (r Record) Clone() Record {
	next := r
	next.Answers = copyStringMap(r.Answers)
	next.Questions = copyQuestions(r.Questions)
	next.Warnings = append([]string(nil), r.Warnings...)
	next.Notices = append([]string(nil), r.Notices...)
	next.Errors = append([]string(nil), r.Errors...)
	return next
}

func copyStringMap(source map[string]string) map[string]string {
	if source == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func copyQuestions(source []schema.Question) []schema.Question {
	if source == nil {
		return nil
	}
	out := make([]schema.Question, len(source))
	for index, question := range source {
		question.Choices = append([]string(nil), question.Choices...)
		question.Suggestions = append([]string(nil), question.Suggestions...)
		out[index] = question
	}
	return out
}

// ---------------------------------------------------------------------------
// 落盘形态
// ---------------------------------------------------------------------------

type persistedSession struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Text        string            `json:"text"`
	FollowUps   []string          `json:"follow_ups,omitempty"`
	Answers     map[string]string `json:"answers,omitempty"`
	Asked       map[string]string `json:"asked,omitempty"`
	QAPairs     []llm.QAPair      `json:"qa_pairs,omitempty"`
	Plan        string            `json:"plan,omitempty"`
	Status      string            `json:"status,omitempty"`
	Questions   []schema.Question `json:"questions,omitempty"`
	Transcript  []Record          `json:"transcript,omitempty"`
	Turns       int               `json:"turns"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	SubmittedAt time.Time         `json:"submitted_at,omitempty"`
}

type persistedStore struct {
	Sessions []persistedSession `json:"sessions"`
}

func (s *Session) toPersisted() persistedSession {
	return persistedSession{
		ID: s.ID, Kind: s.Kind, Text: s.Text,
		FollowUps: s.FollowUps, Answers: s.Answers, Asked: s.Asked,
		QAPairs: s.QAPairs, Plan: s.Plan, Status: s.Status,
		Questions: s.Questions, Transcript: s.Transcript, Turns: s.Turns,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		SubmittedAt: s.SubmittedAt,
	}
}

func (p persistedSession) toSession() *Session {
	session := &Session{
		ID: p.ID, Kind: p.Kind, Text: p.Text,
		FollowUps: p.FollowUps, Answers: p.Answers, Asked: p.Asked,
		QAPairs: p.QAPairs, Plan: p.Plan, Status: p.Status,
		Questions: p.Questions, Transcript: p.Transcript, Turns: p.Turns,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		SubmittedAt: p.SubmittedAt,
	}
	if session.Answers == nil {
		session.Answers = map[string]string{}
	}
	if session.Asked == nil {
		session.Asked = map[string]string{}
	}
	if session.QAPairs == nil {
		session.QAPairs = []llm.QAPair{}
	}
	if session.Transcript == nil {
		session.Transcript = []Record{}
	}
	return session
}

// StoreOptions 是会话存储的参数。
type StoreOptions struct {
	TTL         time.Duration
	Path        string // 落盘位置; 空表示仅内存
	MaxSessions int
}

// Store 是会话存储: 内存为主, 可选落盘。
//
// 为什么默认落盘: 界面有"历史对话"侧栏, 若进程一重启历史就空, 那个侧栏等于没用。
// 落盘文件与运行态配置同在 StateDirectory 下 (0600, 属服务用户)。
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	ttl      time.Duration
	path     string
	max      int

	// locks 是"每会话一把"的锁, 见 LockSession。
	//
	// 单独一张表而不是把锁塞进 Session: Session 是要被 JSON 序列化的结构,
	// 往里加 sync.Mutex 会让它不可复制、也让落盘格式多出无意义的字段。
	locks map[string]*sync.Mutex
}

// NewStore 构造会话存储, 并尝试载入已有的历史。
func NewStore(options StoreOptions) *Store {
	store := &Store{
		sessions: map[string]*Session{},
		locks:    map[string]*sync.Mutex{},
		ttl:      options.TTL,
		path:     options.Path,
		max:      options.MaxSessions,
	}
	if store.ttl <= 0 {
		store.ttl = 2 * time.Hour
	}
	if store.max <= 0 {
		store.max = 30
	}
	store.load()
	return store
}

// 锁表 (Store.locks) 的条目不随会话删除而回收: 条数上界就是 MaxSessions, 而回收反而
// 危险 —— 持有者还在用旧 mutex 时新请求会拿到一把新的, 于是两把锁同时"保护"同一个会话。

// Path 返回落盘位置 (空表示仅内存)。
func (s *Store) Path() string { return s.path }

// load 载入历史。载入失败直接忽略 —— 会话历史坏掉不该让服务起不来。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	// 上限按"会话数 × 单条上限"给: 自己的文件也要有界, 否则一个损坏的大文件
	// 会在启动时把内存吃光。单条会话的正文通常几十 KB, 这里给足余量。
	limit := int64(s.max) * (2 << 20)
	raw, err := fileio.ReadBounded(s.path, limit)
	if err != nil {
		return
	}
	var data persistedStore
	if err := json.Unmarshal(raw, &data); err != nil {
		return
	}
	for _, entry := range data.Sessions {
		session := entry.toSession()
		if session.ID == "" {
			continue
		}
		s.sessions[session.ID] = session
	}
}

// Create 新建会话。
func (s *Store) Create(kind, text string) *Session {
	now := time.Now()
	session := &Session{
		ID:         newSessionID(),
		Kind:       kind,
		Text:       text,
		Answers:    map[string]string{},
		Asked:      map[string]string{},
		QAPairs:    []llm.QAPair{},
		Transcript: []Record{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
	s.persistLocked()
	return session
}

// TryLockSession 尝试取得会话的独占权 (非阻塞)。
//
// 返回: release 解锁函数 (ok 为 true 时有效) / ok 表示会话存在 / busy 表示已有回合在处理。
//
// 为什么非阻塞: 一轮问答里要调模型 (秒级), 阻塞排队会让界面卡住且没有任何反馈。
// 并发打同一个会话 (双击提交、两个标签页、界面重试) 时明确回一句"正在处理"更诚实,
// 也避免两个回合各自基于同一份旧状态算完再互相覆盖 (丢更新)。
func (s *Store) TryLockSession(id string) (release func(), ok bool, busy bool) {
	s.mu.Lock()
	if _, exists := s.sessions[id]; !exists {
		s.mu.Unlock()
		return nil, false, false
	}
	lock, exists := s.locks[id]
	if !exists {
		lock = &sync.Mutex{}
		s.locks[id] = lock
	}
	s.mu.Unlock()

	// 必须在释放 s.mu **之后**加锁: 反了就是锁序颠倒
	// (session 锁 -> store 锁 与 store 锁 -> session 锁 并存), 迟早死锁。
	if !lock.TryLock() {
		return nil, true, true
	}
	return lock.Unlock, true, false
}

// Get 取会话; 超过 TTL 未活动的视为过期。
//
// **返回的对象只读**: 调用方要改就在 Clone 的副本上改, 完成后用 Publish 整体发布。
// 就地改会让并发的读者撞上 concurrent map read/write。
func (s *Store) Get(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	if time.Since(session.UpdatedAt) > s.ttl {
		return nil, false
	}
	return session, true
}

// Publish 整体发布会话的新版本, 并刷新活跃时间与落盘。
//
// 会话已被删除或回收时返回 false —— 调用方据此知道这轮结果没人要了,
// 而不是把它"复活"回来 (那会让界面出现一条用户已经删掉的会话)。
func (s *Store) Publish(session *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[session.ID]; !exists {
		return false
	}
	session.UpdatedAt = time.Now()
	s.sessions[session.ID] = session
	s.persistLocked()
	return true
}

// List 返回全部会话, 按最近活动倒序。
func (s *Store) List() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedByUpdatedLocked()
}

// Delete 删除一个会话。
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	s.persistLocked()
	return true
}

// Reset 重置会话为初始需求, 清空已答内容与对话记录。
func (s *Store) Reset(id string) (*Session, bool) {
	s.mu.Lock()
	old, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	return s.Create(old.Kind, old.Text), true
}

// Count 返回当前会话数。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// persistLocked 落盘。调用方需持有锁。
//
// 失败不再是"静默返回": 内存里的会话已经变了而盘上没有, 后果是"重启后这段对话不见了"。
// 这件事必须留下痕迹 —— 只说"落盘失败"运维不知道要不要处理, 说清后果才知道。
func (s *Store) persistLocked() {
	if s.path == "" {
		return
	}

	// 超出上限时丢掉最久未活动的会话, 让文件大小有界
	keep := s.sortedByUpdatedLocked()
	if len(keep) > s.max {
		for _, stale := range keep[s.max:] {
			delete(s.sessions, stale.ID)
		}
		keep = keep[:s.max]
	}

	data := persistedStore{Sessions: make([]persistedSession, 0, len(keep))}
	for _, session := range keep {
		data.Sessions = append(data.Sessions, session.toPersisted())
	}

	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("警告: 对话历史序列化失败: %v; 这次的会话变更没有落盘", err)
		return
	}
	body = append(body, '\n')

	if err := fileio.WriteAtomic(s.path, body, 0o600); err != nil {
		log.Printf("警告: 对话历史落盘失败 (%s): %v; 重启后这些会话会丢失", s.path, err)
	}
}

func (s *Store) sortedByUpdatedLocked() []*Session {
	out := make([]*Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		out = append(out, session)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func newSessionID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		// 极端情况下退化为时间戳, 仍然可用
		return "s-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	}
	return "s-" + hex.EncodeToString(buffer)
}

// truncateRunes 按字符截断, 避免切坏中文。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// Summary 是给侧栏用的会话摘要。
func (s *Session) Summary() map[string]any {
	return map[string]any{
		"id":         s.ID,
		"title":      s.Title(),
		"kind":       s.Kind,
		"status":     s.Status,
		"turns":      s.Turns,
		"pending":    len(s.Questions),
		"submitted":  !s.SubmittedAt.IsZero(),
		"created_at": s.CreatedAt.Format(time.RFC3339),
		"updated_at": s.UpdatedAt.Format(time.RFC3339),
	}
}

// Detail 是会话详情 (供界面重建整段对话)。
func (s *Session) Detail() map[string]any {
	questions := s.Questions
	if questions == nil {
		questions = []schema.Question{}
	}
	transcript := s.Transcript
	if transcript == nil {
		transcript = []Record{}
	}
	followUps := s.FollowUps
	if followUps == nil {
		followUps = []string{}
	}

	var plan any
	if strings.TrimSpace(s.Plan) != "" {
		_ = json.Unmarshal([]byte(s.Plan), &plan)
	}

	// 总结不在这里算: 它需要运行时清单 (介质服务器名等), 而 Session 不该依赖
	// 外部服务。由 Agent.SessionDetail 注入。
	return map[string]any{
		"id":           s.ID,
		"kind":         s.Kind,
		"title":        s.Title(),
		"text":         s.Text,
		"follow_ups":   followUps,
		"status":       s.Status,
		"turns":        s.Turns,
		"questions":    questions,
		"transcript":   transcript,
		"plan":         plan,
		"submitted_at": submittedAtText(s.SubmittedAt),
		"answers":      s.Answers,
		"created_at":   s.CreatedAt.Format(time.RFC3339),
		"updated_at":   s.UpdatedAt.Format(time.RFC3339),
	}
}

// submittedAtText 把定稿时间格式化成 RFC3339; 未提交返回空串。
func submittedAtText(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}
