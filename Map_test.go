package vmap

import (
	"math"
	"sync"
	"testing"
	"time"
)

// ============================================================
// 测试辅助工具 (Test Helpers)
// ============================================================

type rec struct {
	mu   sync.Mutex
	vals []any
}

func (r *rec) fn(v any) {
	r.mu.Lock()
	r.vals = append(r.vals, v)
	r.mu.Unlock()
}

func (r *rec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vals)
}

func (r *rec) all() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.vals...)
}

func (r *rec) wait(t *testing.T, n int) []any {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if r.count() >= n {
			return r.all()
		}
	}
	t.Fatalf("回调次数 = %d，期望至少 %d 次", r.count(), n)
	return nil
}

func (r *rec) exactly(t *testing.T, n int, d time.Duration) []any {
	t.Helper()
	got := r.wait(t, n)
	time.Sleep(d)
	if c := r.count(); c != n {
		t.Fatalf("回调次数 = %d，期望恰好 %d 次（%v）", c, n, r.all())
	}
	return got
}

func (r *rec) none(t *testing.T, d time.Duration) {
	t.Helper()
	r.exactly(t, 0, d)
}

func mapWith(k, v any) *Map {
	m := NewMap()
	m.Set(k, v)
	return m
}

func withTTL(m *Map, key any, d time.Duration, r *rec) *Map {
	m.SetExpiredCall(key, d, r.fn)
	return m
}

// ============================================================
// 表驱动测试：零值与 nil 接收者行为
// ============================================================

func TestZeroValueAndNilReceiver(t *testing.T) {
	t.Run("Zero value map is immediately usable", func(t *testing.T) {
		var m Map
		m.Set("a", "b")
		if m.Len() != 1 || m.Get("a") != "b" {
			t.Fatalf("zero value 不可用: len=%d all=%#v", m.Len(), m.ReadAll())
		}
		m.Del("a")
		if m.Len() != 0 || m.Has("a") {
			t.Fatal("zero value 删除失败")
		}
	})

	t.Run("NewMapSize edge cases", func(t *testing.T) {
		if m := NewMapSize(-1); m == nil || m.Len() != 0 {
			t.Fatal("NewMapSize(-1) 创建失败")
		}
	})

	t.Run("Nil receiver operations are inert and safe", func(t *testing.T) {
		var m *Map
		m.Set("a", 1)
		m.Del("a")
		m.Close()
		m.Reset()
		m.Range(func(any, any) bool { t.Fatal("nil 接收者不应回调"); return false })

		if m.Len() != 0 || m.Has("a") || m.Get("a") != nil {
			t.Fatal("nil 接收者读写检查失败")
		}
		if _, ok := m.GetHas("a"); ok {
			t.Fatal("GetHas 应为 false")
		}
		if m.Keys() != nil || m.Values() != nil || m.Clone() != nil || m.ReadAll() != nil {
			t.Fatal("nil 接收者快照/集合方法应返回 nil")
		}
		if err := m.Copy(NewMap(), true); err == nil {
			t.Fatal("nil 接收者 Copy 应报错")
		}
		if b, err := m.MarshalJSON(); err != nil || string(b) != "null" {
			t.Fatalf("MarshalJSON = %q, %v", b, err)
		}
		if s := m.String(); s != "null" {
			t.Fatalf("String = %q", s)
		}
	})
}

// ============================================================
// 表驱动测试：键合法性校验 (Key Validation)
// ============================================================

func TestKeyValidation(t *testing.T) {
	type nanFloat float64
	type nanStruct struct{ F float64 }

	ptr := new(int)
	tests := []struct {
		name string
		key  any
		ok   bool
	}{
		{"nil key", nil, true},
		{"string key", "k", true},
		{"bool key", true, true},
		{"int key", 1, true},
		{"float key", 1.5, true},
		{"complex key", complex(1, 2), true},
		{"pointer key", ptr, true},
		{"struct key", struct{ A int }{1}, true},
		{"array key", [2]int{1, 2}, true},
		{"NaN float", math.NaN(), false},
		{"named NaN float", nanFloat(math.NaN()), false},
		{"NaN complex", complex(math.NaN(), 0), false},
		{"struct holding NaN", nanStruct{math.NaN()}, false},
		{"slice key", []int{1}, false},
		{"map key", map[string]int{}, false},
		{"func key", func() {}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validKey(tt.key); got != tt.ok {
				t.Fatalf("validKey = %v，期望 %v", got, tt.ok)
			}
			m := NewMap()
			func() {
				defer func() {
					r := recover()
					if tt.ok && r != nil {
						t.Fatalf("合法键不应 panic: %v", r)
					}
					if !tt.ok && r == nil {
						t.Fatal("非法键应 panic")
					}
				}()
				m.Set(tt.key, "v")
			}()

			if !tt.ok {
				if m.Len() != 0 {
					t.Fatalf("panic 后表被污染: %#v", m.Keys())
				}
				m.Del(tt.key)
				m.Dels([]any{tt.key})
				if _, ok := m.GetHas(tt.key); ok {
					t.Fatal("非法键不应可读")
				}
			} else {
				if v, ok := m.GetHas(tt.key); !ok || v != "v" {
					t.Fatalf("GetHas = %#v, %v", v, ok)
				}
			}
		})
	}
}

// ============================================================
// 基础读写及高级索引 (Basic Operations & Indexing)
// ============================================================

func TestBasicOpsAndIndexing(t *testing.T) {
	m := mapWith("one", 1)
	m.Set("nil", nil)

	t.Run("Standard reads and defaults", func(t *testing.T) {
		if m.Get("one") != 1 || m.Get("nil") != nil || m.Get("missing") != nil {
			t.Fatal("Get 取值错误")
		}
		if v, ok := m.GetHas("nil"); !ok || v != nil {
			t.Fatalf("存入的 nil 无法通过 GetHas 区分: %#v, %v", v, ok)
		}
		if got := m.GetOrDefault("missing", "def"); got != "def" {
			t.Fatalf("GetOrDefault 默认值错误: %v", got)
		}
	})

	t.Run("Nested Index and IndexHas chains", func(t *testing.T) {
		root := NewMap()
		sub := mapWith("b", "value")
		root.Set("a", sub)

		if v, ok := root.IndexHas("a", "b"); !ok || v != "value" {
			t.Fatalf("IndexHas = %#v, %v", v, ok)
		}
		if got := root.Index("a", "b"); got != "value" {
			t.Fatalf("Index = %#v", got)
		}
		// 边界：空链、中间节点非 Map、缺失键
		if _, ok := root.IndexHas(); ok {
			t.Fatal("空链应返回 false")
		}
		if _, ok := root.IndexHas("a", "b", "c"); ok {
			t.Fatal("链条越界应返回 false")
		}
		if _, ok := root.IndexHas("non-existent", "b"); ok {
			t.Fatal("首级不存在应返回 false")
		}
	})
}

// ============================================================
// 原子操作：LoadOrStore 与 LoadAndDelete
// ============================================================

func TestAtomicOperations(t *testing.T) {
	t.Run("LoadOrStore semantics", func(t *testing.T) {
		m := NewMap()
		if val, loaded := m.LoadOrStore("k", 100); loaded || val != 100 {
			t.Fatalf("首次 LoadOrStore 失败: %v, %v", val, loaded)
		}
		if val, loaded := m.LoadOrStore("k", 200); !loaded || val != 100 {
			t.Fatalf("重复 LoadOrStore 未能返回原值: %v, %v", val, loaded)
		}
		// 已存在的 nil 键
		m.Set("nil_key", nil)
		if val, loaded := m.LoadOrStore("nil_key", 999); !loaded || val != nil {
			t.Fatalf("nil 值重复加载失败: %v, %v", val, loaded)
		}
	})

	t.Run("LoadOrStore concurrent safety", func(t *testing.T) {
		cm := NewMap()
		var wg sync.WaitGroup
		results := make([]any, 50)
		for i := range results {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				v, _ := cm.LoadOrStore("shared", idx)
				results[idx] = v
			}(i)
		}
		wg.Wait()
		first := results[0]
		for i, v := range results {
			if v != first {
				t.Fatalf("第 %d 个 goroutine 抢占结果不一致: %v vs %v", i, v, first)
			}
		}
	})

	t.Run("LoadAndDelete semantics", func(t *testing.T) {
		m := mapWith("key", "val")
		if v, ok := m.LoadAndDelete("key"); !ok || v != "val" {
			t.Fatalf("LoadAndDelete 返回错误: %v, %v", v, ok)
		}
		if m.Has("key") {
			t.Fatal("LoadAndDelete 后键依然存在")
		}
		if _, ok := m.LoadAndDelete("key"); ok {
			t.Fatal("对已删除键二次删除应返回 false")
		}
	})
}

// ============================================================
// 嵌套 Map 辅助器 (New / GetNewMap / GetNewMaps)
// ============================================================

func TestNestedMapHelpers(t *testing.T) {
	t.Run("GetNewMap reuse and type replacement", func(t *testing.T) {
		m := NewMap()
		sub1 := m.GetNewMap("sub")
		sub2 := m.GetNewMap("sub")
		if sub1 == nil || sub1 != sub2 {
			t.Fatal("GetNewMap 未能正确复用已有 *Map")
		}

		// 用标量覆盖后再次调用 GetNewMap 应自动创建新表并触发旧值清理通知
		r := &rec{}
		withTTL(m, "sub", time.Hour, r)
		m.Set("sub", "scalar_value")
		newSub := m.GetNewMap("sub")
		if newSub == nil || m.Get("sub") != newSub {
			t.Fatal("GetNewMap 未能正确替换非 Map 值为新表")
		}
	})

	t.Run("GetNewMaps chain creation", func(t *testing.T) {
		root := NewMap()
		leaf := root.GetNewMaps("lvl1", "lvl2", "lvl3")
		if leaf == nil {
			t.Fatal("GetNewMaps 返回 nil")
		}
		if got, ok := root.IndexHas("lvl1", "lvl2", "lvl3"); !ok || got != leaf {
			t.Fatal("GetNewMaps 链式路径不匹配")
		}
		if root.GetNewMaps() != root {
			t.Fatal("GetNewMaps 无参数应返回接收者本身")
		}
	})
}

// ============================================================
// TTL 生命周期与定时器管理
// ============================================================

func TestTTLLifecycle(t *testing.T) {
	t.Run("Normal TTL expiration and callback", func(t *testing.T) {
		r := &rec{}
		m := withTTL(mapWith("k", "v"), "k", 25*time.Millisecond, r)
		if got := r.exactly(t, 1, 100*time.Millisecond); got[0] != "v" {
			t.Fatalf("过期回调参数错误: %v", got[0])
		}
		if m.Has("k") {
			t.Fatal("过期后键未自动删除")
		}
		if _, ok := m.TTL("k"); ok {
			t.Fatal("过期后不应再能获取 TTL")
		}
	})

	t.Run("Re-arming TTL and cancellation (d=0)", func(t *testing.T) {
		r := &rec{}
		m := withTTL(mapWith("k", 1), "k", 50*time.Millisecond, r)
		// 重设延时并保持旧回调
		m.SetExpiredCall("k", 80*time.Millisecond, nil)
		// 更改为 d=0 取消 TTL
		m.SetExpired("k", 0)
		r.none(t, 60*time.Millisecond)
		if !m.Has("k") {
			t.Fatal("取消 TTL 不应删除键")
		}
	})

	t.Run("Negative duration expires immediately", func(t *testing.T) {
		r := &rec{}
		m := mapWith("k", 42)
		m.SetExpiredCall("k", -1, r.fn)
		if got := r.exactly(t, 1, 50*time.Millisecond); got[0] != 42 {
			t.Fatalf("立即过期回调错误: %v", got[0])
		}
		if m.Has("k") {
			t.Fatal("立即过期后键应被移除")
		}
	})
}

// ============================================================
// 锁外回调及重入性验证
// ============================================================

func TestCallbackConcurrencyAndReentrancy(t *testing.T) {
	t.Run("Callbacks execute outside lock preventing deadlock", func(t *testing.T) {
		var mu sync.Mutex
		mu.Lock()

		m := mapWith("k", 10)
		done := make(chan struct{})
		m.SetExpiredCall("k", time.Hour, func(any) {
			mu.Lock()
			mu.Unlock()
			close(done)
		})

		delDone := make(chan struct{})
		go func() {
			defer close(delDone)
			m.Del("k")
		}()

		select {
		case <-delDone:
			// 成功返回证明没有在锁内同步等待回调
		case <-time.After(2 * time.Second):
			t.Fatal("Del 死锁：回调可能在持锁期间同步执行")
		}

		mu.Unlock() // 释放外部锁让回调继续
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("过期回调未执行")
		}
	})

	t.Run("Callback can re-enter map operations safely", func(t *testing.T) {
		m := mapWith("k", 1)
		done := make(chan struct{})
		m.SetExpiredCall("k", -1, func(any) {
			m.Set("reentrant_key", 999)
			_ = m.Len()
			close(done)
		})
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("回调重入导致死锁")
		}
		if m.Get("reentrant_key") != 999 {
			t.Fatal("回调内的写入未能生效")
		}
	})
}

// ============================================================
// Close 与 Reset 的差异语义
// ============================================================

func TestCloseVsReset(t *testing.T) {
	t.Run("Reset clears silently and keeps map usable", func(t *testing.T) {
		r := &rec{}
		m := withTTL(mapWith("k", 1), "k", time.Hour, r)
		m.Reset()
		r.none(t, 40*time.Millisecond) // Reset 不应触发过期回调
		if m.Len() != 0 {
			t.Fatal("Reset 未能清空表")
		}
		m.Set("new_k", 2)
		if m.Get("new_k") != 2 {
			t.Fatal("Reset 后的表应能正常读写")
		}
	})

	t.Run("Close triggers unexpired callbacks and becomes terminal", func(t *testing.T) {
		r := &rec{}
		m := withTTL(mapWith("k", 5), "k", time.Hour, r)
		m.Close()

		if got := r.exactly(t, 1, 40*time.Millisecond); got[0] != 5 {
			t.Fatalf("Close 未正确投递未到期回调: %v", got[0])
		}
		if !m.IsClosed() {
			t.Fatal("Close 后 IsClosed 应为 true")
		}
		// 关闭后禁止写入
		m.Set("k", 10)
		if m.Len() != 0 {
			t.Fatal("关闭后的 Map 不应接受新写入")
		}
	})
}

// ============================================================
// 遍历与快照 (Iteration & Snapshots)
// ============================================================

func TestIterationAndSnapshots(t *testing.T) {
	t.Run("Keys and Values alignment", func(t *testing.T) {
		m := NewMap()
		for i := 0; i < 10; i++ {
			m.Set(i, i*100)
		}
		ks, vs := m.Keys(), m.Values()
		if len(ks) != 10 || len(vs) != 10 {
			t.Fatalf("Keys/Values 长度不匹配: %d vs %d", len(ks), len(vs))
		}
	})

	t.Run("Range early termination and safe mutation", func(t *testing.T) {
		m := NewMap()
		for i := 0; i < 5; i++ {
			m.Set(i, i)
		}
		count := 0
		m.Range(func(k, v any) bool {
			count++
			return count < 2 // 提前终止
		})
		if count != 2 {
			t.Fatalf("Range 提前终止失败，遍历了 %d 项", count)
		}

		// 遍历时修改不应引发死锁或 panic
		m.Range(func(k, v any) bool {
			m.Set("mutated", true)
			return true
		})
		if !m.Has("mutated") {
			t.Fatal("Range 期间修改未生效")
		}
		m.Range(nil) // 不应 panic
	})
}

// ============================================================
// 深拷贝、Copy 与 Clone
// ============================================================

func TestCopyAndClone(t *testing.T) {
	t.Run("Clone preserves structures and isolates data", func(t *testing.T) {
		src := NewMap()
		src.Set("k", "v")
		child := mapWith("sub_k", 1)
		src.Set("nested", child)

		dst := src.Clone()
		if dst.Get("k") != "v" {
			t.Fatal("Clone 数据丢失")
		}

		// 隔离性检查
		subDst := dst.Get("nested").(*Map)
		if subDst == child {
			t.Fatal("嵌套 *Map 与源表共享")
		}
		subDst.Set("sub_k", 99)
		if child.Get("sub_k") != 1 {
			t.Fatal("修改克隆表影响了源表")
		}
	})

	t.Run("Copy with overwrite rules", func(t *testing.T) {
		dst := mapWith("a", 1)
		src := mapWith("a", 2)
		src.Set("b", 3)

		// over = false 不覆盖
		_ = dst.Copy(src, false)
		if dst.Get("a") != 1 || dst.Get("b") != 3 {
			t.Fatal("over=false 行为错误")
		}

		// over = true 覆盖
		_ = dst.Copy(src, true)
		if dst.Get("a") != 2 {
			t.Fatal("over=true 未能覆盖现有键")
		}
	})
}

// ============================================================
// JSON 序列化与反序列化
// ============================================================

func TestJSONSerialization(t *testing.T) {
	t.Run("Basic JSON roundtrip", func(t *testing.T) {
		src := NewMap()
		src.Set("str", "hello")
		src.Set("num", 123.45)
		src.Set("bool", true)
		src.Set("sub", mapWith("nested", "val"))

		b, err := src.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON 失败: %v", err)
		}

		dst := NewMap()
		if err := dst.UnmarshalJSON(b); err != nil {
			t.Fatalf("UnmarshalJSON 失败: %v", err)
		}
		if src.String() != dst.String() {
			t.Fatalf("JSON 往返数据不一致:\n%s\n%s", src.String(), dst.String())
		}
	})

	t.Run("JSON cycle detection", func(t *testing.T) {
		m := NewMap()
		m.Set("self", m)
		if _, err := m.MarshalJSON(); err == nil {
			t.Fatal("检测到循环引用时应返回错误")
		}
	})

	t.Run("Unmarshal null clears map", func(t *testing.T) {
		m := mapWith("a", 1)
		if err := m.UnmarshalJSON([]byte("null")); err != nil {
			t.Fatal(err)
		}
		if m.Len() != 0 {
			t.Fatal("解析 null 未能清空表")
		}
	})
}

// ============================================================
// WriteTo 与 ReadFrom 数据迁移
// ============================================================

func TestWriteToAndReadFrom(t *testing.T) {
	t.Run("WriteTo native map allocation", func(t *testing.T) {
		m := mapWith("key", 42)
		var dst map[string]int
		if err := m.WriteTo(&dst); err != nil {
			t.Fatalf("WriteTo 失败: %v", err)
		}
		if dst["key"] != 42 {
			t.Fatalf("写入 native map 错误: %v", dst)
		}
	})

	t.Run("ReadFrom merges data correctly", func(t *testing.T) {
		m := mapWith("existing", 1)
		src := map[string]any{"new_key": 2, "existing": 99}
		if err := m.ReadFrom(src); err != nil {
			t.Fatalf("ReadFrom 失败: %v", err)
		}
		if m.Get("existing") != 99 || m.Get("new_key") != 2 {
			t.Fatalf("ReadFrom 合并结果错误: %#v", m.ReadAll())
		}
	})
}

// ============================================================
// 错误处理与 Panic 观测钩子
// ============================================================

func TestPanicHandlerHook(t *testing.T) {
	caught := make(chan any, 1)
	SetPanicHandler(func(v any) {
		select {
		case caught <- v:
		default:
		}
	})
	t.Cleanup(func() { SetPanicHandler(nil) })

	m := mapWith("k", 1)
	m.SetExpiredCall("k", -1, func(any) {
		panic("test_panic_signal")
	})

	select {
	case p := <-caught:
		if p != "test_panic_signal" {
			t.Fatalf("捕获到意外的 panic 值: %v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未能在超时前观测到回调中的 panic")
	}
}

// ============================================================
// 高并发综合混跑测试 (Stress Test)
// ============================================================

func TestConcurrentMixedStress(t *testing.T) {
	m := NewMap()
	var wg sync.WaitGroup
	workers := 4
	iterations := 150

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				key := workerID*1000 + (i % 30)
				m.Set(key, i)
				m.SetExpiredCall(key, 2*time.Millisecond, func(any) {})

				if i%10 == 0 {
					_ = m.Clone()
					_, _ = m.MarshalJSON()
					_ = m.Keys()
				}
				if i%20 == 0 {
					m.Del(key)
				}
				if i%77 == 0 {
					m.Reset()
				}
			}
		}(w)
	}
	wg.Wait()
	time.Sleep(20 * time.Millisecond)
	m.Close()

	// 最终验证索引自洽性
	m.mu.RLock()
	dataLen := len(m.data)
	keysLen := len(m.keys)
	indexLen := len(m.keyIndex)
	m.mu.RUnlock()

	if dataLen != keysLen || dataLen != indexLen {
		t.Fatalf("高并发混跑后索引漂移失控: data=%d, keys=%d, keyIndex=%d", dataLen, keysLen, indexLen)
	}
}
