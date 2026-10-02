package agent

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backup-restore-agent/internal/schema"
)

// 会话并发模型的三条不变量, 这里逐条盯住:
//
//  1. 已发布的会话对象对外只读 —— 改只能在 Clone 的副本上改, 否则并发的读会撞上
//     "concurrent map read and map write", 那是直接崩进程。
//  2. 同一会话同时只能有一个回合在跑 —— 否则两个回合各自基于同一份旧状态算完再互相
//     覆盖, 结果是"刚刚答的那一项在历史里不见了"。
//  3. 同一个会话的并发请求不排队等模型 —— 直接回"正在处理", 界面才不用卡好几秒。

func TestTryLockSessionSerializesOneSession(t *testing.T) {
	store := NewStore(StoreOptions{TTL: time.Minute})
	session := store.Create("backup", "全备份")

	release, exists, busy := store.TryLockSession(session.ID)
	if !exists || busy {
		t.Fatalf("首次取锁应成功: exists=%v busy=%v", exists, busy)
	}

	// 同一会话第二次取锁: 会话存在, 但已经有人在处理
	if _, exists, busy := store.TryLockSession(session.ID); !exists || !busy {
		t.Fatalf("已持有锁时应报 busy: exists=%v busy=%v", exists, busy)
	}

	release()
	if _, _, busy := store.TryLockSession(session.ID); busy {
		t.Fatal("释放后应能再次取到锁")
	}
}

func TestTryLockSessionOnMissingSession(t *testing.T) {
	store := NewStore(StoreOptions{TTL: time.Minute})
	if _, exists, busy := store.TryLockSession("no-such-id"); exists || busy {
		t.Errorf("不存在的会话不该给出锁: exists=%v busy=%v", exists, busy)
	}
}

// Clone 必须是深拷贝: 浅拷贝会让副本与已发布的对象共享底层 map/切片,
// 于是"在副本上改"等于就地改已发布的对象 —— 这个测试就是防它。
func TestCloneIsDeep(t *testing.T) {
	store := NewStore(StoreOptions{TTL: time.Minute})
	original := store.Create("backup", "全备份")
	original.Answers["schedule.full.time"] = "01:00"
	original.FollowUps = append(original.FollowUps, "先这样")
	original.Questions = append(original.Questions, schema.Question{
		Slot: "s", Question: "q", Kind: "time",
		Choices: []string{"候选一"}, Suggestions: []string{"建议一"},
	})
	original.Append(Record{Role: RoleUser, Text: "原始记录", Answers: map[string]string{"a": "1"}})

	clone := original.Clone()
	clone.Answers["schedule.full.time"] = "改过了"
	clone.Answers["new"] = "新键"
	clone.FollowUps = append(clone.FollowUps, "再补一句")
	clone.Questions[0].Choices[0] = "改过的候选"
	clone.Transcript[0].Answers["a"] = "改过了"
	clone.Transcript = append(clone.Transcript, Record{Role: RoleAgent})

	stored, ok := store.Get(original.ID)
	if !ok {
		t.Fatal("会话应仍在")
	}
	if got := stored.Answers["schedule.full.time"]; got != "01:00" {
		t.Errorf("改副本影响了原件 (Answers): %q", got)
	}
	if _, exists := stored.Answers["new"]; exists {
		t.Error("改副本给原件加了新键 (map 共享)")
	}
	if len(stored.FollowUps) != 1 {
		t.Errorf("改副本影响了原件 (FollowUps): %v", stored.FollowUps)
	}
	if len(stored.Questions) != 1 || stored.Questions[0].Choices[0] == "改过的候选" {
		t.Error("改副本影响了原件 (Questions 里的切片共享)")
	}
	if got := stored.Transcript[0].Answers["a"]; got != "1" {
		t.Errorf("改副本影响了原件 (Record.Answers): %q", got)
	}
	if len(stored.Transcript) != 1 {
		t.Errorf("改副本影响了原件 (Transcript): %d 条", len(stored.Transcript))
	}
}

func TestPublishDoesNotResurrectDeletedSession(t *testing.T) {
	store := NewStore(StoreOptions{TTL: time.Minute})
	session := store.Create("backup", "全备份")
	clone := session.Clone()

	if !store.Delete(session.ID) {
		t.Fatal("删除应成功")
	}
	if store.Publish(clone) {
		t.Error("已删除的会话不该被复活")
	}
	if _, ok := store.Get(session.ID); ok {
		t.Error("会话应仍是删除状态")
	}
}

// 并发打同一个会话: 成功的回合数 == 历史里多出来的补充条数 (没有丢更新),
// 其余请求得到明确的 ErrSessionBusy 而不是排队或报 500。
//
// 这个用例要配合 -race 跑: 它同时压写路径 (Continue) 与读路径 (详情/列表),
// 修复前这里会出现 "concurrent map read and map write" 或数据竞争报告。
func TestConcurrentTurnsOnSameSession(t *testing.T) {
	agent := newAgentWithStore(t, "")
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	sessionID := result.SessionID

	const writers = 8
	var busyCount atomic.Int64
	var successCount atomic.Int64
	var waitGroup sync.WaitGroup

	for index := 0; index < writers; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			_, err := agent.Continue(sessionID, "补充"+string(rune('A'+index)))
			switch {
			case err == nil:
				successCount.Add(1)
			case errors.Is(err, ErrSessionBusy):
				busyCount.Add(1)
			default:
				t.Errorf("并发补充应只可能是成功或 busy, 实际: %v", err)
			}
		}(index)
	}

	// 同时不断读: 读接口若拿到正在被就地改写的会话, 会在这里爆出来
	stop := make(chan struct{})
	var reader sync.WaitGroup
	for index := 0; index < 3; index++ {
		reader.Add(1)
		go func() {
			defer reader.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := agent.SessionDetail(sessionID); !ok {
					t.Error("会话不该在并发读的时候消失")
					return
				}
				_ = agent.ListSessions()
			}
		}()
	}

	waitGroup.Wait()
	close(stop)
	reader.Wait()

	detail, ok := agent.SessionDetail(sessionID)
	if !ok {
		t.Fatal("会话应仍在")
	}
	// 没有丢更新: 每次成功的回合都恰好追加一条补充文本
	if got := len(detail["follow_ups"].([]string)); int64(got) != successCount.Load() {
		t.Errorf("补充条数 (%d) 应等于成功回合数 (%d) —— 不等就是有回合的写入被覆盖了",
			got, successCount.Load())
	}
	if successCount.Load()+busyCount.Load() != writers {
		t.Errorf("成功 %d + busy %d 应等于尝试数 %d",
			successCount.Load(), busyCount.Load(), writers)
	}
}

// 取锁期间新请求立刻拿到 busy, 而不是排队等 —— 这条保证界面不会无声卡住。
func TestBusyIsImmediateNotQueued(t *testing.T) {
	agent := newAgentWithStore(t, "")
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("首轮失败: %v", err)
	}

	// 手动占住会话, 模拟"上一轮还在跑"
	release, exists, busy := agent.Store().TryLockSession(result.SessionID)
	if !exists || busy {
		t.Fatalf("应能取到锁: exists=%v busy=%v", exists, busy)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := agent.Continue(result.SessionID, "在上一轮没结束时提交")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionBusy) {
			t.Errorf("应回 ErrSessionBusy, 实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("不该排队等待: 应立刻返回 busy")
	}
}
