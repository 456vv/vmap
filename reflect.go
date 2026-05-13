package vmap

import (
	"reflect"
)

// TypeSelect 类型选择 - 安全读取 reflect.Value
//
//	参：
//	  v reflect.Value        映射一种未知类型的变量
//	返：
//	  any            读出v的值，如果v是未知类型，则返回nil
func typeSelect(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint()
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
		if v.CanInterface() {
			return v.Interface()
		}
		// 统一转为 []any
		l := v.Len()
		result := make([]any, l)
		for i := 0; i < l; i++ {
			result[i] = typeSelect(v.Index(i))
		}
		return result
	case reflect.Map:
		fallthrough
	case reflect.Struct:
		fallthrough
	default:
		if v.CanInterface() {
			return v.Interface()
		}
		return v.String()
	}
}

// InDirect 安全解引用指针/接口
//
//	参：
//	  v reflect.Value        映射引用为真实内存地址
//	返：
//	  reflect.Value          真实内存地址
func inDirect(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return v // 返回 nil 指针
		}
		v = v.Elem()
	}
	return v
}
