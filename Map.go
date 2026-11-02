package vmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	_ json.Marshaler   = (*Map)(nil)
	_ json.Unmarshaler = (*Map)(nil)
	_ fmt.Stringer     = (*Map)(nil)
)

const (
	maxNestDepth = 1024
	shrinkMinCap = 64
	// maxMarshalReentry 同一张表同时未返回的 MarshalJSON 上限。
	// 环每绕一圈加 1；正常情况下只有同时编组同一张表的 goroutine 数会占用它。
	maxMarshalReentry = maxNestDepth
)

// errTooDeep 把各路径里重复的嵌套上限错误收成一处，避免格式串以后改漏。
func errTooDeep(scope string) error {
	return fmt.Errorf("vmap: %s超过 %d 层", scope, maxNestDepth)
}

// errNotMap 修改原因：WriteTo、writeToSeen、ReadFrom 三处重复了同一句错误文本。
// 文案逐字保持原样，避免依赖错误串的调用方受影响。
func errNotMap(k reflect.Kind) error {
	return fmt.Errorf("Map: 不支持此类型type(%v)", k)
}

var errMarshalCycle = errors.New("vmap: MarshalJSON 检测到循环引用或嵌套过深")

// panicHook 存 func(any)。atomic.Value 不能存 nil，清空时改存 nopPanic。
// 回调必须 recover，否则用户 panic 打垮进程；同时也要能观测，且注册和触发不能数据竞争。
var panicHook atomic.Value

func nopPanic(any) {}

// SetPanicHandler 设置过期回调 panic 的观测函数。fn 为 nil 时恢复静默。
func SetPanicHandler(fn func(any)) {
	if fn == nil {
		fn = nopPanic
	}
	panicHook.Store(fn)
}

func reportPanic(r any) {
	v := panicHook.Load()
	fn, ok := v.(func(any))
	if !ok || fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(r)
}

// call 必须在释放 Map 锁之后执行。
type call struct {
	fn  func(any)
	val any
}

func (c call) run() {
	if c.fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
		}
	}()
	c.fn(c.val)
}

func goCall(c call) {
	if c.fn == nil {
		return
	}
	go c.run()
}

func goCalls(cs []call) {
	if len(cs) == 0 {
		return
	}
	go func() {
		for i := range cs {
			cs[i].run()
		}
	}()
}

// expiry 在登记后不再改 fn。timer 为 nil 表示尚未启动，克隆被丢弃时可以被 GC。
type expiry struct {
	timer    *time.Timer
	fn       func(any)
	gen      uint64
	deadline time.Time
}

// Map 是带 TTL 的 key-value 映射。TTL 可以在登记时指定，也可以在读取时指定。
// 禁止按值复制 Map（会复制锁且与源共享底层桶，并发写是 fatal）。请使用 *Map。
type Map struct {
	mu       sync.RWMutex
	data     map[any]any
	keyIndex map[any]int
	keys     []any
	// expired 为 nil 表示没有 TTL。读 nil map 是合法的，写路径再懒分配。
	expired map[any]*expiry
	inited  bool
	expSeq  uint64
	// marshaling 是本表尚未返回的 MarshalJSON 层数，只用原子操作读写。
	marshaling int32
}

// NewMap 创建空 Map。
func NewMap() *Map {
	return NewMapSize(0)
}

// NewMapSize 预分配约 n 个键。n<=0 与 NewMap 相同。
func NewMapSize(n int) *Map {
	if n < 0 {
		n = 0
	}
	return &Map{
		data:     make(map[any]any, n),
		keyIndex: make(map[any]int, n),
		keys:     make([]any, 0, n),
		inited:   true,
	}
}

// initLocked 调用者必须持有写锁。读路径禁止调用。
func (m *Map) initLocked() {
	if m == nil {
		return
	}
	if m.inited && m.data != nil && m.keyIndex != nil {
		return
	}
	if m.data == nil {
		m.data = make(map[any]any)
	}
	if m.keyIndex == nil {
		m.keyIndex = make(map[any]int)
	}
	m.inited = true
}

func complexOK(c complex128) bool {
	return !math.IsNaN(real(c)) && !math.IsNaN(imag(c))
}

// validKey 拒绝切片、map、函数，以及任何含 NaN 的键。
// 复合类型只做一次自比较。NaN != NaN，藏在结构体/数组里时 key==key 就是 false。
// 指针键只比地址，不看指向的值。
func validKey(key any) bool {
	switch k := key.(type) {
	case nil:
		return true
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr:
		return true
	case float32:
		return !math.IsNaN(float64(k))
	case float64:
		return !math.IsNaN(k)
	case complex64:
		return complexOK(complex128(k))
	case complex128:
		return complexOK(k)
	default:
		t := reflect.TypeOf(key)
		if t == nil || !t.Comparable() {
			return false
		}
		return keyMatch(key, key)
	}
}

func mustKey(key any) {
	if !validKey(key) {
		panic(fmt.Sprintf("vmap: 不可比较或 NaN 键 (%T)", key))
	}
}

func keyMatch(a, b any) (eq bool) {
	defer func() { _ = recover() }()
	return a == b
}

// nestedMap 取出非 nil *Map。
func nestedMap(v any) *Map {
	mm, ok := v.(*Map)
	if !ok || mm == nil {
		return nil
	}
	return mm
}

// Len 长度。以 len(data) 为准，不再单独维护会漂移的 length。
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.data)
}

// Set 设置
func (m *Map) Set(key, val any) {
	if m == nil {
		return
	}
	mustKey(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setLocked(key, val)
}

// setLocked 必须持有写锁。
func (m *Map) setLocked(key, val any) {
	m.initLocked()
	m.ensureIndexLocked()
	val = normalizeVal(val)
	if _, exists := m.data[key]; !exists {
		m.keys = append(m.keys, key)
		m.keyIndex[key] = len(m.keys) - 1
	}
	m.data[key] = val
	if !m.indexSyncedLocked() {
		m.reindexLocked()
	}
}

func isScalarAny(v any) bool {
	switch v.(type) {
	case string, bool, []byte,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64, complex64, complex128:
		return true
	}
	return false
}

// normalizeVal 不去锁其它 Map
func normalizeVal(val any) any {
	if v, ok := val.(*Map); ok {
		if v == nil {
			return NewMap()
		}
		return v
	}
	if isScalarAny(val) {
		return val
	}
	if mv := mapFromAny(val); mv != nil {
		return detachMapValueKeepTTL(mv)
	}
	return val
}

// mapValuePtr 把反射中的按值 Map 搬到新分配的 *Map。不是按值 Map 时返回 nil。
func mapValuePtr(rv reflect.Value) *Map {
	if !rv.IsValid() || rv.Type() != mapValType || !rv.CanInterface() {
		return nil
	}
	p := reflect.New(mapValType)
	p.Elem().Set(rv)
	return p.Interface().(*Map)
}

// mapFromAny 是 mapValuePtr 的 any 入口。v 已是 interface，ValueOf 不会再拷贝具体值。
func mapFromAny(v any) *Map {
	if v == nil {
		return nil
	}
	return mapValuePtr(reflect.ValueOf(v))
}

// detachMapValueKeepTTL 数据层浅拷贝，并登记、启动 TTL（含已到期但尚未触发的项）。
// 这张表会立刻交给调用方，没有后续 activate，所以这里 arm。
func detachMapValueKeepTTL(v *Map) *Map {
	if v == nil {
		return NewMap()
	}
	return newDetached(valueItems(v), true, true)
}

// detachMapValue 浅拷贝按值 Map 的数据层，不带 TTL。
// 参数用 *Map，避免按值接收复制 sync.RWMutex。
func detachMapValue(v *Map) (out *Map) {
	if v == nil {
		return NewMap()
	}
	// 读或写入时 panic 就报告并返回空表，不把 panic 抛给 Set。
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
			out = NewMapSize(0)
		}
	}()
	return newDetached(valueItems(v), false, false)
}

// valueItems 读取按值 Map 副本的条目（含 TTL 快照）。副本的锁不可用，因此不加锁。
// 边界：并发写 v.data / v.expired 是 fatal，recover 接不住，是调用方责任。
func valueItems(v *Map) (items []snapItem) {
	if v == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
			items = nil
		}
	}()
	return v.itemsLocked()
}

// newDetached 把条目写入尚未发布的新表。
func newDetached(items []snapItem, keepTTL, arm bool) *Map {
	out := NewMapSize(len(items))
	out.mu.Lock()
	defer out.mu.Unlock()
	for _, it := range items {
		if _, exists := out.data[it.key]; exists {
			out.data[it.key] = it.val
		} else {
			out.indexPut(it.key, it.val)
		}
		if keepTTL {
			out.keepExp(it.key, it.exp, arm)
		}
	}
	return out
}

// Get 读取。不存在返回 nil，无法与存入的 nil 区分，请用 GetHas。
func (m *Map) Get(key any) any {
	v, _ := m.GetHas(key)
	return v
}

// GetHas 读取并判断。
func (m *Map) GetHas(key any) (any, bool) {
	if m == nil || !validKey(key) {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	return v, ok
}

// Has 判断键是否存在。与 GetHas 看同一份 data。
func (m *Map) Has(key any) bool {
	_, ok := m.GetHas(key)
	return ok
}

// indexPut 直接写入 data/keys/keyIndex。
func (m *Map) indexPut(k, v any) {
	m.data[k] = v
	m.keys = append(m.keys, k)
	m.keyIndex[k] = len(m.keys) - 1
}

// GetOrDefault 键不存在时返回 def。键存在且值为 nil 时返回 nil。
func (m *Map) GetOrDefault(key any, def any) any {
	v, ok := m.GetHas(key)
	if !ok {
		return def
	}
	return v
}

// LoadOrStore 键已存在则返回已有值，否则写入 val。
func (m *Map) LoadOrStore(key, val any) (actual any, loaded bool) {
	if m == nil {
		return nil, false
	}
	mustKey(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	if v, ok := m.data[key]; ok {
		return v, true
	}
	m.setLocked(key, val)
	return m.data[key], false
}

// New 删除 key（若存在）后写入一个新 Map。
func (m *Map) New(key any) *Map {
	if m == nil {
		return nil
	}
	mustKey(key)
	t := NewMap()
	var c call
	defer func() { goCall(c) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	c, _ = m.delLocked(key)
	m.setLocked(key, t)
	return t
}

// GetNewMap 读取 key 对应的 *Map；不存在时创建。
func (m *Map) GetNewMap(key any) *Map {
	if m == nil {
		return nil
	}
	mustKey(key)

	m.mu.RLock()
	if v, ok := m.data[key]; ok {
		if mm := nestedMap(v); mm != nil {
			m.mu.RUnlock()
			return mm
		}
	}
	m.mu.RUnlock()

	var c call
	defer func() { goCall(c) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	if v, ok := m.data[key]; ok {
		if mm := nestedMap(v); mm != nil {
			return mm
		}
		c, _ = m.delLocked(key)
	}
	t := NewMap()
	m.setLocked(key, t)
	return t
}

// GetNewMaps 链式读取或创建。空参数返回接收者本身。
func (m *Map) GetNewMaps(keys ...any) *Map {
	tm := m
	for _, k := range keys {
		if tm == nil {
			return nil
		}
		tm = tm.GetNewMap(k)
	}
	return tm
}

// Index 指定读取。
func (m *Map) Index(key ...any) any {
	v, _ := m.IndexHas(key...)
	return v
}

// IndexHas 逐层读取。中间节点必须是非 nil *Map。
//
//	m1 := vmap.NewMap()
//	m2 := vmap.NewMap()
//	m2.Set("b", "value")
//	m1.Set("a", m2)
//	v, ok := m1.IndexHas("a", "b")
func (m *Map) IndexHas(keys ...any) (any, bool) {
	if m == nil || len(keys) == 0 {
		return nil, false
	}
	cur := m
	for i, k := range keys {
		v, ok := cur.GetHas(k)
		if !ok {
			return nil, false
		}
		if i == len(keys)-1 {
			return v, true
		}
		mm := nestedMap(v)
		if mm == nil {
			return nil, false
		}
		cur = mm
	}
	return nil, false
}

// Del 删除。过期回调在释放锁之后、另一个 goroutine 里执行。
func (m *Map) Del(key any) {
	if m == nil || !validKey(key) {
		return
	}
	var c call
	defer func() { goCall(c) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	c, _ = m.delLocked(key)
}

// LoadAndDelete 原子取出并删除。
func (m *Map) LoadAndDelete(key any) (any, bool) {
	if m == nil || !validKey(key) {
		return nil, false
	}
	var c call
	defer func() { goCall(c) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	c, removed := m.delLocked(key)
	return c.val, removed
}

// delLocked 必须持有写锁。不执行回调。
// fn 必须在 removeKeyLocked 之前拷贝：remove 会停掉定时器并丢掉 expiry。
func (m *Map) delLocked(key any) (call, bool) {
	m.ensureIndexLocked()
	var fn func(any)
	if e := m.clearExpLocked(key); e != nil {
		fn = e.fn
	}
	val, ok := m.removeKeyLocked(key)
	if !ok {
		// data 没有该键。残留定时器已由 remove 停掉。Del 对不存在的键不回调。
		return call{}, false
	}
	return call{fn: fn, val: val}, true
}

func stopTimer(e *expiry) {
	if e == nil || e.timer == nil {
		return
	}
	e.timer.Stop()
	// 断开 Timer 引用，便于 GC；也避免后续误以为定时器仍在跑。
	e.timer = nil
}

// removeKeyLocked 从 data、keys、keyIndex 同时删除。必须持有写锁。
func (m *Map) removeKeyLocked(key any) (any, bool) {
	val, ok := m.data[key]
	if !ok {
		// data 已无键时清掉残留 TTL，避免之后打到已删除键并拖住 GC。
		m.clearExpLocked(key)
		if _, indexed := m.keyIndex[key]; indexed {
			delete(m.keyIndex, key)
			m.reindexLocked()
		}
		return nil, false
	}
	delete(m.data, key)
	// 先摘数据再停表
	m.clearExpLocked(key)
	idx, indexed := m.keyIndex[key]
	delete(m.keyIndex, key)
	n := len(m.keys)
	if !indexed || idx < 0 || idx >= n || !keyMatch(m.keys[idx], key) {
		m.reindexLocked()
		return val, true
	}
	m.swapDeleteKeyAt(idx)
	if !m.indexSyncedLocked() {
		m.reindexLocked()
	}
	return val, true
}

func (m *Map) swapDeleteKeyAt(idx int) {
	last := len(m.keys) - 1
	if idx != last {
		lk := m.keys[last]
		m.keys[idx] = lk
		m.keyIndex[lk] = idx
	}
	m.keys[last] = nil
	m.keys = m.keys[:last]
	m.maybeShrinkLocked()
}

func (m *Map) maybeShrinkLocked() {
	c := cap(m.keys)
	n := len(m.keys)
	// 32 位平台上 n*4 可能溢出为负，紧缩条件失效。
	if c > shrinkMinCap && int64(c) > int64(n)*4 {
		m.reindexLocked()
	}
}

func (m *Map) ensureIndexLocked() {
	// 写路径保持 O(1)。等长但下标不对的损坏由快照兜底，避免每次 Set 扫全表。
	if m.indexSyncedLocked() {
		return
	}
	m.reindexLocked()
}

// indexSyncedLocked 三表长度一致
func (m *Map) indexSyncedLocked() bool {
	return len(m.keys) == len(m.data) && len(m.keyIndex) == len(m.data)
}

// reindexLocked 丢掉不可比较键和 NaN，并换新 map，让旧桶可以被 GC。
func (m *Map) reindexLocked() {
	nk := make([]any, 0, len(m.data))
	ni := make(map[any]int, len(m.data))
	nd := make(map[any]any, len(m.data))
	add := func(k, v any) {
		if !validKey(k) {
			return
		}
		// 优化：复用 ni 做去重，省掉单独的 seen map 分配。
		if _, dup := ni[k]; dup {
			return
		}
		ni[k] = len(nk)
		nk = append(nk, k)
		nd[k] = v
	}
	// 第一遍（m.keys）：按 m.keys 的顺序把“keys 里存在且 data 里确实有”的键加入 nk/nd/ni，保留插入/交换删除后的顺序。
	for _, k := range m.keys {
		if v, ok := m.data[k]; ok {
			add(k, v)
		}
	}
	// 第二遍（m.data）：兜底 m.keys 里缺失、但 data 里还在的键（索引漂移、keys 被截断等损坏场景）。
	for k, v := range m.data {
		add(k, v)
	}
	m.keys = nk
	m.keyIndex = ni
	m.data = nd
	// 被丢掉的键若还挂着定时器，回调会打在已不存在的键上，并拖住 Map 无法 GC。
	for k, e := range m.expired {
		if e != nil && validKey(k) {
			if _, ok := nd[k]; ok {
				continue
			}
		}
		stopTimer(e)
		delete(m.expired, k)
	}
}

// Dels 一次加锁删除。
func (m *Map) Dels(keys []any) {
	if m == nil || len(keys) == 0 {
		return
	}
	var calls []call
	defer func() { goCalls(calls) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	for _, k := range keys {
		if !validKey(k) {
			continue
		}
		if c, ok := m.delLocked(k); ok && c.fn != nil {
			calls = append(calls, c)
		}
	}
}

// SetExpired 设置有效期。d==0 取消；键不存在则忽略。
func (m *Map) SetExpired(key any, d time.Duration) {
	m.SetExpiredCall(key, d, nil)
}

// SetExpiredCall 设置有效期。过期、到期前 Del、Close 时调用 f。
// d==0 取消且不调用 f。d<0 立即过期并调用 f。f==nil 时保留旧回调。
func (m *Map) SetExpiredCall(key any, d time.Duration, f func(any)) {
	if m == nil || !validKey(key) {
		return
	}
	var c call
	defer func() { goCall(c) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	if _, ok := m.data[key]; !ok {
		return
	}
	fn := f
	if old := m.expired[key]; old != nil && fn == nil {
		fn = old.fn
	}
	if d < 0 {
		val, _ := m.removeKeyLocked(key)
		c = call{fn: fn, val: val}
		return
	}
	if d == 0 {
		m.clearExpLocked(key)
		return
	}
	m.armLocked(key, d, fn)
}

// armLocked 必须持有写锁，并且在返回前完成 expired 登记。
func (m *Map) armLocked(key any, d time.Duration, fn func(any)) {
	if d <= 0 {
		return
	}
	m.rememberExpLocked(key, deadlineAfter(d), fn)
	m.armEntryLocked(key, m.expired[key])
}

func (m *Map) rememberExpLocked(key any, deadline time.Time, fn func(any)) {
	if m.expired == nil {
		m.expired = make(map[any]*expiry)
	}
	m.clearExpLocked(key)
	m.expSeq++
	if m.expSeq == 0 {
		m.expSeq = 1
	}
	m.expired[key] = &expiry{fn: fn, gen: m.expSeq, deadline: deadline}
}

// keepExp 登记一份 TTL 快照。调用者必须持有写锁。exp 为 nil 时不动已有 TTL。
// Until<=0 只说明 deadline 已过，源定时器可能还没跑。丢掉 TTL 会把键留成永久键。
// arm 为 false 时不启动定时器。AfterFunc 在别的 goroutine 里跑，不会在持锁时重入。
func (m *Map) keepExp(key any, exp *expSnap, arm bool) {
	if exp == nil {
		return
	}
	m.rememberExpLocked(key, exp.deadline, exp.fn)
	if !arm {
		return
	}
	m.armEntryLocked(key, m.expired[key])
}

func (m *Map) armEntryLocked(key any, e *expiry) {
	if e == nil || e.timer != nil {
		return
	}
	rem := time.Until(e.deadline)
	if rem <= 0 {
		rem = time.Nanosecond
	}
	gen := e.gen
	e.timer = time.AfterFunc(rem, func() { m.onTimer(key, gen) })
}

func deadlineAfter(d time.Duration) time.Time {
	now := time.Now()
	dl := now.Add(d)
	if d > 0 && !dl.After(now) {
		// 极大 Duration 加到 Time 上会溢出到过去，TTL 显示已过期，定时器却还在。
		return now.Add(time.Hour * 24 * 365 * 100)
	}
	return dl
}

// clearExpLocked 摘掉 key 的 TTL 并停表，返回被摘掉的登记（没有则 nil）。必须持有写锁。
func (m *Map) clearExpLocked(key any) *expiry {
	e := m.expired[key]
	if e == nil {
		return nil
	}
	delete(m.expired, key)
	stopTimer(e)
	return e
}

func (m *Map) onTimer(key any, gen uint64) {
	var c call
	func() {
		// time.AfterFunc 在独立 goroutine 里跑，panic 不会被调用方 recover，必须在这里收住。
		defer func() {
			if r := recover(); r != nil {
				reportPanic(r)
			}
		}()
		m.mu.Lock()
		defer m.mu.Unlock()
		e := m.expired[key]
		if e == nil || e.gen != gen {
			return
		}
		// 先留下 fn。removeKeyLocked 会停表。
		// 键已不在 data 时与 Del 一致不回调，避免给残留 TTL 投递 nil。
		fn := e.fn
		val, ok := m.removeKeyLocked(key)
		if !ok {
			return
		}
		c = call{fn: fn, val: val}
	}()
	// 与 Del 一致：锁已释放后再起 goroutine，避免回调时序绑在 AfterFunc 线程上。
	goCall(c)
}

// TTL 返回剩余有效期。ok 为 false 表示没有设置过期。剩余时间可能为负。
func (m *Map) TTL(key any) (time.Duration, bool) {
	if m == nil || !validKey(key) {
		return 0, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.expired[key]
	if e == nil {
		return 0, false
	}
	// expired 残留时不能对已删除键报告 TTL。
	if _, ok := m.data[key]; !ok {
		return 0, false
	}
	return time.Until(e.deadline), true
}

// ExpireAll 终态关闭 Map。
// 停止所有定时器并在释放锁后异步触发已登记的有效清理回调。
// 彻底释放底层数据结构并置为已关闭状态。关闭后 Map 变为不可写入状态。
func (m *Map) ExpireAll() {
	if m == nil {
		return
	}
	var calls []call
	defer func() { goCalls(calls) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	calls = m.takeExpiredLocked()
	m.resetDataLocked(false)
}

type snapItem struct {
	key any
	val any
	exp *expSnap
}

type expSnap struct {
	deadline time.Time
	fn       func(any)
}

// snap 补上expired的数据，用于快照。
func (m *Map) snap(k, v any) snapItem {
	it := snapItem{key: k, val: v}
	if e := m.expired[k]; e != nil {
		it.exp = &expSnap{deadline: e.deadline, fn: e.fn}
	}
	return it
}

// slotValue 下标必须指向自己。只比长度时 [a,a] 与 {a,b} 都是 2，快照会漏 b。
func (m *Map) slotValue(i int, k any) (any, bool) {
	if !validKey(k) {
		return nil, false
	}
	v, ok := m.data[k]
	if !ok {
		return nil, false
	}
	idx, ok := m.keyIndex[k]
	if !ok || idx != i {
		return nil, false
	}
	return v, true
}

func (m *Map) syncedItems() ([]snapItem, bool) {
	if !m.indexSyncedLocked() {
		return nil, false
	}
	out := make([]snapItem, len(m.keys))
	for i, k := range m.keys {
		v, ok := m.slotValue(i, k)
		if !ok {
			return nil, false
		}
		out[i] = m.snap(k, v)
	}
	return out, true
}

func (m *Map) fallbackItems() []snapItem {
	out := make([]snapItem, 0, len(m.data))
	seen := make(map[any]struct{}, len(m.data))
	for _, k := range m.keys {
		if !validKey(k) {
			continue
		}
		v, ok := m.data[k]
		if !ok {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, m.snap(k, v))
	}
	for k, v := range m.data {
		if !validKey(k) {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, m.snap(k, v))
	}
	return out
}

// itemsLocked 调用者持有读锁或写锁。索引漂移时以 data 为准补齐。
func (m *Map) itemsLocked() []snapItem {
	if m == nil || len(m.data) == 0 {
		return nil
	}
	if out, ok := m.syncedItems(); ok {
		return out
	}
	return m.fallbackItems()
}

func (m *Map) captureItems() (items []snapItem, err error) {
	if m == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
			items = nil
			err = fmt.Errorf("vmap: 快照失败: %v", r)
		}
	}()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.itemsLocked(), nil
}

func (m *Map) snapshotItems() []snapItem {
	items, _ := m.captureItems()
	return items
}

// snapshotColumnLocked 合并原来的 key/value 两份快照。values 为 false 时取出键。
func (m *Map) snapshotColumnLocked(values bool) ([]any, bool) {
	if !m.indexSyncedLocked() {
		return nil, false
	}
	out := make([]any, len(m.keys))
	for i, k := range m.keys {
		v, ok := m.slotValue(i, k)
		if !ok {
			return nil, false
		}
		if values {
			out[i] = v
		} else {
			out[i] = k
		}
	}
	return out, true
}

// collect 合并 Keys/Values。索引不可用时直接 fallback，不再经 itemsLocked 重扫。
func (m *Map) collect(values bool) (out []any) {
	if m == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
			out = nil
		}
	}()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if col, ok := m.snapshotColumnLocked(values); ok {
		return col
	}
	items := m.fallbackItems()
	out = make([]any, len(items))
	for i := range items {
		if values {
			out[i] = items[i].val
		} else {
			out[i] = items[i].key
		}
	}
	return out
}

// Keys 返回键快照。删除仍是交换删除，不是严格插入序。
func (m *Map) Keys() []any { return m.collect(false) }

// Values 按 Keys 的同一顺序返回值快照。
func (m *Map) Values() []any { return m.collect(true) }

// Reset 静默清空所有键并停止定时器，不触发任何回调。
// 复用内部切片容量（不超过 shrinkMinCap），调用后 Map 仍处于可用状态。
func (m *Map) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	m.dropExpiredSilentlyLocked()
	m.resetDataLocked(true)
}

func (m *Map) dropExpiredSilentlyLocked() {
	for _, e := range m.expired {
		stopTimer(e)
	}
	m.expired = nil
}

// resetDataLocked 清空 data/keys/keyIndex。
// keepSmall 为 true时复用不超过 shrinkMinCap 的 keys 数组；
// 为 false 时总是丢掉底层数组。
// 释放路径不再逐个置 nil：切片被丢掉后元素已经不可达，那遍循环不能让 GC 更快。
func (m *Map) resetDataLocked(keepSmall bool) {
	if keepSmall {
		if c := cap(m.keys); c > 0 && c <= shrinkMinCap {
			for i := range m.keys {
				m.keys[i] = nil
			}
			m.keys = m.keys[:0]
			m.data = make(map[any]any, c)
			m.keyIndex = make(map[any]int, c)
			m.inited = true
			return
		}
	}
	m.keys = nil
	m.data = make(map[any]any)
	m.keyIndex = make(map[any]int)
	m.inited = true
}

func (m *Map) takeExpiredLocked() []call {
	if len(m.expired) == 0 {
		m.expired = nil
		return nil
	}
	calls := make([]call, 0, len(m.expired))
	for k, e := range m.expired {
		if e == nil {
			continue
		}
		stopTimer(e)
		// 与 Del、onTimer「键不存在不回调」对齐。值为 nil 但键还在时仍然回调。
		if v, ok := m.data[k]; ok && e.fn != nil {
			calls = append(calls, call{fn: e.fn, val: v})
		}
	}
	m.expired = nil
	return calls
}

// Range 把快照交给 f。f 返回 false 时停止。
func (m *Map) Range(f func(key, value any) bool) {
	if m == nil || f == nil {
		return
	}
	items := m.snapshotItems()
	for _, it := range items {
		if !f(it.key, it.val) {
			return
		}
	}
}

// Copy 把 from 写入接收者。over 为 false 时不覆盖已存在的键。
// nil、自拷贝直接返回。嵌套 *Map 和容器会深拷贝。
// TTL 一律复制；deadline 已过但源定时器还没跑的键会马上过期，而不是变成永久键。
func (m *Map) Copy(from *Map, over bool) error {
	if m == nil || from == nil || m == from {
		return errors.New("invalid Map Copy")
	}
	items, err := from.captureItems()
	if err != nil {
		return fmt.Errorf("Map Copy failed: %w", err)
	}
	if len(items) == 0 {
		return nil
	}
	repl := map[*Map]*Map{from: m}
	made := map[*Map]struct{}{}
	// clearStale=over：与原 installCopiedExp 一致。
	m.commitItems(cloneItems(items, repl, made, 0), over, over, made)
	return nil
}

// commitItems 在写锁内逐项写入 items，锁外激活本次克隆出的嵌套 *Map。
// over=false 跳过已存在的键；over=true 覆盖。
func (m *Map) commitItems(items []snapItem, over, clearStale bool, made map[*Map]struct{}) {
	if len(items) == 0 {
		return
	}
	var written []any
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.initLocked()
		written = make([]any, 0, len(items))
		for _, it := range items {
			if !over {
				if _, exists := m.data[it.key]; exists {
					continue
				}
			}
			m.setLocked(it.key, it.val)
			if it.exp != nil {
				m.keepExp(it.key, it.exp, true)
			} else if clearStale {
				m.clearExpLocked(it.key)
			}
			written = append(written, it.val)
		}
	}()
	// 与 Del/onTimer 一致：激活在 m.mu 之外，只对 made 里的表加锁，避免 ABBA。
	actSeen := map[*Map]struct{}{m: {}}
	for _, v := range written {
		activateTree(v, made, actSeen, 0)
	}
}

// cloneItems 逐项深拷贝值，键和 TTL 快照原样保留。必须在不持有任何 Map 锁时调用。
func cloneItems(items []snapItem, repl map[*Map]*Map, made map[*Map]struct{}, depth int) []snapItem {
	ps := make([]snapItem, len(items))
	for i, it := range items {
		ps[i] = snapItem{key: it.key, val: cloneAnyDepth(it.val, repl, made, depth), exp: it.exp}
	}
	return ps
}

// Clone 深拷贝。nil 接收者返回 nil。
func (m *Map) Clone() *Map {
	if m == nil {
		return nil
	}
	dst := NewMap()
	dst.Copy(m, true)
	return dst
}

func cloneMap(src *Map, repl map[*Map]*Map, made map[*Map]struct{}, depth int) *Map {
	if src == nil {
		return nil
	}
	if repl == nil {
		repl = map[*Map]*Map{}
	}
	if made == nil {
		made = map[*Map]struct{}{}
	}
	if dst, ok := repl[src]; ok {
		return dst
	}
	// 必须先查 repl 再判深度，循环引用在超限前仍要回到已建的 dst。
	if depth > maxNestDepth {
		return src
	}
	items := src.snapshotItems()
	dst := NewMapSize(len(items))
	repl[src] = dst
	made[dst] = struct{}{}
	// cloneItems 作为实参先求值，克隆发生在加 dst 锁之前，避免和子表的锁交叉。
	storeClone(dst, cloneItems(items, repl, made, depth+1))
	return dst
}

// storeClone 只登记 TTL，不 arm；定时器统一交给 activateTree。
// 必须已持有 dst 写锁。 已过期 TTL 也要登记，否则嵌套 Clone 变成永久键。
// 这里不 arm，避免克隆被丢弃后定时器仍触发。
func storeClone(dst *Map, ps []snapItem) {
	dst.mu.Lock()
	defer dst.mu.Unlock() // panic 时不能漏锁。
	for _, p := range ps {
		dst.setLocked(p.key, p.val)
		dst.keepExp(p.key, p.exp, false)
	}
}

// cloneMapVal 深拷贝按值传入的 Map。参数指向调用方的副本，副本的锁不可用，因此不加锁。
func cloneMapVal(v *Map, repl map[*Map]*Map, made map[*Map]struct{}, depth int) *Map {
	if v == nil {
		return NewMap()
	}
	if depth > maxNestDepth {
		// 超限结果若没被 activate（调用方丢弃、Copy 未写入），回调仍会触发。
		dst := newDetached(valueItems(v), true, false)
		if made != nil {
			made[dst] = struct{}{}
		}
		return dst
	}
	if repl == nil {
		repl = map[*Map]*Map{}
	}
	if made == nil {
		made = map[*Map]struct{}{}
	}
	items := valueItems(v)
	dst := NewMapSize(len(items))
	made[dst] = struct{}{}
	storeClone(dst, cloneItems(items, repl, made, depth+1))
	return dst
}

func cloneAnyDepth(v any, repl map[*Map]*Map, made map[*Map]struct{}, depth int) any {
	if v == nil {
		return v
	}
	// 超限前必须先走 cloneMap，循环引用才能命中 repl。
	if mm, ok := v.(*Map); ok {
		return cloneMap(mm, repl, made, depth)
	}
	if depth > maxNestDepth {
		// 不在这里 arm。交给 cloneMapVal，未保留就不启动定时器。
		if mv := mapFromAny(v); mv != nil {
			return cloneMapVal(mv, repl, made, depth)
		}
		return v
	}
	switch x := v.(type) {
	case []byte:
		if x == nil {
			return x
		}
		out := make([]byte, len(x))
		copy(out, x)
		return out
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i := range x {
			out[i] = cloneAnyDepth(x[i], repl, made, depth+1)
		}
		return out
	case []*Map:
		if x == nil {
			return x
		}
		out := make([]*Map, len(x))
		for i := range x {
			out[i] = cloneMap(x[i], repl, made, depth+1)
		}
		return out
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = cloneAnyDepth(vv, repl, made, depth+1)
		}
		return out
	case map[any]any:
		if x == nil {
			return x
		}
		out := make(map[any]any, len(x))
		for k, vv := range x {
			out[k] = cloneAnyDepth(vv, repl, made, depth+1)
		}
		return out
	default:
		rv := reflect.ValueOf(v)
		if mv := mapValuePtr(rv); mv != nil {
			return cloneMapVal(mv, repl, made, depth)
		}
		if !rv.IsValid() {
			return v
		}
		switch rv.Kind() {
		case reflect.Map, reflect.Slice, reflect.Array:
			cloned := cloneReflect(rv, repl, made, depth)
			if cloned == nil {
				return v
			}
			return cloned
		case reflect.Pointer, reflect.Interface, reflect.Struct:
			// 类型不含 *Map 时也要拷贝切片/map，否则结构体字段会和源共享底层数组。
			if !typeNeedsDeepCopy(rv.Type()) {
				return v
			}
			cloned := cloneReflect(rv, repl, made, depth)
			if cloned == nil {
				return v
			}
			return cloned
		default:
			return v
		}
	}
}

// ifaceOrNil 合并 cloneReflect 里反复出现的 CanInterface 判断。
func ifaceOrNil(rv reflect.Value) any {
	if rv.IsValid() && rv.CanInterface() {
		return rv.Interface()
	}
	return nil
}

// cloneElem 合并 Slice、Array、Map 三处元素克隆。
func cloneElem(ev reflect.Value, repl map[*Map]*Map, made map[*Map]struct{}, depth int) any {
	if ev.CanInterface() {
		return cloneAnyDepth(ev.Interface(), repl, made, depth)
	}
	return cloneReflect(ev, repl, made, depth)
}

func cloneReflect(rv reflect.Value, repl map[*Map]*Map, made map[*Map]struct{}, depth int) any {
	if !rv.IsValid() || depth > maxNestDepth {
		return nil
	}
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() {
			return ifaceOrNil(rv)
		}
		ev := rv.Elem()
		// 未导出 interface 字段的动态值会被丢掉。Elem() 通常可以 Interface。
		if ev.IsValid() && ev.CanInterface() {
			return cloneAnyDepth(ev.Interface(), repl, made, depth+1)
		}
		if !ev.IsValid() {
			return ifaceOrNil(rv)
		}
		if cloned := cloneReflect(ev, repl, made, depth+1); cloned != nil {
			return cloned
		}
		return ifaceOrNil(rv)
	case reflect.Pointer:
		if rv.IsNil() {
			return ifaceOrNil(rv)
		}
		if rv.Type() == mapPtrType {
			if !rv.CanInterface() {
				return nil
			}
			// 反射路径克隆 *Map 也必须带 depth，不能在指针边界归零。
			return cloneMap(rv.Interface().(*Map), repl, made, depth)
		}
		if !typeNeedsDeepCopy(rv.Type()) {
			return ifaceOrNil(rv)
		}
		dst := reflect.New(rv.Type().Elem())
		cloned := cloneReflect(rv.Elem(), repl, made, depth+1)
		vv, err := valueFor(cloned, rv.Type().Elem())
		if err != nil || !vv.IsValid() {
			return ifaceOrNil(rv)
		}
		dst.Elem().Set(vv)
		return dst.Interface()
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice {
			if rv.IsNil() {
				return ifaceOrNil(rv)
			}
			if !sliceNeedsWalk(rv.Type()) {
				return copySliceValue(rv)
			}
		} else if !sliceNeedsWalk(rv.Type()) {
			// 数组走 reflect.Copy 必然 panic，整体 Set 最省事。
			if !rv.CanInterface() {
				return nil // 调用方 cloneAnyDepth 会退回原值
			}
			dst := reflect.New(rv.Type()).Elem()
			dst.Set(rv)
			return dst.Interface()
		}
		n := rv.Len()
		var dst reflect.Value
		if rv.Kind() == reflect.Slice {
			dst = reflect.MakeSlice(rv.Type(), n, n)
		} else {
			dst = reflect.New(rv.Type()).Elem()
		}
		for i := 0; i < n; i++ {
			ev := rv.Index(i)
			// 失败时保留原元素，避免静默变零值。
			keepClone(dst.Index(i), ev, cloneElem(ev, repl, made, depth+1))
		}
		return dst.Interface()
	case reflect.Map:
		if rv.IsNil() {
			return nil
		}
		nm := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		et := rv.Type().Elem()
		for _, k := range rv.MapKeys() {
			ev := rv.MapIndex(k)
			vv, err := valueFor(cloneElem(ev, repl, made, depth+1), et)
			if err != nil || !vv.IsValid() {
				if orig := reflect.ValueOf(ifaceOrNil(ev)); orig.IsValid() && orig.Type().AssignableTo(et) {
					nm.SetMapIndex(k, orig)
				}
				continue
			}
			nm.SetMapIndex(k, vv)
		}
		return nm.Interface()
	case reflect.Struct:
		// 顶层按值 Map 已在 normalizeVal 里转成 *Map。嵌在结构体里的按值 Map
		// 若不能 Interface，下面的 dst.Set 仍是浅拷贝，与原来相同。
		if mv := mapValuePtr(rv); mv != nil {
			return cloneMapVal(mv, repl, made, depth)
		}
		if !rv.CanInterface() || !typeNeedsDeepCopy(rv.Type()) {
			return ifaceOrNil(rv)
		}
		dst := reflect.New(rv.Type()).Elem()
		dst.Set(rv)
		for i := 0; i < rv.NumField(); i++ {
			f := dst.Field(i)
			src := rv.Field(i)
			if !f.CanSet() || !src.CanInterface() || !typeNeedsDeepCopy(src.Type()) {
				continue
			}
			cloned := cloneAnyDepth(src.Interface(), repl, made, depth+1)
			vv, err := valueFor(cloned, f.Type())
			if err != nil || !vv.IsValid() {
				continue
			}
			f.Set(vv)
		}
		return dst.Interface()
	default:
		return ifaceOrNil(rv)
	}
}

func valueFor(v any, et reflect.Type) (reflect.Value, error) {
	if v == nil {
		return nilValue(et)
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nilValue(et)
	}
	return convertAssign(rv, et)
}

func copySliceValue(rv reflect.Value) any {
	if rv.Kind() != reflect.Slice || !rv.IsValid() || !rv.CanInterface() {
		return nil
	}
	if rv.IsNil() {
		return rv.Interface()
	}
	dst := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
	reflect.Copy(dst, rv)
	return dst.Interface()
}

// activateTree 只启动本次克隆出来的 Map。共享原表已有自己的定时器，再锁它会和用户锁形成 ABBA。
func activateTree(v any, made map[*Map]struct{}, seen map[*Map]struct{}, depth int) {
	if v == nil {
		return
	}
	if t, ok := v.(*Map); ok {
		if _, mine := made[t]; mine {
			// 超限也要给本层克隆表 arm，否则已登记的 TTL 永不触发。
			// 不在 made 里的表是共享原表，或已被丢弃、不该启动的克隆。
			activateMap(t, made, seen, depth)
		}
		return
	}
	if depth > maxNestDepth {
		return
	}
	switch t := v.(type) {
	case []any:
		for _, el := range t {
			activateTree(el, made, seen, depth+1)
		}
	case []*Map:
		for _, p := range t {
			activateTree(p, made, seen, depth+1)
		}
	case map[string]any:
		for _, el := range t {
			activateTree(el, made, seen, depth+1)
		}
	case map[any]any:
		for _, el := range t {
			activateTree(el, made, seen, depth+1)
		}
	default:
		activateReflect(reflect.ValueOf(v), made, seen, depth)
	}
}

func activateMap(m *Map, made map[*Map]struct{}, seen map[*Map]struct{}, depth int) {
	if m == nil {
		return
	}
	if seen == nil {
		seen = map[*Map]struct{}{}
	}
	if _, ok := seen[m]; ok {
		return
	}
	seen[m] = struct{}{}
	var nested []any
	func() {
		m.mu.Lock()
		defer m.mu.Unlock() // panic 不能漏解锁。
		for k, e := range m.expired {
			if e == nil || e.timer != nil {
				continue
			}
			m.armEntryLocked(k, e)
		}
		if depth > maxNestDepth {
			return
		}
		// 预分配，避免嵌套值多时反复扩容。
		nested = make([]any, 0, len(m.data))
		for _, v := range m.data {
			if valueMayContainMap(v) {
				nested = append(nested, v)
			}
		}
	}()
	if depth > maxNestDepth {
		return
	}
	for _, v := range nested {
		activateTree(v, made, seen, depth+1)
	}
}

// valueMayContainMap 给激活阶段用。标量走类型开关，避免每个值都做反射。
func valueMayContainMap(v any) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case *Map, []any, []*Map, map[string]any, map[any]any:
		return true
	}
	if isScalarAny(v) {
		return false
	}
	// 按值 Map 的锁是副本，激活不能进去，也不能无锁扫它的 data。
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Type() == mapValType {
		return false
	}
	return typeMayContainMap(rv.Type())
}

func activateReflect(rv reflect.Value, made map[*Map]struct{}, seen map[*Map]struct{}, depth int) {
	if !rv.IsValid() || depth > maxNestDepth {
		return
	}
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() || !rv.CanInterface() {
			return
		}
		activateTree(rv.Interface(), made, seen, depth+1)
	case reflect.Pointer:
		if rv.IsNil() {
			return
		}
		// *Map 必须整指针交给 activateTree。其它指针要解引用，
		// 否则 *Struct 会把同一指针反复送回，里面的 *Map 定时器永远不启动。
		if rv.Type() == mapPtrType {
			if rv.CanInterface() {
				activateTree(rv.Interface(), made, seen, depth+1)
			}
			return
		}
		ev := rv.Elem()
		if !ev.IsValid() {
			return
		}
		if ev.CanInterface() {
			activateTree(ev.Interface(), made, seen, depth+1)
			return
		}
		activateReflect(ev, made, seen, depth+1)
	case reflect.Map:
		if rv.IsNil() || !typeMayContainMap(rv.Type()) {
			return
		}
		for _, k := range rv.MapKeys() {
			ev := rv.MapIndex(k)
			if ev.CanInterface() {
				activateTree(ev.Interface(), made, seen, depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		if !typeMayContainMap(rv.Type()) {
			return
		}
		for i := 0; i < rv.Len(); i++ {
			ev := rv.Index(i)
			if ev.CanInterface() {
				activateTree(ev.Interface(), made, seen, depth+1)
			}
		}
	case reflect.Struct:
		if rv.Type() == mapValType {
			return
		}
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Field(i)
			if !f.CanInterface() || !typeMayContainMap(f.Type()) {
				continue
			}
			activateTree(f.Interface(), made, seen, depth+1)
		}
	}
}

// ReadAll 递归展开为 map[any]any。环引用写成占位字符串，避免栈溢出。不复制 TTL。
// []*Map 保持原类型，避免旧的类型断言失效；[]any 会继续展开。
func (m *Map) ReadAll() any {
	if m == nil {
		return nil
	}
	return m.readAllDepth(make(map[*Map]struct{}), 0)
}

func (m *Map) readAllDepth(seen map[*Map]struct{}, depth int) any {
	if m == nil {
		return nil
	}
	if depth > maxNestDepth {
		return "<vmap.Map max depth>"
	}
	if _, ok := seen[m]; ok {
		return "<vmap.Map cycle>"
	}
	seen[m] = struct{}{}
	defer delete(seen, m)
	items, err := m.captureItems()
	if err != nil {
		// 保持返回 map，避免调用方类型断言变成字符串。
		return map[any]any{}
	}
	mm := make(map[any]any, len(items))
	for _, it := range items {
		mm[it.key] = readAllValueDepth(it.val, seen, depth+1)
	}
	return mm
}

func readAllValueDepth(v any, seen map[*Map]struct{}, depth int) any {
	if depth > maxNestDepth {
		return "<vmap max depth>"
	}
	switch t := v.(type) {
	case *Map:
		return t.readAllDepth(seen, depth+1)
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i := range t {
			out[i] = readAllValueDepth(t[i], seen, depth+1)
		}
		return out
	case []*Map:
		return t
	case map[string]any:
		if t == nil {
			return t
		}
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = readAllValueDepth(vv, seen, depth+1)
		}
		return out
	case map[any]any:
		if t == nil {
			return t
		}
		out := make(map[any]any, len(t))
		for k, vv := range t {
			out[k] = readAllValueDepth(vv, seen, depth+1)
		}
		return out
	default:
		// ReadAll 不复制 TTL，所以这里不带 TTL。
		if mv := mapFromAny(v); mv != nil {
			return detachMapValue(mv).readAllDepth(seen, depth+1)
		}
		if mm, ok := mapValueToVMap(v); ok {
			return mm.readAllDepth(seen, depth+1)
		}
		return v
	}
}

// sliceTo 合并「逐项转换、遇错即停」的切片循环。
func sliceTo[T any](in []T, conv func(T) (any, error)) ([]any, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]any, len(in))
	for i, v := range in {
		cv, err := conv(v)
		if err != nil {
			return nil, err
		}
		out[i] = cv
	}
	return out, nil
}

// MarshalJSON 转 JSON。只有键字符串冲突时才追加后缀，并保证后缀本身也不冲突。
// encoding/json 会排序对象键，输出不依赖 map 遍历顺序。
func (m *Map) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	if atomic.AddInt32(&m.marshaling, 1) > maxMarshalReentry {
		atomic.AddInt32(&m.marshaling, -1)
		return nil, errMarshalCycle
	}
	defer atomic.AddInt32(&m.marshaling, -1) // panic 时也要减回去。
	tree, err := m.marshalNode(make(map[*Map]struct{}), 0)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(tree)
	// 每一圈都会被 encoding/json 包一层 MarshalerError，
	// 1024 层嵌套的错误串很长。这里剥回同一个错误。
	if errors.Is(err, errMarshalCycle) {
		return nil, errMarshalCycle
	}
	return b, err
}

func (m *Map) marshalNode(seen map[*Map]struct{}, depth int) (any, error) {
	if m == nil {
		return nil, nil
	}
	if depth > maxNestDepth {
		return nil, errTooDeep("MarshalJSON 嵌套")
	}
	// 直接嵌套的 *Map 在这里展开，环由 seen 在第一圈发现。
	if _, ok := seen[m]; ok {
		return nil, fmt.Errorf("vmap: MarshalJSON 检测到循环引用")
	}
	seen[m] = struct{}{}
	defer delete(seen, m)
	items, err := m.captureItems()
	if err != nil {
		return nil, err
	}
	// keyString 只算一次。没有字符串冲突时不必排序，
	// encoding/json 会排对象键，后缀分配也不会发生。
	// 有冲突才排序，保证谁保留原字符串是稳定的；类型名只在字符串相等时格式化。
	//
	// 修改原因：原实现用 if len(items) > 1 {…} else if len(items) == 1 {…}
	// 两个分支，唯一的差别是「建不建去重表」，却把 keyString(items[i].key)
	// 这一行写了两遍（循环体内一次、单元素分支一次）。合并成一次循环：
	// seenKey 为 nil 表示不需要去重，跳过哈希表操作即可，结果与原来逐项相同：
	//   len==0 → ks 为空、dup=false；
	//   len==1 → ks[0] 写入同一表达式、dup=false；
	//   len>1  → 逐项写 ks 并检测重复。
	// 这样新增/修改键字符串的规则只会有一个落点。
	ks := make([]string, len(items))
	var seenKey map[string]struct{}
	if len(items) > 1 {
		seenKey = make(map[string]struct{}, len(items))
	}
	dup := false
	for i, it := range items {
		s := keyString(it.key)
		ks[i] = s
		if seenKey == nil {
			continue
		}
		if _, ok := seenKey[s]; ok {
			dup = true
			continue
		}
		seenKey[s] = struct{}{}
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	if dup {
		types := make([]string, len(items))
		for i, it := range items {
			types[i] = fmt.Sprintf("%T", it.key)
		}
		sort.SliceStable(order, func(a, b int) bool {
			ia, ib := order[a], order[b]
			if ks[ia] != ks[ib] {
				return ks[ia] < ks[ib]
			}
			return types[ia] < types[ib]
		})
	}
	mj := make(map[string]any, len(items))
	used := make(map[string]struct{}, len(items))
	for _, idx := range order {
		it := items[idx]
		k, err := disambiguate(ks[idx], it.key, used)
		if err != nil {
			return nil, err
		}
		mv, err := marshalValueDepth(it.val, seen, depth+1)
		if err != nil {
			return nil, err
		}
		mj[k] = mv
	}
	return mj, nil
}

func marshalValueDepth(v any, seen map[*Map]struct{}, depth int) (any, error) {
	if depth > maxNestDepth {
		return nil, errTooDeep("MarshalJSON 嵌套")
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case *Map:
		return t.marshalNode(seen, depth+1)
	case []any:
		if t == nil {
			return t, nil
		}
		// 本层已经加过深度，再传 depth+1 会让每层切片吃掉两级。
		return arraySubSeenDepth(t, seen, depth)
	case []*Map:
		if t == nil {
			return t, nil
		}
		return sliceTo(t, func(p *Map) (any, error) {
			return marshalValueDepth(p, seen, depth+1)
		})
	case map[string]any:
		if t == nil {
			return t, nil
		}
		out := make(map[string]any, len(t))
		for k, vv := range t {
			mv, err := marshalValueDepth(vv, seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = mv
		}
		return out, nil
	case map[any]any:
		if t == nil {
			// nil map[any]any 交给 encoding/json 会因键类型失败，返回 null。
			return nil, nil
		}
		mm, ok := mapValueToVMap(t)
		if !ok {
			return nil, nil
		}
		return mm.marshalNode(seen, depth+1)
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64, complex64, complex128:
		return v, nil
	default:
		if mv := mapFromAny(v); mv != nil {
			// 不带 TTL，避免编组期间启动定时器。
			return detachMapValue(mv).marshalNode(seen, depth+1)
		}
		return marshalReflectOrRaw(v, seen, depth)
	}
}

func marshalReflectOrRaw(v any, seen map[*Map]struct{}, depth int) (any, error) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return v, nil
	}
	switch rv.Kind() {
	case reflect.Map:
		// map[int]T、map[any]any 都不能直接交给 encoding/json，统一走字符串键。
		if mm, ok := mapValueToVMap(v); ok {
			return mm.marshalNode(seen, depth+1)
		}
		return v, nil
	case reflect.Slice, reflect.Array:
		// []byte 必须原样交给 encoding/json（base64）。
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return v, nil
		}
		if !typeHasMap(rv.Type()) {
			return v, nil
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return v, nil
		}
		n := rv.Len()
		out := make([]any, n)
		for i := 0; i < n; i++ {
			ev := rv.Index(i)
			if !ev.CanInterface() {
				out[i] = nil
				continue
			}
			mv, err := marshalValueDepth(ev.Interface(), seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = mv
		}
		return out, nil
	default:
		// 结构体原样交给 encoding/json，omitempty、内嵌字段等规则不变。
		// 字段里的 *Map 会重入 MarshalJSON，环由 marshaling 计数截断。
		return v, nil
	}
}

func arraySubSeenDepth(vs []any, seen map[*Map]struct{}, depth int) (any, error) {
	if vs == nil {
		return vs, nil
	}
	if depth > maxNestDepth {
		return nil, errTooDeep("MarshalJSON 嵌套")
	}
	return sliceTo(vs, func(v any) (any, error) {
		return marshalValueDepth(v, seen, depth+1)
	})
}

func disambiguate(base string, key any, used map[string]struct{}) (string, error) {
	if _, ok := used[base]; !ok {
		used[base] = struct{}{}
		return base, nil
	}
	// 固定后缀可能就是另一个真实键，json.Marshal 会静默覆盖。
	typ := fmt.Sprintf("%T", key)
	for n := 2; n < 1000000; n++ {
		cand := fmt.Sprintf("%s#%d(%s)", base, n, typ)
		if _, ok := used[cand]; !ok {
			used[cand] = struct{}{}
			return cand, nil
		}
	}
	return "", fmt.Errorf("vmap: JSON 键冲突过多")
}

func keyString(k any) string {
	switch x := k.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int8:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint8:
		return strconv.FormatUint(uint64(x), 10)
	case uint16:
		return strconv.FormatUint(uint64(x), 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case uintptr:
		return strconv.FormatUint(uint64(x), 10)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case *Map:
		if x == nil {
			return "<nil>"
		}
		// fmt.Sprint(*Map) 会调 String 并加锁，键名也不该是整表 JSON。
		return fmt.Sprintf("vmap.Map(%p)", x)
	default:
		return safeSprint(k)
	}
}

// safeSprint 包装 fmt.Sprint，防止用户代码 panic 逃逸到 JSON 编组流程。
func safeSprint(v any) (s string) {
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
			s = fmt.Sprintf("%v", reflect.TypeOf(v))
		}
	}()
	return fmt.Sprint(v)
}

// mapValueToVMap 把任意 map 收成临时 *Map。不走 setLocked，避免编组期间启动定时器。
func mapValueToVMap(v any) (*Map, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Map || rv.IsNil() {
		return nil, false
	}
	mm := NewMapSize(rv.Len())
	for _, k := range rv.MapKeys() {
		if !k.CanInterface() {
			continue
		}
		key := k.Interface()
		if !validKey(key) {
			continue
		}
		ev := rv.MapIndex(k)
		var vv any
		if ev.IsValid() && ev.CanInterface() {
			vv = ev.Interface()
		}
		mm.indexPut(key, vv)
	}
	return mm, true
}

// keepClone 优先写入克隆结果。
// 切片/数组元素 valueFor 失败时直接 continue 会留下零值，等于把数据清空。
func keepClone(dst, ev reflect.Value, cloned any) {
	if !dst.IsValid() || !dst.CanSet() {
		return
	}
	et := dst.Type()
	vv, err := valueFor(cloned, et)
	if err == nil && vv.IsValid() {
		dst.Set(vv)
		return
	}
	if ev.IsValid() && ev.CanInterface() {
		orig := reflect.ValueOf(ev.Interface())
		if orig.IsValid() && orig.Type().AssignableTo(et) {
			dst.Set(orig)
		}
	}
}

func unarraySubDepth(vs []any, depth int) (any, error) {
	if vs == nil {
		return vs, nil
	}
	return sliceTo(vs, func(v any) (any, error) {
		return parseJSONValue(v, depth+1)
	})
}

// UnmarshalJSON 要求 JSON 对象。null 会清空。解析失败不修改接收者。
func (m *Map) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("vmap: UnmarshalJSON nil Map")
	}
	tmp, err := mapFromJSON(data)
	if err != nil {
		return err
	}
	var calls []call
	defer func() { goCalls(calls) }()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	calls = m.takeExpiredLocked()
	m.adoptLocked(tmp)
	return nil
}

func mapFromJSON(data []byte) (*Map, error) {
	var mjs map[string]any
	if err := json.Unmarshal(data, &mjs); err != nil {
		return nil, err
	}
	tmp := NewMapSize(len(mjs))
	if err := tmp.unmarshalJSONLocked(mjs, 0); err != nil {
		return nil, err
	}
	return tmp, nil
}

func (m *Map) adoptLocked(tmp *Map) {
	for i := range m.keys {
		m.keys[i] = nil
	}
	m.data = tmp.data
	m.keys = tmp.keys
	m.keyIndex = tmp.keyIndex
	m.expired = tmp.expired
	m.inited = true
	tmp.data = nil
	tmp.keys = nil
	tmp.keyIndex = nil
	tmp.expired = nil
}

func (m *Map) unmarshalJSONLocked(mjs map[string]any, depth int) error {
	if depth > maxNestDepth {
		return errTooDeep("JSON 嵌套")
	}
	for k, v := range mjs {
		parsed, err := parseJSONValue(v, depth+1)
		if err != nil {
			return err
		}
		m.setLocked(k, parsed)
	}
	return nil
}

func parseJSONValue(v any, depth int) (any, error) {
	if v == nil {
		return nil, nil
	}
	if depth > maxNestDepth {
		return nil, errTooDeep("JSON 嵌套")
	}
	switch t := v.(type) {
	case map[string]any:
		sub := NewMapSize(len(t))
		if err := sub.unmarshalJSONLocked(t, depth); err != nil {
			return nil, err
		}
		return sub, nil
	case []any:
		return unarraySubDepth(t, depth)
	default:
		rv := reflect.ValueOf(v)
		if !rv.IsValid() {
			return nil, nil
		}
		// *Map 必须在 inDirect 解引用之前捕获，否则会被解成
		// Map 值再被 normalizeVal 复制；JSON 路径本身不会出现 *Map，
		// 但保留此分支与其他路径 (ReadFrom) 的语义一致，避免上游把
		// *Map 塞进 default 时悄悄复制。
		if rv.CanInterface() {
			if mp, ok := rv.Interface().(*Map); ok {
				return mp, nil
			}
		}
		rv = inDirect(rv)
		if !rv.IsValid() {
			return nil, fmt.Errorf("vmap: 指针环或间接层数环路")
		}
		// 多层指针解包后可能停在 *Map，不能再掉回原始值。
		if rv.CanInterface() {
			if mp, ok := rv.Interface().(*Map); ok {
				return mp, nil
			}
		}
		switch rv.Kind() {
		case reflect.Map:
			sub := NewMap()
			// JSON 解析路径不涉及 *Map 克隆，repl、made 传 nil
			// 保持旧的"保留引用"语义（readFromLocked 在 repl==nil 时行为不变）。
			if err := sub.readFromLocked(rv, make(map[uintptr]*Map), nil, nil, depth); err != nil {
				return nil, err
			}
			return sub, nil
		case reflect.Slice, reflect.Array:
			return parseSlice(rv, make(map[uintptr]*Map), nil, nil, depth)
		default:
			return v, nil
		}
	}
}

// WriteTo 把内容写入 mm。mm 必须是 map，或指向 map 的指针。
// nil map 且可设置时会自动 make。接口元素里的 *Map 仍写成 map[any]any。
func (m *Map) WriteTo(mm any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("vmap: WriteTo panic: %v", r)
		}
	}()
	if m == nil {
		return fmt.Errorf("vmap: WriteTo nil Map")
	}
	rv := inDirect(reflect.ValueOf(mm))
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return errNotMap(rv.Kind())
	}
	if rv.IsNil() {
		if !rv.CanSet() {
			return fmt.Errorf("Map: 目标 map 为 nil")
		}
		rv.Set(reflect.MakeMap(rv.Type()))
	}
	return m.writeTo(rv)
}

// writeTo 保留旧签名。
func (m *Map) writeTo(rv reflect.Value) error {
	return m.writeToSeen(rv, make(map[*Map]struct{}), 0)
}

func (m *Map) writeToSeen(rv reflect.Value, seen map[*Map]struct{}, depth int) error {
	if m == nil {
		return nil
	}
	if depth > maxNestDepth {
		return errTooDeep("WriteTo 嵌套")
	}
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return errNotMap(rv.Kind())
	}
	if rv.IsNil() {
		return fmt.Errorf("Map: 目标 map 为 nil")
	}
	if seen == nil {
		seen = make(map[*Map]struct{})
	}
	if _, ok := seen[m]; ok {
		return fmt.Errorf("vmap: WriteTo 检测到循环引用")
	}
	seen[m] = struct{}{}
	defer delete(seen, m)
	items, err := m.captureItems()
	if err != nil {
		return err
	}
	kt, et := rv.Type().Key(), rv.Type().Elem()
	// 会变成同一个键，SetMapIndex 静默覆盖。目标键类型必然可比较。
	// 全部转换成功后才写入，冲突或转换失败时不改目标。
	used := make(map[any]struct{}, len(items))
	type pair struct {
		k, v reflect.Value
	}
	pairs := make([]pair, 0, len(items))
	for _, it := range items {
		kv, err := adaptKey(it.key, kt)
		if err != nil {
			return err
		}
		ck := kv.Interface()
		if _, dup := used[ck]; dup {
			return fmt.Errorf("vmap: WriteTo 键冲突 %#v (from %T)", ck, it.key)
		}
		used[ck] = struct{}{}
		vv, err := adaptVal(it.val, et, seen, depth+1)
		if err != nil {
			return err
		}
		pairs = append(pairs, pair{kv, vv})
	}
	for _, p := range pairs {
		if err := setMapIndex(rv, p.k, p.v); err != nil {
			return err
		}
	}
	return nil
}

func adaptKey(key any, kt reflect.Type) (reflect.Value, error) {
	if key == nil {
		switch kt.Kind() {
		case reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
			return reflect.Zero(kt), nil
		default:
			return reflect.Value{}, fmt.Errorf("vmap: nil 不能作为键赋给 %v", kt)
		}
	}
	if kt.Kind() == reflect.String {
		if s, ok := key.(string); ok {
			return reflect.ValueOf(s).Convert(kt), nil
		}
		return reflect.ValueOf(keyString(key)).Convert(kt), nil
	}
	return convertAssign(reflect.ValueOf(key), kt)
}

func adaptVal(val any, et reflect.Type, seen map[*Map]struct{}, depth int) (reflect.Value, error) {
	if depth > maxNestDepth {
		return reflect.Value{}, errTooDeep("嵌套")
	}
	switch x := val.(type) {
	case *Map:
		return adaptMap(x, et, seen, depth)
	case []*Map:
		return adaptMapSlice(x, et, seen, depth)
	case []any:
		return adaptAnySlice(x, et, seen, depth)
	}
	if mv := mapFromAny(val); mv != nil {
		return adaptMap(detachMapValue(mv), et, seen, depth)
	}
	if val == nil {
		return nilValue(et)
	}
	// 写到 interface{} 时，map 里的 *Map 也要展开，不能整张 map 原样塞进去。
	if et.Kind() == reflect.Interface {
		pv, err := plainValue(val, seen, depth)
		if err != nil {
			return reflect.Value{}, err
		}
		if pv == nil {
			return nilValue(et)
		}
		return convertAssign(reflect.ValueOf(pv), et)
	}
	return convertAssign(reflect.ValueOf(val), et)
}

func adaptMap(mp *Map, et reflect.Type, seen map[*Map]struct{}, depth int) (reflect.Value, error) {
	if depth > maxNestDepth {
		return reflect.Value{}, errTooDeep("嵌套")
	}
	if mp == nil {
		return nilValue(et)
	}
	if et.Kind() == reflect.Interface {
		sub := make(map[any]any)
		if err := mp.writeToSeen(reflect.ValueOf(sub), seen, depth); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(sub), nil
	}
	pv := reflect.ValueOf(mp)
	if pv.Type().AssignableTo(et) || pv.Type().ConvertibleTo(et) {
		return convertAssign(pv, et)
	}
	if et.Kind() == reflect.Map {
		sub := reflect.MakeMap(et)
		if err := mp.writeToSeen(sub, seen, depth); err != nil {
			return reflect.Value{}, err
		}
		return sub, nil
	}
	if et.Kind() == reflect.Ptr && et.Elem().Kind() == reflect.Map {
		ptr := reflect.New(et.Elem())
		if err := mp.writeToSeen(ptr.Elem(), seen, depth); err != nil {
			return reflect.Value{}, err
		}
		return ptr, nil
	}
	return reflect.Value{}, fmt.Errorf("vmap: *Map 无法写入 %v", et)
}

func adaptMapSlice(vs []*Map, et reflect.Type, seen map[*Map]struct{}, depth int) (reflect.Value, error) {
	if vs == nil {
		// nil []*Map 与 nil []any 走同一套目标类型规则，避免数组目标误报「不能赋 nil」。
		return adaptAnySlice(nil, et, seen, depth)
	}
	anys := make([]any, len(vs))
	for i, p := range vs {
		anys[i] = p
	}
	return adaptAnySlice(anys, et, seen, depth)
}

func adaptAnySlice(vs []any, et reflect.Type, seen map[*Map]struct{}, depth int) (reflect.Value, error) {
	if depth > maxNestDepth {
		return reflect.Value{}, errTooDeep("嵌套")
	}
	if vs == nil {
		switch et.Kind() {
		case reflect.Interface, reflect.Slice:
			// typed nil 写到 slice/interface 应保持 null，不能 make 成空切片。
			return nilValue(et)
		}
		// 数组仍走长度校验（len==0 才匹配零长数组）。
	}
	if et.Kind() == reflect.Interface {
		out := make([]any, len(vs))
		for i, el := range vs {
			pv, err := plainValue(el, seen, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			out[i] = pv
		}
		return reflect.ValueOf(out), nil
	}
	if et.Kind() == reflect.Slice || et.Kind() == reflect.Array {
		if et.Kind() == reflect.Array && et.Len() != len(vs) {
			return reflect.Value{}, fmt.Errorf("vmap: 数组长度 %d 与值长度 %d 不一致", et.Len(), len(vs))
		}
		var dst reflect.Value
		if et.Kind() == reflect.Slice {
			dst = reflect.MakeSlice(et, len(vs), len(vs))
		} else {
			dst = reflect.New(et).Elem()
		}
		for i, el := range vs {
			ev, err := adaptVal(el, et.Elem(), seen, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			if err := assignTo(dst.Index(i), ev); err != nil {
				return reflect.Value{}, err
			}
		}
		return dst, nil
	}
	return convertAssign(reflect.ValueOf(vs), et)
}

func plainValue(val any, seen map[*Map]struct{}, depth int) (any, error) {
	if depth > maxNestDepth {
		return nil, errTooDeep("嵌套")
	}
	switch x := val.(type) {
	case *Map:
		if x == nil {
			return nil, nil
		}
		sub := make(map[any]any)
		if err := x.writeToSeen(reflect.ValueOf(sub), seen, depth); err != nil {
			return nil, err
		}
		return sub, nil
	case []*Map:
		if x == nil {
			return nil, nil
		}
		return sliceTo(x, func(p *Map) (any, error) {
			return plainValue(p, seen, depth+1)
		})
	case []any:
		if x == nil {
			return nil, nil
		}
		return sliceTo(x, func(p any) (any, error) {
			return plainValue(p, seen, depth+1)
		})
	case map[string]any:
		if x == nil {
			return nil, nil
		}
		// 字符串键保持 map[string]any。
		out := make(map[string]any, len(x))
		for k, vv := range x {
			pv, err := plainValue(vv, seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = pv
		}
		return out, nil
	case map[any]any:
		if x == nil {
			return nil, nil
		}
		out := make(map[any]any, len(x))
		for k, vv := range x {
			pv, err := plainValue(vv, seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = pv
		}
		return out, nil
	default:
		// 按值 Map 不能原样写进接口。
		if mv := mapFromAny(val); mv != nil {
			return plainValue(detachMapValue(mv), seen, depth+1)
		}
		rv := reflect.ValueOf(val)
		if rv.IsValid() && rv.Kind() == reflect.Map && !rv.IsNil() && typeMayContainMap(rv.Type()) {
			return plainMap(rv, seen, depth)
		}
		return val, nil
	}
}

func plainMap(rv reflect.Value, seen map[*Map]struct{}, depth int) (any, error) {
	if rv.Type().Key().Kind() == reflect.String {
		out := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			ev := rv.MapIndex(k)
			var el any
			if ev.IsValid() && ev.CanInterface() {
				el = ev.Interface()
			}
			pv, err := plainValue(el, seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[k.String()] = pv
		}
		return out, nil
	}
	out := make(map[any]any, rv.Len())
	for _, k := range rv.MapKeys() {
		if !k.CanInterface() {
			continue
		}
		kk := k.Interface()
		if !validKey(kk) {
			continue
		}
		ev := rv.MapIndex(k)
		var el any
		if ev.IsValid() && ev.CanInterface() {
			el = ev.Interface()
		}
		pv, err := plainValue(el, seen, depth+1)
		if err != nil {
			return nil, err
		}
		out[kk] = pv
	}
	return out, nil
}

func nilValue(et reflect.Type) (reflect.Value, error) {
	switch et.Kind() {
	case reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return reflect.Zero(et), nil
	default:
		return reflect.Value{}, fmt.Errorf("vmap: nil 不能赋给 %v", et)
	}
}

func convertAssign(v reflect.Value, dst reflect.Type) (reflect.Value, error) {
	if !v.IsValid() {
		return nilValue(dst)
	}
	if v.Type() == dst || v.Type().AssignableTo(dst) {
		return v, nil
	}
	if !v.Type().ConvertibleTo(dst) {
		return reflect.Value{}, fmt.Errorf("vmap: 类型 %v 无法赋给 %v", v.Type(), dst)
	}
	if err := checkConvert(v, dst); err != nil {
		return reflect.Value{}, err
	}
	return v.Convert(dst), nil
}

// checkConvert 使用类型位宽，不写死 32/64。int 和 uintptr 在 386 与 amd64 上不同。
// 同时禁止数值/布尔到 string 的隐式转换（Go 的 int -> string 是 rune 转换）。
func checkConvert(v reflect.Value, dst reflect.Type) error {
	// Go 1.20 起切片可转 [N]T / *[N]T。长度不足 Convert 会 panic，超长会静默截断。
	if v.Kind() == reflect.Slice {
		if n := arrayLen(dst); n >= 0 && v.Len() != n {
			return fmt.Errorf("vmap: 切片长度 %d 与 %v 长度 %d 不一致", v.Len(), dst, n)
		}
	}
	if dst.Kind() == reflect.String {
		switch v.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
			reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Bool:
			return fmt.Errorf("vmap: 类型 %v 不能隐式转换为 string", v.Type())
		}
	}
	if v.Kind() == reflect.Complex128 && dst.Kind() == reflect.Complex64 {
		c := v.Complex()
		r, im := real(c), imag(c)
		if math.IsNaN(r) || math.IsNaN(im) || math.IsInf(r, 0) || math.IsInf(im, 0) {
			return fmt.Errorf("vmap: 非有限复数不能赋给 %s", dst)
		}
		limit := float64(math.MaxFloat32)
		if math.Abs(r) > limit || math.Abs(im) > limit {
			return fmt.Errorf("vmap: 复数分量溢出 %v -> %s", c, dst)
		}
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := v.Int()
		switch dst.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if dst.Bits() < 64 {
				max := int64(1)<<uint(dst.Bits()-1) - 1
				min := -max - 1
				if i < min || i > max {
					return fmt.Errorf("vmap: 整数溢出 %d -> %s", i, dst)
				}
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if i < 0 {
				return fmt.Errorf("vmap: 负整数不能赋给 %s", dst)
			}
			if dst.Bits() < 64 && uint64(i) > (uint64(1)<<uint(dst.Bits()))-1 {
				return fmt.Errorf("vmap: 整数溢出 %d -> %s", i, dst)
			}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := v.Uint()
		switch dst.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if dst.Bits() < 64 && u > (uint64(1)<<uint(dst.Bits()))-1 {
				return fmt.Errorf("vmap: 整数溢出 %d -> %s", u, dst)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			var max uint64
			if dst.Bits() >= 64 {
				max = uint64(math.MaxInt64)
			} else {
				max = uint64(1)<<uint(dst.Bits()-1) - 1
			}
			if u > max {
				return fmt.Errorf("vmap: 整数溢出 %d -> %s", u, dst)
			}
		}
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("vmap: 非有限浮点数 %v 不能赋给 %s", f, dst)
		}
		if isIntegerKind(dst.Kind()) && f != math.Trunc(f) {
			return fmt.Errorf("vmap: 浮点数 %v 含小数部分，不能赋给 %s", f, dst)
		}
		switch dst.Kind() {
		case reflect.Float32:
			if f < -math.MaxFloat32 || f > math.MaxFloat32 {
				return fmt.Errorf("vmap: 浮点数溢出 %v -> %s", f, dst)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if dst.Bits() >= 64 {
				// float64(1<<63-1) 会舍入成 2^63，
				// 用 f >= 2^63 才正确拒绝 2^63；负边界 -2^63 是 int64 可取的最小值。
				if f < -float64(1<<63) || f >= float64(1<<63) {
					return fmt.Errorf("vmap: 浮点数溢出 %v -> %s", f, dst)
				}
			} else {
				bits := uint(dst.Bits() - 1)
				maxF := float64(int64(1)<<bits - 1)
				minF := -float64(int64(1) << bits)
				if f < minF || f > maxF {
					return fmt.Errorf("vmap: 浮点数溢出 %v -> %s", f, dst)
				}
			}
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if f < 0 {
				return fmt.Errorf("vmap: 负浮点数不能赋给 %s", dst)
			}
			if dst.Bits() >= 64 {
				// float64(1<<64-1) 舍入成 2^64，
				// 用 f >= 2^64 才正确拒绝 2^64。
				if f >= float64(1<<64) {
					return fmt.Errorf("vmap: 浮点数溢出 %v -> %s", f, dst)
				}
			} else {
				maxF := float64(uint64(1)<<uint(dst.Bits()) - 1)
				if f > maxF {
					return fmt.Errorf("vmap: 浮点数溢出 %v -> %s", f, dst)
				}
			}
		}
	}
	return nil
}

// arrayLen 返回数组或数组指针的长度，其它类型返回 -1。
func arrayLen(t reflect.Type) int {
	switch {
	case t.Kind() == reflect.Array:
		return t.Len()
	case t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Array:
		return t.Elem().Len()
	}
	return -1
}

// isIntegerKind 从 Int 到 Uintptr 的 Kind 值是连续的。
func isIntegerKind(k reflect.Kind) bool {
	return k >= reflect.Int && k <= reflect.Uintptr
}

func setMapIndex(dst, key, val reflect.Value) (err error) {
	if !key.IsValid() || !val.IsValid() {
		return fmt.Errorf("vmap: 无效的 map 索引")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("vmap: SetMapIndex panic: %v", r)
		}
	}()
	dst.SetMapIndex(key, val)
	return nil
}

func assignTo(dst, src reflect.Value) (err error) {
	if !src.IsValid() {
		return fmt.Errorf("vmap: 无法写入无效值")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("vmap: 赋值 panic: %v", r)
		}
	}()
	if src.Type().AssignableTo(dst.Type()) {
		dst.Set(src)
		return nil
	}
	if src.Type().ConvertibleTo(dst.Type()) {
		if err = checkConvert(src, dst.Type()); err != nil {
			return err
		}
		dst.Set(src.Convert(dst.Type()))
		return nil
	}
	return fmt.Errorf("vmap: 无法将 %v 写入 %v", src.Type(), dst.Type())
}

// ReadFrom 从 map 合并到当前 Map，不先清空。需要替换时先 Reset。
func (m *Map) ReadFrom(mm any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("vmap: ReadFrom panic: %v", r)
		}
	}()
	if m == nil {
		return fmt.Errorf("vmap: ReadFrom nil Map")
	}
	rv := inDirect(reflect.ValueOf(mm))
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return errNotMap(rv.Kind())
	}
	repl := map[*Map]*Map{m: m}
	made := map[*Map]struct{}{}
	tmp := NewMapSize(rv.Len())
	if err := tmp.readFromLocked(rv, make(map[uintptr]*Map), repl, made, 0); err != nil {
		return err
	}
	// tmp 是局部表，未发布；itemsLocked 返回的 exp 恒为 nil（readFromLocked 不写 TTL）。
	m.commitItems(tmp.itemsLocked(), true, false, made)
	return nil
}

func mapPtr(rv reflect.Value) uintptr {
	if rv.IsValid() && rv.Kind() == reflect.Map && !rv.IsNil() {
		return rv.Pointer()
	}
	return 0
}

// readFromLocked 必须持有调用者自己的写锁（或对独占的 Map 调用）。
//   - repl == nil：旧的"保留引用"语义（readFrom、JSON 解析路径）。
//   - repl != nil：遇到 *Map 时调 cloneMap 深拷贝。
//
// 注意：repl != nil 时本函数不能在持外部 Map 锁的上下文里调用。
func (m *Map) readFromLocked(rv reflect.Value, seen map[uintptr]*Map, repl map[*Map]*Map, made map[*Map]struct{}, depth int) error {
	if !rv.IsValid() || rv.Kind() != reflect.Map || rv.IsNil() {
		return nil
	}
	if depth > maxNestDepth {
		return errTooDeep("ReadFrom 嵌套")
	}
	if seen == nil {
		seen = make(map[uintptr]*Map)
	}
	for _, key := range rv.MapKeys() {
		var k any
		if key.CanInterface() {
			k = key.Interface()
		} else {
			k = typeSelect(key)
		}
		if !validKey(k) {
			return fmt.Errorf("vmap: ReadFrom 遇到不可比较键 %T，已中止且未修改目标", k)
		}
		parsed, err := parseReflect(rv.MapIndex(key), seen, repl, made, depth+1)
		if err != nil {
			return err
		}
		m.setLocked(k, parsed)
	}
	return nil
}

func mapPtrFrom(val reflect.Value) (*Map, bool) {
	if !val.IsValid() || !val.CanInterface() {
		return nil, false
	}
	mp, ok := val.Interface().(*Map)
	return mp, ok
}

func reuseOrClone(mp *Map, repl map[*Map]*Map, made map[*Map]struct{}, depth int) (any, error) {
	if mp == nil {
		return nil, nil
	}
	if repl != nil {
		// ReadFrom 克隆 *Map 必须带上当前嵌套深度，与 Copy 共用同一上限。
		return cloneMap(mp, repl, made, depth), nil
	}
	return mp, nil
}

func parseReflect(val reflect.Value, seen map[uintptr]*Map, repl map[*Map]*Map, made map[*Map]struct{}, depth int) (any, error) {
	if !val.IsValid() {
		return nil, nil
	}
	if depth > maxNestDepth {
		return nil, errTooDeep("ReadFrom 嵌套")
	}
	// 必须在 inDirect 解引用之前判断，否则 *Map 会被解成 Map 值。
	if mp, ok := mapPtrFrom(val); ok {
		return reuseOrClone(mp, repl, made, depth)
	}
	rvi := inDirect(val)
	if !rvi.IsValid() {
		return nil, fmt.Errorf("vmap: 指针环或间接层数环路")
	}
	// inDirect 停在 *Map，多层指针解包后仍要克隆或保留原指针。
	if mp, ok := mapPtrFrom(rvi); ok {
		return reuseOrClone(mp, repl, made, depth)
	}
	if mv := mapValuePtr(rvi); mv != nil {
		// 交给 typeSelect 会按值返回，并和源嵌套表别名。
		if repl != nil {
			return cloneMapVal(mv, repl, made, depth), nil
		}
		return detachMapValue(mv), nil
	}
	switch rvi.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		if rvi.IsNil() {
			if rvi.CanInterface() {
				return rvi.Interface(), nil
			}
			return nil, nil
		}
	}
	switch rvi.Kind() {
	case reflect.Map:
		p := mapPtr(rvi)
		if prev, ok := seen[p]; ok && p != 0 {
			return prev, nil
		}
		mm := NewMapSize(rvi.Len())
		if p != 0 {
			seen[p] = mm
		}
		// Go map 转出的子表是本次新建的。不登记进 made 的话，
		// ReadFrom 的 activateTree 会把它当成共享原表跳过，里面克隆出的
		// *Map 已登记 TTL，定时器却永不启动。JSON 路径 made 为 nil，不登记。
		if made != nil {
			made[mm] = struct{}{}
		}
		if err := mm.readFromLocked(rvi, seen, repl, made, depth); err != nil {
			return nil, err
		}
		return mm, nil
	case reflect.Slice, reflect.Array:
		return parseSlice(rvi, seen, repl, made, depth)
	default:
		if repl != nil && rvi.CanInterface() {
			iv := rvi.Interface()
			// ReadFrom 在 repl!=nil 时应深拷贝切片/结构体，避免与源共享底层数组。
			if typeNeedsDeepCopy(rvi.Type()) {
				return cloneAnyDepth(iv, repl, made, depth), nil
			}
			return iv, nil
		}
		return typeSelect(rvi), nil
	}
}

func parseSlice(rv reflect.Value, seen map[uintptr]*Map,
	repl map[*Map]*Map, made map[*Map]struct{}, depth int,
) (any, error) {
	if depth > maxNestDepth {
		return nil, errTooDeep("切片嵌套")
	}
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		if rv.CanInterface() {
			return rv.Interface(), nil
		}
		return nil, nil
	}
	if repl != nil && rv.CanInterface() {
		// 与 Clone 共用 cloneAnyDepth，保留 []*Map 等具体切片类型并深拷贝元素。
		return cloneAnyDepth(rv.Interface(), repl, made, depth), nil
	}
	if rv.Kind() == reflect.Slice && !sliceNeedsWalk(rv.Type()) {
		copied := copySliceValue(rv)
		if copied != nil {
			return copied, nil
		}
	}
	if rv.Kind() == reflect.Array && !sliceNeedsWalk(rv.Type()) && rv.CanInterface() {
		return rv.Interface(), nil
	}
	n := rv.Len()
	out := make([]any, n)
	for i := 0; i < n; i++ {
		el, err := parseReflect(rv.Index(i), seen, repl, made, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = el
	}
	return out, nil
}

func sliceNeedsWalk(t reflect.Type) bool {
	if t == nil || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) {
		return false
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return false
	}
	// 元素是切片/map 时也要深入，否则 [][]int 只拷外层头。
	return typeNeedsDeepCopy(t.Elem())
}

var (
	mapPtrType = reflect.TypeOf((*Map)(nil))
	// reflect.TypeOf(Map{}) 会把含 RWMutex 的值装箱，vet copylocks 会报。
	mapValType = mapPtrType.Elem()
)

type typeQuery uint8

const (
	qContainMap typeQuery = iota // 激活：是否可能含 *Map/Map，不把 map[string]int 当成需要扫描。
	qHasMap                      // JSON：任意 map、*Map、interface 都要逐项走。
	qDeepCopy                    // 深拷贝：含 slice/map/interface 就要复制，避免共享底层数组。
	qCount
)

var typeQueryCache [qCount]sync.Map

func typeQueryCached(t reflect.Type, q typeQuery) bool {
	if t == nil {
		return false
	}
	if v, ok := typeQueryCache[q].Load(t); ok {
		b, _ := v.(bool)
		return b
	}
	b := typeWalk(t, q, make(map[reflect.Type]struct{}))
	typeQueryCache[q].Store(t, b)
	return b
}

func typeWalk(t reflect.Type, q typeQuery, stack map[reflect.Type]struct{}) bool {
	if t == nil {
		return false
	}
	if t == mapPtrType || t == mapValType {
		return true
	}
	if _, ok := stack[t]; ok {
		return false
	}
	stack[t] = struct{}{}
	defer delete(stack, t)
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Map:
		if q != qContainMap {
			return true
		}
		// 激活阶段不因为「本身是 map」就返回 true，只看元素里有没有 *Map。
		return typeWalk(t.Elem(), q, stack)
	case reflect.Slice:
		if q == qDeepCopy {
			return true
		}
		// []byte 没有 map，避免把 base64 切片拆开。数组不走这个特例，与原来一致。
		if q == qHasMap && t.Elem().Kind() == reflect.Uint8 {
			return false
		}
		return typeWalk(t.Elem(), q, stack)
	case reflect.Ptr, reflect.Array:
		return typeWalk(t.Elem(), q, stack)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if typeWalk(t.Field(i).Type, q, stack) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func typeMayContainMap(t reflect.Type) bool { return typeQueryCached(t, qContainMap) }

// typeHasMap 为 JSON 编组服务：类型树里只要有 map、*Map 或 interface 就需要逐项走。
func typeHasMap(t reflect.Type) bool { return typeQueryCached(t, qHasMap) }

func typeNeedsDeepCopy(t reflect.Type) bool { return typeQueryCached(t, qDeepCopy) }

// String JSON 文本。nil 接收者为 "null"。失败时返回带错误的占位串，而不是空串。
func (m *Map) String() (s string) {
	if m == nil {
		return "null"
	}
	defer func() {
		if r := recover(); r != nil {
			reportPanic(r)
			s = fmt.Sprintf("vmap.Map(panic: %v)", r)
		}
	}()
	b, err := m.MarshalJSON()
	if err != nil {
		return fmt.Sprintf("vmap.Map(marshal error: %v)", err)
	}
	return string(b)
}
