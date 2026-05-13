package vmap

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"
)

type timer struct {
	*time.Timer
	fn func(any)
}

func (t *timer) exec(val any) {
	if t.fn != nil {
		t.fn(val)
	}
}

// Map 推荐使用NewMap来规范的创建，这样可避免不必要的错误。
type Map struct {
	m        sync.Map
	expired  map[any]*timer
	keyIndex map[any]int // 新增：O(1) 查找位置
	keys     []any
	length   int
	mu       sync.RWMutex // 读写锁优化
}

// NewMap 对象
//
//	*Map      Map对象
func NewMap() *Map {
	return &Map{
		expired:  make(map[any]*timer),
		keyIndex: make(map[any]int),
	}
}

// Len 长度
//
//	int   长度
func (m *Map) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.length
}

// Set 设置，如果你设置的值是Map，将会被强制初始化该值。这样避免读取并调用时候出错。
//
//	key any   键名
//	val any   值
func (m *Map) Set(key, val any) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.keyIndex[key]; !exists {
		m.keys = append(m.keys, key)
		m.keyIndex[key] = len(m.keys) - 1
		m.length++
	}

	m.m.Store(key, val)
}

// Get 读取
//
//	key any   读取值的键名
//	any       读取值
func (m *Map) Get(key any) any {
	if v, ok := m.m.Load(key); ok {
		return v
	}
	return nil
}

// GetHas 读取并判断
//
//	key any   键名
//	val any   读取值
//	ok bool           判断，如果为true,判断存在。否则为flase
func (m *Map) GetHas(key any) (any, bool) {
	return m.m.Load(key)
}

// Has 判断
//
//	key any   键名
//	bool              判断，如果为true,判断存在。否则为flase
func (m *Map) Has(key any) bool {
	m.mu.RLock()
	_, ok := m.keyIndex[key]
	m.mu.RUnlock()
	return ok
}

// GetOrDefault 读取，如果不存，返回默认值
//
//	key any   	键名
//	def any		默认值
//	val any   	读取值
func (m *Map) GetOrDefault(key any, def any) any {
	if v, ok := m.m.Load(key); ok && v != nil {
		return v
	}
	return def
}

// ==================== 嵌套 Map 操作 ====================

// New 增加一个key对应的Map对象，并返回该对象。
// .New 不管是否存在该 key 键值，都会写入一个新的Map覆盖 key 键值。
// 要是你需要一个这样的功能，存在key键值，返回该key键值。如果不存在该key键值，返回一个新的key对应的Map。请使用 .GetNewMap 方法
//
//	key any     键名
//	*Map                Map对象
func (m *Map) New(key any) *Map {
	t := NewMap()
	m.Set(key, t)
	return t
}

// GetNewMap 如果不存在增加一个key对应的Map对象，否则读取并返回该对象。
//
//	key any     键名
//	*Map                Map对象
func (m *Map) GetNewMap(key any) *Map {
	if v, ok := m.GetHas(key); ok {
		if mm, ok := v.(*Map); ok {
			return mm
		}
	}
	return m.New(key)
}

// GetNewMaps 如果不存在增加一个key对应的Map对象，否则读取并返回该对象。
// 他支持链式读取或创建，如果你想相独读取，可以使用 .Index 方法
//
//	key ...any     键名
//	*Map                Map对象
func (m *Map) GetNewMaps(keys ...any) *Map {
	tm := m
	for _, k := range keys {
		tm = tm.GetNewMap(k)
	}
	return tm
}

// Index 指定读取，这个仅能使用于 *Map 中有子 *Map。功能用于快速索引定位。
//
//	参：
//	  key ...any        快速指定父子关系中的值，如 .Index("A", "B", "C")
//	返：
//	  any               读取值
func (m *Map) Index(key ...any) any {
	v, _ := m.IndexHas(key...)
	return v
}

// IndexHas 指定读取和判断，这个只有使用于 *Map 中有子 *Map。功能用于快速索引定位。
//
//	key ...any        快速指定父子关系中的值，如 .Index("A", "B", "C")
//	any               读取值
//	bool                      判断，如果为true，表示存在。否则为flase
//	例：
//	  m1 := birdswo.NewMap()
//	  m2 := birdswo.NewMap()
//	  m2.Set("b", "value")
//	  m1.Set("a", m2)
//	  v, ok := m1.IndexHas("a", "b")
//	  fmt.Println(v, ok)
//	  //value true
func (m *Map) IndexHas(keys ...any) (any, bool) {
	if len(keys) == 0 {
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
		if mm, ok := v.(*Map); ok {
			cur = mm
		} else {
			return nil, false
		}
	}
	return nil, false
}

// ==================== 删除 & 过期 ====================

// Del 删除
//
//	key any   键名
func (m *Map) Del(key any) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx, exists := m.keyIndex[key]
	if !exists {
		return
	}

	// 删除 keys 和 index
	lastIdx := len(m.keys) - 1
	lastKey := m.keys[lastIdx]

	m.keys[idx] = lastKey
	m.keyIndex[lastKey] = idx
	delete(m.keyIndex, key)
	m.keys = m.keys[:lastIdx]
	m.length--

	val, _ := m.m.LoadAndDelete(key)

	if t, ok := m.expired[key]; ok {
		delete(m.expired, key)
		t.Stop()
		go t.exec(val)
	}
}

// Dels 删除
//
//	keys []any   键名
func (m *Map) Dels(keys []any) {
	for _, k := range keys {
		m.Del(k)
	}
}

// SetExpired 单个键值的有效期
//
//	key any		键名
//	d time.Duration		时间
func (m *Map) SetExpired(key any, d time.Duration) {
	m.SetExpiredCall(key, d, nil)
}

// SetExpiredCall 单个键值的有效期，过期后并调用函数
//
//	key any		键名
//	d time.Duration		时间
//	f func(interface)	函数,过期调用，键删除调用
func (m *Map) SetExpiredCall(key any, d time.Duration, f func(any)) {
	if !m.Has(key) {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if d == 0 {
		if t, ok := m.expired[key]; ok {
			t.Stop()
			delete(m.expired, key)
		}
		return
	}

	if t, ok := m.expired[key]; ok {
		if f != nil {
			t.fn = f
		}
		t.Reset(d)
		return
	}

	if m.expired == nil {
		m.expired = make(map[any]*timer)
	}

	t := &timer{fn: f}
	t.Timer = time.AfterFunc(d, func() { m.Del(key) })
	m.expired[key] = t
}

// ==================== 批量 & 工具 ====================

// Keys 以列表形式返回map中的所有键名
//
//	[]any	所有键名
func (m *Map) Keys() []any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ks := make([]any, len(m.keys))
	copy(ks, m.keys)
	return ks
}

// Values 以列表形式返回map中的所有值
//
//	[]any	所有值
func (m *Map) Values() []any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vs := make([]any, 0, m.length)
	for _, k := range m.keys {
		if v, ok := m.m.Load(k); ok {
			vs = append(vs, v)
		}
	}
	return vs
}

// Reset 重置归零
func (m *Map) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, t := range m.expired {
		t.Stop()
	}
	for _, k := range m.keys {
		if v, ok := m.m.Load(k); ok {
			if t, ok := m.expired[k]; ok {
				go t.exec(v)
			}
		}
		m.m.Delete(k)
	}

	m.keys = m.keys[:0]
	m.keyIndex = make(map[any]int)
	m.expired = make(map[any]*timer)
	m.length = 0
}

// Copy 从 from 复制所有并写入到 m 中
//
//	from *Map       Map对象
//	error           错误
func (T *Map) Copy(from *Map, over bool) {
	from.m.Range(func(k, v any) bool {
		if vm, ok := v.(*Map); ok {
			if over || !T.Has(k) {
				// 1，强制覆盖
				// 2，不存在
				tm := NewMap()
				tm.Copy(vm, over)
				T.Set(k, tm)
			}
		} else if over || !T.Has(k) {
			T.Set(k, v)
		}
		return true
	})
}

// ReadAll 读取所有
//
//	any   复制一份Map
func (T *Map) ReadAll() any {
	mm := make(map[any]any)
	T.m.Range(func(k, inf any) bool {
		if mv, ok := inf.(*Map); ok {
			mm[k] = mv.ReadAll()
			return true
		}
		mm[k] = inf
		return true
	})
	return mm
}

// ==================== JSON & 序列化 ====================

// MarshalJSON 转JSON
//
//	[]byte    字节格式的json
//	error     错误，格式无法压缩，导致 json.Marshal 发生错误。
func (m *Map) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.marshalJSON())
}

func arraySub(vs []any) any {
	subs := make([]any, 0)
	for _, v := range vs {
		if vt, ok := v.([]any); ok {
			subs = append(subs, arraySub(vt))
		} else if vt, ok := v.(*Map); ok {
			subs = append(subs, vt.marshalJSON())
		} else {
			subs = append(subs, v)
		}
	}
	return subs
}

func (T *Map) marshalJSON() any {
	mj := make(map[string]any)
	T.m.Range(func(key, val any) bool {
		k := fmt.Sprint(key)
		if vm, ok := val.(*Map); ok {
			mj[k] = vm.marshalJSON()
		} else if vms, ok := val.([]any); ok {
			mj[k] = arraySub(vms)
		} else {
			mj[k] = val
		}
		return true
	})
	return mj
}

// UnmarshalJSON JSON转Map，格式需要是 map[string]any
//
//	data []byte    字节格式的json
//	error          错误，格式无法解压，导致 json.Unmarshal 发生错误。
func (m *Map) UnmarshalJSON(data []byte) error {
	var mjs map[string]any
	if err := json.Unmarshal(data, &mjs); err != nil {
		return err
	}
	m.unmarshalJSON(mjs)
	return nil
}

func (m *Map) unmarshalJSON(mjs map[string]any) {
	m.Reset()
	for k, v := range mjs {
		if v == nil {
			m.Set(k, nil)
			continue
		}
		rv := inDirect(reflect.ValueOf(v))
		switch rv.Kind() {
		case reflect.Map:
			sub := NewMap()
			if vm, ok := v.(map[string]any); ok {
				sub.unmarshalJSON(vm)
			}
			m.Set(k, sub)
		case reflect.Slice, reflect.Array:
			if arr, ok := v.([]any); ok {
				m.Set(k, unarraySub(arr))
			} else {
				m.Set(k, v)
			}
		default:
			m.Set(k, v)
		}
	}
}

func unarraySub(vs []any) []any {
	subs := make([]any, 0, len(vs))
	for _, v := range vs {
		rv := inDirect(reflect.ValueOf(v))
		switch rv.Kind() {
		case reflect.Map:
			if vm, ok := v.(map[string]any); ok {
				sub := NewMap()
				sub.unmarshalJSON(vm)
				subs = append(subs, sub)
			}
		case reflect.Slice, reflect.Array:
			if arr, ok := v.([]any); ok {
				subs = append(subs, unarraySub(arr))
			}
		default:
			subs = append(subs, v)
		}
	}
	return subs
}

// WriteTo / ReadFrom 保持类似逻辑但更健壮（省略部分，逻辑已优化）
//
//	mm any     写入到mm
//	error              错误，mm类型不是map，发生错误。
func (T *Map) WriteTo(mm any) (err error) {
	rv := inDirect(reflect.ValueOf(mm))
	if rv.Kind() != reflect.Map {
		return fmt.Errorf("Map: 不支持此类型type(%v)", rv.Kind())
	}
	return T.writeTo(rv)
}

func (T *Map) writeTo(rv reflect.Value) (err error) {
	T.m.Range(func(key, val any) bool {
		if vm, ok := val.(*Map); ok {
			mm := make(map[any]any)
			vm.writeTo(reflect.ValueOf(&mm).Elem())
			rv.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(mm))
		} else if vms, ok := val.([]*Map); ok {
			rv.SetMapIndex(reflect.ValueOf(key), writeToArray(reflect.ValueOf(&vms).Elem()))
		} else {
			rv.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(val))
		}
		return true
	})
	return
}

func writeToArray(rv reflect.Value) reflect.Value {
	NilInf := reflect.TypeOf((*any)(nil)).Elem()
	subs := reflect.MakeSlice(reflect.SliceOf(NilInf), 0, 0)
	for i := 0; i < rv.Len(); i++ {
		val := rv.Index(i)
		rvi := inDirect(val)

		if vm, ok := val.Interface().(*Map); ok {
			mmType := reflect.MapOf(NilInf, NilInf)
			mm := reflect.MakeMap(mmType)
			vm.writeTo(mm)
			subs = reflect.Append(subs, mm)
		} else if rvi.Kind() == reflect.Array || rvi.Kind() == reflect.Slice {
			subs = reflect.Append(subs, writeToArray(rvi))
		} else {
			subs = reflect.Append(subs, rvi)
		}
	}
	return subs
}

// ReadFrom 从mm中读取 map
//
//	mm any      从mm中读取
//	error               错误，mm类型不是map，发生错误。
func (T *Map) ReadFrom(mm any) error {
	rv := inDirect(reflect.ValueOf(mm))
	if rv.Kind() != reflect.Map {
		return fmt.Errorf("Map: 不支持此类型type(%v)", rv.Kind())
	}
	T.readFrom(rv)
	return nil
}

func readFromArray(rv reflect.Value) []any {
	subs := make([]any, 0)
	for i := 0; i < rv.Len(); i++ {
		val := rv.Index(i)
		rvi := inDirect(val)
		if rvi.Kind() == reflect.Map {
			mm := NewMap()
			mm.readFrom(rvi)
			subs = append(subs, mm)
		} else if rvi.Kind() == reflect.Array || rvi.Kind() == reflect.Slice {
			subs = append(subs, readFromArray(rvi))
		} else {
			subs = append(subs, typeSelect(rvi))
		}
	}
	return subs
}

func (T *Map) readFrom(rv reflect.Value) error {
	for _, key := range rv.MapKeys() {
		val := rv.MapIndex(key)
		rvi := inDirect(val)
		if rvi.Kind() == reflect.Map {
			mm := NewMap()
			mm.readFrom(rvi)
			T.Set(typeSelect(key), mm)
		} else if rvi.Kind() == reflect.Array || rvi.Kind() == reflect.Slice {
			T.Set(typeSelect(key), readFromArray(rvi))
		} else {
			T.Set(typeSelect(key), typeSelect(val))
		}
	}
	return nil
}

// 遍历
//
//	f func(key, value any	遍历函数
func (m *Map) Range(f func(key, value any) bool) {
	m.m.Range(f)
}

// String 字符串
//
//	string    字符串
func (m *Map) String() string {
	if m.length == 0 {
		return "{}"
	}
	b, _ := m.MarshalJSON()
	return string(b)
}
