package vmap

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func Test_New(t *testing.T) {
	m := NewMap()
	m.New("k")
	if !m.Has("k") {
		t.Fatalf("错误，无法写入")
	}
	inf, ok := m.GetHas("k")
	if !ok {
		t.Fatalf("错误，无法读取")
	}
	if _, ok := inf.(*Map); !ok {
		t.Fatalf("错误，无法转换")
	}
}

func Test_Set(t *testing.T) {
	tests := map[string]any{
		"a": "b",
		"c": "d",
		"e": NewMap(),
		"f": struct{}{},
	}
	m := NewMap()
	for k, v := range tests {
		m.Set(k, v)
		val, ok := m.IndexHas(k)
		if val != v || !ok {
			t.Fatalf("写入的键名(%v)=值(%v)，返回的值(%v)", k, v, val)
		}
	}
}

func Test_SetExpired(t *testing.T) {
	a := NewMap()
	a.Set("k", "v")
	a.SetExpired("k", time.Second)
	time.Sleep(time.Second * 2)

	if _, ok := a.GetHas("k"); ok || a.Has("k") {
		t.Fatalf("错误，无法删除干静 2")
	}
}

func Test_SetExpiredCall(t *testing.T) {
	var err int = 1
	a := NewMap()

	a.Set("k", "v")
	a.SetExpired("k", time.Second)
	time.Sleep(time.Second * 2)

	if a.Has("k") {
		t.Fatalf("错误，过期无法删除")
	}

	a.Set("k", "v")
	a.SetExpiredCall("k", time.Second, func(inf any) {
		if v, ok := inf.(string); ok && v == "v" {
			err = 0
		}
	})
	time.Sleep(time.Second * 2)

	if err == 1 {
		t.Fatalf("错误，无法过期删除")
	}
	if _, ok := a.GetHas("k"); ok || a.Has("k") {
		t.Fatalf("错误，无法删除干静")
	}
}

func Test_Reset(t *testing.T) {
	var err int = 1
	a := NewMap()
	a.Set("k", "v")
	a.SetExpiredCall("k", time.Second, func(inf any) {
		if v, ok := inf.(string); ok && v == "v" {
			err = 0
		}
	})
	a.Reset()
	time.Sleep(time.Second)

	if err == 1 {
		t.Fatalf("错误，无法清空 1")
	}
	if a.Len() > 0 {
		t.Fatalf("错误，无法清空 2")
	}
	if len(a.keys) > 0 {
		t.Fatalf("错误，无法清空 3")
	}
}

func Test_NewMap_IndexHas(t *testing.T) {
	a := NewMap()
	a.GetNewMap("b").GetNewMap("c").Set("d", 1)

	val, ok := a.IndexHas("b", "c", "d")
	if !ok && val != 1 {
		t.Fatal("error")
	}
}

func Test_NewMap_Keys(t *testing.T) {
	m := NewMap()
	m.Set("1", "1")
	m.Set("2", Map{})
	m.Set("3", NewMap())
	ks := m.Keys()
	if !reflect.DeepEqual(ks, m.keys) {
		t.Fatal("error: 返回的 keys 不完整")
	}
}

func Test_NewMap_Values(t *testing.T) {
	avs := []any{"1", "2", "3"}
	m := NewMap()
	for _, v := range avs {
		m.Set(v, v)
	}
	vs := m.Values()
	if !reflect.DeepEqual(vs, avs) {
		t.Fatal("error: 返回的 value 不完整")
	}
	ks := m.Keys()
	if !reflect.DeepEqual(ks, avs) {
		t.Fatal("error: 返回的 keys 不完整")
	}
}

func Test_NewMap_Copy(t *testing.T) {
	a := NewMap()
	a.Set("1", "1")
	a.Set("2", Map{})
	a.Set("3", NewMap())

	b := NewMap()
	b.Set("a", a)

	m := NewMap()
	m.Copy(b, true)
	if m.String() != `{"a":{"1":"1","2":{},"3":{}}}` {
		t.Fatal("结果不相等")
	}
}

func Test_NewMap_MarshalJSONAndUnmarshalJSON_1(t *testing.T) {
	m := NewMap()

	a := m.GetNewMap("a")
	a.Set("1", "1")
	a.Set("2", Map{})
	a.Set("3", NewMap())

	b := a.GetNewMap("b")

	var c []*Map
	c = append(c, NewMap())
	b.Set("c", c)

	by, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	if string(by) != `{"a":{"1":"1","2":{},"3":{},"b":{"c":[{}]}}}` {
		t.Fatal("结果不相等")
	}

	m.Reset() // 这里重置了
	err = m.UnmarshalJSON(by)
	if err != nil {
		t.Fatal(err)
	}

	by, err = m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(by) != `{"a":{"1":"1","2":{},"3":{},"b":{"c":[{}]}}}` {
		t.Fatal("结果不相等")
	}
}

func Test_NewMap_MarshalJSONAndUnmarshalJSON_2(t *testing.T) {
	m := NewMap()
	err := m.UnmarshalJSON([]byte(`{"c":400,"a":"a","b":null,"d":[null,"d1",20,30.1]}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.String() != `{"a":"a","b":null,"c":400,"d":[null,"d1",20,30.1]}` {
		t.Fatal("结果不相等")
	}
}

func Test_NewMap_WriteToAndReadFrom(t *testing.T) {
	a := NewMap()
	a.Set("1", "1")
	a.Set("2", &Map{})
	a.Set("3", NewMap())

	b := NewMap()
	b.Set("a", a)

	m := NewMap()
	m.Set("b", b)
	m.Set("c", []*Map{b, a})

	tom := make(map[any]any)
	if err := m.WriteTo(tom); err != nil {
		t.Fatal(err)
	}

	// 重置归零
	m.Reset()

	if err := m.ReadFrom(tom); err != nil {
		t.Fatal(err)
	}

	if m.String() != `{"b":{"a":{"1":"1","2":{},"3":{}}},"c":[{"a":{"1":"1","2":{},"3":{}}},{"1":"1","2":{},"3":{}}]}` {
		t.Fatal("结果不相等")
	}
}

func TestMap_Basic(t *testing.T) {
	m := NewMap()
	m.Set("a", 1)
	m.Set("b", "hello")
	m.Set("c", NewMap())

	if m.Len() != 3 {
		t.Fatalf("Len() = %d", m.Len())
	}

	if v := m.Get("a"); v != 1 {
		t.Fatal("Get failed")
	}

	if !m.Has("b") {
		t.Fatal("Has failed")
	}
}

func TestMap_Expired(t *testing.T) {
	m := NewMap()
	called := false
	m.Set("k", "v")
	m.SetExpiredCall("k", 100*time.Millisecond, func(v any) {
		called = true
	})

	time.Sleep(300 * time.Millisecond)
	if m.Has("k") || !called {
		t.Fatal("expired failed")
	}
}

func TestMap_Nested(t *testing.T) {
	m := NewMap()
	sub := m.GetNewMaps("a", "b", "c")
	sub.Set("d", 999)

	v, ok := m.IndexHas("a", "b", "c", "d")
	if !ok || v != 999 {
		t.Fatal("nested IndexHas failed")
	}
}

func TestMap_Concurrent(t *testing.T) {
	m := NewMap()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			k := id % 20
			m.Set(k, id)
			m.Get(k)
			if id%5 == 0 {
				m.Del(k)
			}
		}(i)
	}
	wg.Wait()
}

func TestMap_JSON(t *testing.T) {
	m := NewMap()
	m.GetNewMap("nested").Set("x", 42)
	m.Set("arr", []any{1, 2, map[string]any{"a": 3}})

	data, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	m2 := NewMap()
	if err := m2.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}

	if m2.String() != m.String() {
		t.Fatal("JSON roundtrip failed")
	}
}

func TestMap_Copy(t *testing.T) {
	src := NewMap()
	src.GetNewMaps("level1", "level2").Set("deep", "value")
	src.Set("normal", 42)

	dst := NewMap()
	dst.Copy(src, true)

	if dst.String() != src.String() {
		t.Fatalf("Copy not equal:\ngot: %s\nwant:%s", dst.String(), src.String())
	}
}

func TestMap_Marshal_Unmarshal(t *testing.T) {
	m := NewMap()
	m.GetNewMaps("a", "b").Set("c", []any{1, 2, NewMap()})

	data, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	m2 := NewMap()
	if err := m2.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}

	if m2.String() != m.String() {
		t.Fatal("Marshal/Unmarshal roundtrip failed")
	}
}

func TestMap_WriteTo_ReadFrom(t *testing.T) {
	m := NewMap()
	m.GetNewMap("nested").Set("x", 100)
	m.Set("slice", []any{NewMap(), 42})

	target := make(map[string]any)
	if err := m.WriteTo(target); err != nil {
		t.Fatal(err)
	}

	m2 := NewMap()
	if err := m2.ReadFrom(target); err != nil {
		t.Fatal(err)
	}

	if m2.String() != m.String() {
		t.Fatal("WriteTo/ReadFrom failed")
	}
}

func TestMap_Reset(t *testing.T) {
	m := NewMap()
	e := false
	m.Set("k", "v")
	m.SetExpiredCall("k", 100*time.Millisecond, func(any) { e = true })
	m.Reset()

	time.Sleep(300 * time.Millisecond)
	if !e {
		t.Fatal("Reset failed not call expired function")
	}
	if m.Len() != 0 || len(m.Keys()) != 0 {
		t.Fatal("Reset failed")
	}
}

func TestMap_Keys_Values(t *testing.T) {
	m := NewMap()
	m.Set(1, "one")
	m.Set("two", 2)
	m.Set(true, false)

	keys := m.Keys()
	values := m.Values()

	if len(keys) != 3 || len(values) != 3 {
		t.Fatal("Keys/Values length wrong")
	}
}
