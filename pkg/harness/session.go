// Package harness 提供会话持久化与压缩（对应 pi 的 harness + session-manager）。
// 会话是 append-only 的 JSONL 文件，entry 通过 parentId 构成树；
// Branch 只是移动 leaf 指针，旧路径不删除。
package harness

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// EntryType 是会话 entry 类型。
type EntryType string

const (
	EntrySession        EntryType = "session" // 文件头
	EntryMessage        EntryType = "message" // 一条 AgentMessage
	EntryModelChange    EntryType = "model_change"
	EntryThinkingChange EntryType = "thinking_level_change"
	EntryCompaction     EntryType = "compaction"     // 压缩摘要
	EntryBranchSummary  EntryType = "branch_summary" // 分叉时给被放弃路径留的摘要
	EntryCustom         EntryType = "custom"
)

// Entry 是 JSONL 里的一行（对应 pi 的 SessionEntry）。
type Entry struct {
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"` // 根为 ""
	Type      EntryType       `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// MessagePayload 是 EntryMessage 的载荷。
type MessagePayload struct {
	Message agent.AgentMessage `json:"message"`
}

// CompactionPayload 是 EntryCompaction 的载荷（对应 pi 的 CompactionEntry）。
type CompactionPayload struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int    `json:"tokensBefore"`
}

// BranchSummaryPayload 是 EntryBranchSummary 的载荷。
type BranchSummaryPayload struct {
	Summary string `json:"summary"`
	FromID  string `json:"fromId"`
}

// HeaderPayload 是 EntrySession 文件头的载荷。
type HeaderPayload struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	CWD     string `json:"cwd"`
}

// SessionVersion 是当前会话格式版本。
const SessionVersion = 1

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Session 是 append-only 的树状会话存储（对应 pi 的 SessionManager）。
type Session struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	byID   map[string]*Entry
	leafID string // 当前分支末端；"" 表示空
	header HeaderPayload
}

// CreateSession 在 dir 下新建一个会话文件。
func CreateSession(dir, cwd string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	header := HeaderPayload{Version: SessionVersion, ID: newID(), CWD: cwd}
	path := filepath.Join(dir, header.ID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Session{path: path, file: f, byID: map[string]*Entry{}, header: header}
	payload, _ := json.Marshal(header)
	if err := s.appendEntry(&Entry{ID: "root", ParentID: "", Type: EntrySession, Timestamp: nowISO(), Payload: payload}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// OpenSession 打开并重放一个已有会话文件。
func OpenSession(path string) (*Session, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Session{path: path, file: f, byID: map[string]*Entry{}}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // 容忍损坏行（pi 同样宽松）
		}
		entry := e
		s.byID[e.ID] = &entry
		if e.Type == EntrySession {
			var h HeaderPayload
			_ = json.Unmarshal(e.Payload, &h)
			s.header = h
		}
		if e.ID != "root" {
			s.leafID = e.ID // 最后一行即 leaf（append-only）
		}
	}
	if err := scanner.Err(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// Path 返回会话文件路径。
func (s *Session) Path() string { return s.path }

// ID 返回会话 ID。
func (s *Session) ID() string { return s.header.ID }

// LeafID 返回当前 leaf entry ID。
func (s *Session) LeafID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leafID
}

// appendEntry 追加一行并更新树索引（调用方需持锁或处于初始化期）。
func (s *Session) appendEntry(e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := s.file.Write(append(b, '\n')); err != nil {
		return err
	}
	s.byID[e.ID] = e
	if e.Type != EntrySession {
		s.leafID = e.ID
	}
	return nil
}

// AppendMessage 追加一条消息 entry。
func (s *Session) AppendMessage(m agent.AgentMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := json.Marshal(MessagePayload{Message: m})
	if err != nil {
		return "", err
	}
	parent := s.leafID
	if parent == "" {
		parent = "root"
	}
	e := &Entry{ID: newID(), ParentID: parent, Type: EntryMessage, Timestamp: nowISO(), Payload: payload}
	return e.ID, s.appendEntry(e)
}

// AppendCustom 追加自定义 entry。
func (s *Session) AppendCustom(t EntryType, payload any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	parent := s.leafID
	if parent == "" {
		parent = "root"
	}
	e := &Entry{ID: newID(), ParentID: parent, Type: t, Timestamp: nowISO(), Payload: p}
	return e.ID, s.appendEntry(e)
}

// Branch 把 leaf 移回历史节点（对应 pi 的 branch()）。
// 之后 append 的消息会成为该节点的子节点，形成新分支。
func (s *Session) Branch(fromID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fromID != "root" {
		if _, ok := s.byID[fromID]; !ok {
			return fmt.Errorf("entry %s not found", fromID)
		}
	}
	s.leafID = fromID
	return nil
}

// BranchWithSummary 分叉并给被放弃的路径留摘要（对应 pi 的 branchWithSummary()）。
func (s *Session) BranchWithSummary(fromID, summary string) error {
	if fromID == "" {
		fromID = "root"
	}
	oldLeaf := s.LeafID()
	if err := s.Branch(fromID); err != nil {
		return err
	}
	_, err := s.AppendCustom(EntryBranchSummary, BranchSummaryPayload{Summary: summary, FromID: oldLeaf})
	return err
}

// pathToRoot 从 leaf 沿 parentId 回溯到根，返回根→叶顺序的 entry 链。
func (s *Session) pathFromRoot() []*Entry {
	if s.leafID == "" {
		return nil
	}
	var rev []*Entry
	cur := s.leafID
	for cur != "" && cur != "root" {
		e, ok := s.byID[cur]
		if !ok {
			break
		}
		rev = append(rev, e)
		cur = e.ParentID
	}
	out := make([]*Entry, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// Entries 返回根到 leaf 的完整 entry 链。
func (s *Session) Entries() []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pathFromRoot()
}

type messageRef struct {
	entryID string
	message agent.AgentMessage
}

// Messages 重建当前分支的消息序列（对应 pi 的 buildSessionContext）。
// 遇到 compaction entry 时用摘要替换被压缩区段。
func (s *Session) Messages() ([]agent.AgentMessage, error) {
	refs, err := s.messageRefs()
	if err != nil {
		return nil, err
	}
	var msgs []agent.AgentMessage
	for _, ref := range refs {
		msgs = append(msgs, ref.message)
	}
	return msgs, nil
}

func (s *Session) messageRefs() ([]messageRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var refs []messageRef
	for _, e := range s.pathFromRoot() {
		switch e.Type {
		case EntryMessage:
			var p MessagePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, err
			}
			refs = append(refs, messageRef{entryID: e.ID, message: p.Message})
		case EntryCompaction:
			var p CompactionPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, err
			}
			summary := messageRef{entryID: e.ID, message: agent.AgentMessage{Message: compactionSummaryMessage(p.Summary)}}
			if p.FirstKeptEntryID == "" {
				refs = []messageRef{summary}
				continue
			}
			firstKept := -1
			for i := range refs {
				if refs[i].entryID == p.FirstKeptEntryID {
					firstKept = i
					break
				}
			}
			if firstKept < 0 {
				// 旧格式或损坏的 entry 没有可用边界时保留上下文，避免静默丢消息。
				refs = append(refs, summary)
				continue
			}
			kept := append([]messageRef(nil), refs[firstKept:]...)
			refs = append([]messageRef{summary}, kept...)
		case EntryBranchSummary:
			var p BranchSummaryPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, err
			}
			refs = append(refs, messageRef{entryID: e.ID, message: agent.AgentMessage{Message: branchSummaryMessage(p.Summary)}})
		}
	}
	return refs, nil
}

// compactionSummaryMessage 把压缩摘要包装成一条 user 消息注入上下文
// （对应 pi 的 createCompactionSummaryMessage）。
func compactionSummaryMessage(summary string) llm.Message {
	return llm.NewUserMessage("[Context compaction] Earlier conversation was summarized:\n\n" + summary)
}

// branchSummaryMessage 把分叉摘要包装成 user 消息（对应 pi 的 createBranchSummaryMessage）。
func branchSummaryMessage(summary string) llm.Message {
	return llm.NewUserMessage("[Branch summary] An earlier branch of this session was summarized:\n\n" + summary)
}

// Close 关闭会话文件。
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}
