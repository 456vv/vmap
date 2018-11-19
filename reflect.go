package vmap
import (
    "reflect"
)

//TypeSelect 类型选择
//    参：
//      v reflect.Value        映射一种未知类型的变量
//    返：
//      interface{}            读出v的值
//func TypeSelect(v reflect.Value) interface{} {
//    return typeSelect(v)
//}
func typeSelect(v reflect.Value) interface{} {

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
	    case reflect.Invalid:
            return nil
        case reflect.Slice, reflect.Array:
            if v.CanInterface() {
                return v.Interface()
            }
            var t []interface{}
            for i:=0; i<v.Len(); i++ {
                t = append(t, typeSelect(v.Index(i)))
            }
            return t
        default:
            if v.CanInterface() {
                return v.Interface()
            }
            return v.String()
    }
}

//InDirect 指针到内存
//    参：
//      v reflect.Value        映射引用为真实内存地址
//    返：
//      reflect.Value          真实内存地址
//func InDirect(v reflect.Value) reflect.Value {
//    return inDirect(v)
//}
func inDirect(v reflect.Value) reflect.Value {
	for ; v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface; v = v.Elem() {
    }
    return v
}

