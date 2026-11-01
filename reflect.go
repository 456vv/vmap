package vmap

import "reflect"

type indirectVisit struct {
	typ reflect.Type
	ptr uintptr
}

// unwrapIndirect 停在非指针/非接口、nil、或 *Map。
func unwrapIndirect(v reflect.Value) (reflect.Value, bool) {
	if stopIndirect(v) || (v.Kind() == reflect.Pointer && v.Type() == mapPtrType) {
		return v, true
	}
	return v.Elem(), false
}

// inDirect 解包指针/接口。快路径 0~2 层不分配，慢路径启用环检测。
func inDirect(v reflect.Value) reflect.Value {
	for i := 0; i < 2; i++ {
		nv, done := unwrapIndirect(v)
		if done {
			return nv
		}
		v = nv
	}
	// 环检测同时记录类型和地址，避免不同类型指针地址相同导致误判。
	// seen 只在当前调用内使用，不产生跨 goroutine 共享。
	seen := make(map[indirectVisit]struct{}, 4)
	for n := 0; n < maxNestDepth; n++ {
		nv, done := unwrapIndirect(v)
		if done {
			return nv
		}
		// 环检测必须对着解包前的指针，不能对着 Elem。
		if v.Kind() == reflect.Pointer {
			key := indirectVisit{typ: v.Type(), ptr: v.Pointer()}
			if _, ok := seen[key]; ok {
				return reflect.Value{}
			}
			seen[key] = struct{}{}
		}
		v = nv
	}
	return reflect.Value{}
}

func stopIndirect(v reflect.Value) bool {
	return !v.IsValid() || (v.Kind() != reflect.Pointer && v.Kind() != reflect.Interface) || v.IsNil()
}

// typeSelect 读取 reflect.Value。优先 CanInterface，保留具体类型。
func typeSelect(v reflect.Value) any {
	return typeSelectDepth(v, 0)
}

func typeSelectDepth(v reflect.Value, depth int) any {
	if !v.IsValid() || depth > maxNestDepth {
		return nil
	}
	v = inDirect(v)
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.Interface, reflect.Slice:
		if v.IsNil() {
			if v.CanInterface() {
				return v.Interface()
			}
			return nil
		}
	}
	if v.CanInterface() {
		return v.Interface()
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Uintptr:
		return uintptr(v.Uint())
	case reflect.Float32, reflect.Float64:
		return v.Float()
	case reflect.Bool:
		return v.Bool()
	case reflect.Complex64, reflect.Complex128:
		return v.Complex()
	case reflect.String:
		return v.String()
	case reflect.UnsafePointer:
		return v.Pointer()
	case reflect.Slice, reflect.Array:
		n := v.Len()
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = typeSelectDepth(v.Index(i), depth+1)
		}
		return out
	case reflect.Map:
		out := make(map[any]any, v.Len())
		for _, k := range v.MapKeys() {
			kk := typeSelectDepth(k, depth+1)
			// 取不出的键会退化成 nil，多个不同键落到 out[nil] 上互相覆盖。
			// 只有原键本身是 nil 时才保留 nil 键。
			if kk == nil && !isNilKey(k) {
				continue
			}
			if !validKey(kk) {
				continue
			}
			out[kk] = typeSelectDepth(v.MapIndex(k), depth+1)
		}
		return out
	default:
		return nil
	}
}

// isNilKey 判断 map 键自身是否为 nil。可作 map 键且可为 nil 的只有这几种。
func isNilKey(k reflect.Value) bool {
	switch k.Kind() {
	case reflect.Chan, reflect.Pointer, reflect.Interface, reflect.UnsafePointer:
		return k.IsNil()
	}
	return false
}
