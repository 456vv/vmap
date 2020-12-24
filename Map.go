package vmap

import (
    "sync"
    "encoding/json"
    "reflect"
    "fmt"
    "time"
)

type timer struct{
	t	*time.Timer
	f	func(interface{})
}
func (t *timer) Reset(d time.Duration) bool{
	return t.t.Reset(d)
}
func (t *timer) Stop() bool {
	return t.t.Stop()
}

// Map 推荐使用NewMap来规范的创建，这样可避免不必要的错误。
type Map struct {
	m			sync.Map				 	// Map
    expired		map[interface{}]*timer		// 有效期
    mu			sync.Mutex					// 锁
    keys		[]interface{}				// 存放键名
    length		int							// 长度
}

//NewMap 对象
//	*Map      Map对象
func NewMap() *Map {
    return &Map{}
}

//New 增加一个key对应的Map对象，并返回该对象。
//.New 不管是否存在该 key 键值，都会写入一个新的Map覆盖 key 键值。
//要是你需要一个这样的功能，存在key键值，返回该key键值。如果不存在该key键值，返回一个新的key对应的Map。请使用 .GetNewMap 方法
//	key interface{}     键名
//	*Map                Map对象
func (T *Map) New(key interface{}) *Map {
    t := NewMap()
    T.Set(key, t)
    return t
}

//GetNewMap 如果不存在增加一个key对应的Map对象，否则读取并返回该对象。
//	key interface{}     键名
//	*Map                Map对象
func (T *Map) GetNewMap(key interface{}) *Map {
    actual, ok := T.GetHas(key)
	if ok {
		if mm, ok := actual.(*Map); ok {
			return mm
		}
	}
	return T.New(key)
}

//GetNewMaps 如果不存在增加一个key对应的Map对象，否则读取并返回该对象。
//他支持链式读取或创建，如果你想相独读取，可以使用 .Index 方法
//	key ...interface{}     键名
//	*Map                Map对象
func (T *Map) GetNewMaps(keys ...interface{}) *Map {
	tm := T
	for _, key := range keys {
		tm = tm.GetNewMap(key)
	}
	return tm
}

//Len 长度
//	int   长度
func (T *Map) Len() int {
    return T.length
}

//Set 设置，如果你设置的值是Map，将会被强制初始化该值。这样避免读取并调用时候出错。
//	key interface{}   键名
//	val interface{}   值
func (T *Map) Set(key, val interface{}) {
	T.mu.Lock()
	defer T.mu.Unlock()

	if !T.Has(key) {
		T.keys = append(T.keys, key)
		T.length++
	}
	T.m.Store(key, val)
}


// SetExpired 单个键值的有效期
//	key interface{}		键名
//	d time.Duration		时间
func (T *Map) SetExpired(key interface{}, d time.Duration){
	T.SetExpiredCall(key, d, nil)
}

// SetExpiredCall 单个键值的有效期，过期后并调用函数
//	key interface{}		键名
//	d time.Duration		时间
//	f func(interface)	函数,过期调用，键删除调用
func (T *Map) SetExpiredCall(key interface{}, d time.Duration, f func(interface{})){
	T.mu.Lock()
	defer T.mu.Unlock()

	//如果该Key不存在，则退出
	if !T.Has(key) {
		return
	}
	
	giveup := d == 0
	//存在定时，使用定时。如果过期，则创建新的定时
	if timer, ok := T.expired[key]; ok {
		if giveup {
			timer.Stop()
			delete(T.expired, key)
			return
		}
		if f != nil {
			timer.f = f
		}
		if timer.Reset(d) {
			return
		}
		timer.Stop()
	}
	if !giveup {
		if T.expired == nil {
			T.expired = make(map[interface{}]*timer)
		}
		T.expired[key]= T.afterFunc(key, d, f)
	}
}
func (T *Map) afterFunc(key interface{}, d time.Duration, f func(interface{})) *timer{
	k := key
	t := &timer{}
	t.f = f
	t.t = time.AfterFunc(d, func(){
		T.Del(k)
	})
	return t
}


//Get 读取
//	key interface{}   读取值的键名
//	interface{}       读取值
func (T *Map) Get(key interface{}) interface{} {
    val, _ := T.m.Load(key)
    return val
}

//Has 判断
//	key interface{}   键名
//	bool              判断，如果为true,判断存在。否则为flase
func (T *Map) Has(key interface{}) bool {
	for _, k := range T.keys {
		if reflect.DeepEqual(k, key) {
			return true
		}
	}
	return false
}

//GetHas 读取并判断
//	key interface{}   键名
//	val interface{}   读取值
//	ok bool           判断，如果为true,判断存在。否则为flase
func (T *Map) GetHas(key interface{}) (val interface{}, ok bool) {
    return T.m.Load(key)
}

//GetOrDefault 读取，如果不存，返回默认值
//	key interface{}   	键名
//	def interface{}		默认值
//	val interface{}   	读取值
func (T *Map) GetOrDefault(key interface{}, def interface{}) interface{} {
	val, ok := T.m.Load(key)
	if !ok || reflect.ValueOf(&val).Elem().Kind() == reflect.Invalid {
		return def
	}
    return val
}

//Index 指定读取，这个仅能使用于 *Map 中有子 *Map。功能用于快速索引定位。
//    参：
//      key ...interface{}        快速指定父子关系中的值，如 .Index("A", "B", "C")
//    返：
//      interface{}               读取值
func (T *Map) Index(key ...interface{}) interface{} {
    mv, _ := T.IndexHas(key...)
    return mv
}

//IndexHas 指定读取和判断，这个只有使用于 *Map 中有子 *Map。功能用于快速索引定位。
//    key ...interface{}        快速指定父子关系中的值，如 .Index("A", "B", "C")
//    interface{}               读取值
//    bool                      判断，如果为true，表示存在。否则为flase
//    例：
//      m1 := birdswo.NewMap()
//      m2 := birdswo.NewMap()
//      m2.Set("b", "value")
//      m1.Set("a", m2)
//      v, ok := m1.IndexHas("a", "b")
//      fmt.Println(v, ok)
//      //value true
func (T *Map) IndexHas(key ...interface{}) (interface{}, bool) {
    switch len(key){
    case 0:return nil, false
    case 1:
    return T.m.Load(key[0])
    default:
    	mst, ok := T.m.Load(key[0])
    	if ok {
	    	if mt, ok := mst.(*Map); ok {
	    		return mt.IndexHas(key[1:]...)
	    	}
    	}
    	return nil, false
    	
    }
}


//Del 删除
//	key interface{}   键名
func (T *Map) Del(key interface{}) {
	T.mu.Lock()
	defer T.mu.Unlock()
	
	//删除键值
	var val interface{}
    for j := len(T.keys); j>0; j-- {
    	i := j-1
        if reflect.DeepEqual(T.keys[i], key) {
            T.keys = append(T.keys[:i], T.keys[i+1:]...)
            T.length--
            val = T.Get(key)
			T.m.Delete(key)
        }
    }
    
	//停止定时并删除
	if timer, ok := T.expired[key]; ok {
		delete(T.expired, key)
		timer.Stop()
		if timer.f != nil {
			go timer.f(val)
		}
	}
}

//Dels 删除
//	keys []interface{}   键名
func (T *Map) Dels(keys []interface{}) {
	for _, key := range keys {
		T.Del(key)
    }
}


//ReadAll 读取所有
//	interface{}   复制一份Map
func (T *Map) ReadAll() interface{} {
    mm := make(map[interface{}]interface{})
	T.m.Range(func(k, v interface{}) bool{
        mm[k] = v
        return true
	})
    return mm
}

//Reset 重置归零
func (T *Map) Reset() {
	T.mu.Lock()
	defer T.mu.Unlock()
	
    //停止所有定时
    for key, timer := range T.expired {
    	timer.Stop()
		if timer.f != nil {
			go timer.f(T.Get(key))
		}
    }
    
    //删除存存储
    for _, key := range T.keys {
    	T.m.Delete(key)
    }
    
    T.expired	= make(map[interface{}]*timer)
    T.keys		= T.keys[:0:0]
    T.length	= 0
    //T.m			= sync.Map{}
}

//Copy 从 from 复制所有并写入到 m 中
//	from *Map       Map对象
//	error           错误
func (T *Map) Copy(from *Map, over bool) {
    from.m.Range(func(k, v interface{})bool{
        if vm, ok := v.(*Map); ok {
            if over || !T.Has(k) {
        		//1，强制覆盖
        		//2，不存在
        		tm := NewMap()
            	tm.Copy(vm, over)
    			T.Set(k, tm)
            }
        }else if over || !T.Has(k) {
            T.Set(k, v)
        }
        return true
    })
}

//MarshalJSON 转JSON
//	[]byte    字节格式的json
//	error     错误，格式无法压缩，导致 json.Marshal 发生错误。
func (T *Map) MarshalJSON() ([]byte, error) {
    var mj = T.marshalJSON()
    return json.Marshal(mj)
}
func arraySub(vs []interface{}) interface{}{
	subs := make([]interface{}, 0)
	for _, v := range vs {
		if  vt, ok := v.([]interface{}); ok {
			subs = append(subs, arraySub(vt))
		}else if vt, ok := v.(*Map); ok {
			subs = append(subs, vt.marshalJSON())
		}else{
			subs = append(subs, v)
		}
	}
	return subs
}
func (T *Map) marshalJSON() interface{} {
    var mj = make(map[string]interface{})
    T.m.Range(func(key, val interface{})bool{
    	k := fmt.Sprintf("%v", key)
        if vm, ok := val.(*Map); ok {
            mj[k] = vm.marshalJSON()
        }else if vms, ok := val.([]interface{}); ok {
            mj[k] = arraySub(vms)
        }else{
            mj[k] = val
        }
        return true
    })
    return mj
}

//String 字符串
//	string    字符串
func (T *Map) String() string {
    if T.length == 0 {
        return "{}"
    }
    jsonStr, err := T.MarshalJSON()
    if err != nil {
        return "{}"
    }
     return string(jsonStr)
}

//UnmarshalJSON JSON转Map，格式需要是 map[string]interface{}
//	data []byte    字节格式的json
//	error          错误，格式无法解压，导致 json.Unmarshal 发生错误。
func (T *Map) UnmarshalJSON(data []byte) error {
    var mjs  = make(map[string]interface{})
    err := json.Unmarshal(data, &mjs)
    if err == nil {
    	T.unmarshalJSON(mjs)
    }
    return err
}

func unarraySub(vs []interface{})interface{}{
	subs := make([]interface{}, 0)
	for _, v := range vs {
   		vtype := reflect.ValueOf(v)
   		vtype = inDirect(vtype)
   		if vtype.Kind() == reflect.Array || vtype.Kind() == reflect.Slice {
   			if vt, ok := v.([]interface{}); ok {
				subs = append(subs, unarraySub(vt))
   			}
   		}else if vtype.Kind() == reflect.Map {
   			if vt, ok := v.(map[string]interface{}); ok {
   				sub := NewMap()
   				sub.unmarshalJSON(vt)
				subs = append(subs, sub)
			}
   		}else{
			subs = append(subs, v)
   		}
	}
	return subs
}

func (T *Map) unmarshalJSON(mjvs map[string]interface{}) {
	for k, mjv := range mjvs {
		mjvtype := reflect.TypeOf(mjv)
		if mjvtype.Kind() == reflect.Map {
   			if vt, ok := mjv.(map[string]interface{}); ok {
   				sub := NewMap()
   				sub.unmarshalJSON(vt)
				T.Set(k, sub)
			}
		}else if mjvtype.Kind() == reflect.Array || mjvtype.Kind() == reflect.Slice {
   			if vt, ok := mjv.([]interface{}); ok {
				T.Set(k, unarraySub(vt))
   			}
		}else{
			T.Set(k, mjv)
		}
	}
}


//WriteTo 写入到 mm
//	mm interface{}     写入到mm
//	error              错误，mm类型不是map，发生错误。
func (T *Map) WriteTo(mm interface{}) (err error) {
    rv := inDirect( reflect.ValueOf(&mm) )
    if rv.Kind() != reflect.Map {
        return fmt.Errorf("Map: 不支持此类型type(%v)", rv.Kind())
    }
    return T.writeTo(rv)
}
func (T *Map) writeTo(rv reflect.Value) (err error) {
    T.m.Range(func(key, val interface{}) bool{
        if vm, ok := val.(*Map); ok {
            mm := make(map[interface{}]interface{})
            vm.writeTo(reflect.ValueOf(mm))
            rv.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(mm))
        }else if _, ok := val.([]*Map); ok {
            rv.SetMapIndex(reflect.ValueOf(key), writeToArray(reflect.ValueOf(val)))
        }else{
            rv.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(val))
        }
        return true
    })
    return
}

func writeToArray(rv reflect.Value) reflect.Value {
    NilInf := reflect.TypeOf((*interface{})(nil)).Elem()
	subs := reflect.MakeSlice(reflect.SliceOf(NilInf), 0, 0)
	for i:= 0;i<rv.Len();i++{
		val := rv.Index(i)
        rvi := inDirect(val)

        if vm, ok := val.Interface().(*Map); ok {
            mmType := reflect.MapOf(NilInf , NilInf)
            mm := reflect.MakeMap(mmType)
            vm.writeTo(mm)
			subs = reflect.Append(subs, mm)
        }else if rvi.Kind() == reflect.Array || rvi.Kind() == reflect.Slice {
			subs = reflect.Append(subs, writeToArray(rvi))
        }else{
			subs = reflect.Append(subs, rvi)
        }
	}
	return subs
}

//ReadFrom 从mm中读取 map
//	mm interface{}      从mm中读取
//	error               错误，mm类型不是map，发生错误。
func (T *Map) ReadFrom(mm interface{}) error {
    rv := inDirect( reflect.ValueOf(&mm) )
    if rv.Kind() != reflect.Map {
        return fmt.Errorf("Map: 不支持此类型type(%v)", rv.Kind())
    }
    T.readFrom(rv)
    return nil
}
func readFromArray(rv reflect.Value) []interface{}{
	subs := make([]interface{}, 0)
	for i:= 0;i<rv.Len();i++{
		val := rv.Index(i)
        rvi := inDirect(val)
		if rvi.Kind()  == reflect.Map {
			mm :=NewMap()
			mm.readFrom(rvi)
			subs = append(subs, mm)
        }else if rvi.Kind() == reflect.Array || rvi.Kind() == reflect.Slice {
			subs = append(subs, readFromArray(rvi))
        }else{
			subs = append(subs, typeSelect(rvi))
        }
	}
	return subs
}

func (T *Map) readFrom(rv reflect.Value) error {
    vs := rv.MapKeys()
    for _, key := range vs {
        val := rv.MapIndex(key)
        rvi := inDirect(val)
        if rvi.Kind() == reflect.Map {
            mm    := NewMap()
            mm.readFrom(rvi)
            T.Set(typeSelect(key), mm)
        }else if rvi.Kind() == reflect.Array || rvi.Kind() == reflect.Slice {
       	 	T.Set(typeSelect(key), readFromArray(rvi))
        }else{
        	T.Set(typeSelect(key), typeSelect(val))
        }
    }
    return nil
}

//遍历
//	f func(key, value interface{}	遍历函数
func (T *Map) Range(f func(key, value interface{}) bool){
	T.m.Range(f)
}
