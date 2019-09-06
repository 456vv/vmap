package vmap

import (
	"testing"
	"time"
)



func Test_New(t *testing.T){
	m := NewMap()
	m.Set("1","1")
	m.New("1")
	if !m.Has("1") {
		t.Fatalf("错误，无法写入")
	}
	inf, ok := m.GetHas("1")
	if !ok {
		t.Fatalf("错误，无法读取")
	}
	if _, ok := inf.(*Map); !ok {
		t.Fatalf("错误，无法转换")
	}
}
func Test_NewMap(t *testing.T){
	type a struct{}
	tests := [][]interface{}{
		{"a", "b", true},
		{"c", "d", true},
		{"e", NewMap(), true},
		{"f", a{}, true},
	}
	m := NewMap()
	for _, v := range tests {
		m.Set(v[0], v[1])
		val, ok := m.IndexHas(v[0])
		if v[1] != val || v[2] != ok {
			t.Fatalf("写入的键名(%v)=值(%v)，判断(%v)。返回的值(%v)，判断(%v)", v[0], v[1], v[2], val, ok)
		}
	}
}


func Test_SetExpired(t *testing.T){
	a := NewMap()
	a.Set("1", "1")
	a.SetExpired("1", time.Second)
	time.Sleep(time.Second*2)
	if a.Has("1") {
		t.Fatalf("错误，无法删除干静 1")
	}
	if _, ok := a.Map.Load("1"); ok {
		t.Fatalf("错误，无法删除干静 2")
	}
	
}

func Test_SetExpiredCall(t *testing.T){
	var err int = 1
	a := NewMap()
	a.Set("1", "1")
	a.SetExpiredCall("1", time.Second, func (inf interface{}){
		if v, ok := inf.(string); ok && v == "1" {
			err = 0
		}
	})
	time.Sleep(time.Second*2)
	if err == 1 {
		t.Fatalf("错误，无法过期删除")
	}
	if a.Has("1") {
		t.Fatalf("错误，无法删除干静 1")
	}
	if _, ok := a.Map.Load("1"); ok {
		t.Fatalf("错误，无法删除干静 2")
	}
	
}
func Test_Reset(t *testing.T){
	var err int = 1
	a := NewMap()
	a.Set("1", "1")
	a.SetExpiredCall("1", time.Second, func (inf interface{}){
		if v, ok := inf.(string); ok && v == "1" {
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

func Test_NewMap_IndexHas(t *testing.T){
	a1 := NewMap()
    	a2 := NewMap()
    	a2.Set("a3", "A3")
    	a2.Set("a3-1", Map{})
    	a2.Set("a3-2", NewMap())
	a1.Set("a2", a2)
	a1.Set("a3", 123)

	m := NewMap()
	m.Set("a1", a1)
	val, ok := m.IndexHas("a1", "a2", "a3")
	t.Log(val, ok)
	//A3 true

	val, ok = m.IndexHas("a1", "a3", "a3")
	t.Log(val, ok)
	//<nil> false

	val, ok = m.IndexHas("a1", "a2", "a3-1")
	t.Log(val, ok)
	//{<nil> 0} true

	val, ok = m.IndexHas("a1", "a2", "a3-2")
	t.Log(val, ok)
	//map[] true

	val.(*Map).Set("a4", "a4")

	val, ok = m.IndexHas("a1", "a2", "a3-2", "a4")
	t.Log(val, ok)
	//a4 true

	val, ok = m.IndexHas("a1", "a4", "a3-2", "a5")
	t.Log(val, ok)
	//<nil> false
}


func Test_NewMap_ReadAll(t *testing.T){
	a1 := NewMap()
	a2 := NewMap()
	a2.Set("a3", "A3")
	a2.Set("a3-1", Map{})
	a2.Set("a3-2", NewMap())
	a1.Set("a2", a2)

	m := NewMap()
	m.Set("a1", a1)
	var rall = m.ReadAll()
	t.Log(rall)
	//map[a1:map[a2:map[a3:A3 a3-1:{<nil> 0} a3-2:map[]]]]
}

func Test_NewMap_Copy(t *testing.T){
	a1 := NewMap()
	a2 := NewMap()
	a2.Set("a3", "A3")
	a2.Set("a3-1", Map{})
	a2.Set("a3-2", NewMap())
	a1.Set("a2", a2)

	m := NewMap()
	m.Copy(a1, true)

	t.Log(m)
	//map[a2:map[a3:A3 a3-1:{<nil> 0} a3-2:map[]]]
}


func Test_NewMap_MarshalJSONAndUnmarshalJSON(t *testing.T){
	a1 := NewMap()
	a2 := NewMap()
	a2.Set("a3", "A3")
	a2.Set("a3-1", Map{})
	a2.Set("a3-2", NewMap())
	a1.Set("a2", a2)
	
	a4 := make([]*Map, 0)
	a4 = append(a4, NewMap())
	a1.Set("a4", a4)

	m := NewMap()
	m.Set("a1", a1)
	b, err := m.MarshalJSON()
	t.Log(err)
	//<nil>
	if err == nil {
		t.Log(string(b))
		//{"a1":{"a2":{"a3":"A3","a3-1":{},"a3-2":{}}}}

		//这里重置了
		m.Reset()
		err = m.UnmarshalJSON(b)
		if err != nil {
			t.Fatal(err)
		}
		b, err = m.MarshalJSON()
		t.Log(err)
		//<nil>
		if err == nil {
			t.Log(string(b))
			//{"a1":{"a2":{"a3":"A3","a3-1":{},"a3-2":{}}}}
		}
	}
}


func Test_NewMap_WriteToAndReadFrom(t *testing.T){
	a2 := NewMap()
	a2.Set("a3", "A3")
	a2.Set("a3-1", Map{})
	a2.Set("a3-2", NewMap())
	
	tom := make(map[interface{}]interface{})
	m := NewMap()
	
	a1 := NewMap()
	a1.Set("a2", a2)
	m.Set("a1", a1)
	
	a3 := []*Map{a1, a2}
	m.Set("a3", a3)
	err := m.WriteTo(tom)
	if err != nil{
		t.Fatal(err)
	}
	t.Log(tom)
	//map[a1:map[a2:map[a3-2:map[] a3:A3 a3-1:{<nil> 0}]]]

	//重置归零
	m.Reset()
	
	err = m.ReadFrom(tom)
	if err != nil{
		t.Fatal(err)
	}
	t.Log(m)

}












































