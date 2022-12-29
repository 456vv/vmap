package vmap

import (
	"fmt"
	"reflect"
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

func Test_NewMap_ReadAll(t *testing.T) {
	a := NewMap()
	b := a.GetNewMap("b")
	b.Set("1", "1")
	b.Set("2", Map{})
	b.Set("3", NewMap())

	m := NewMap()
	m.Set("a", a)
	all := m.ReadAll()
	if s := fmt.Sprint(all); s != "map[a:map[b:map[1:1 2:{{{0 0} {<nil>} map[] 0} map[] {0 0} [] 0} 3:map[]]]]" {
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
