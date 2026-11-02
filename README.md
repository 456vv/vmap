# vmap

[![Go](https://img.shields.io/badge/Go-1.18%2B-00ADD8)](https://go.dev)

`vmap` 是一个并发安全、支持 TTL 的 `any -> any` 映射容器，专为「配置树 / 会话表 / JSON 友好的动态结构」设计。

相比标准库 `sync.Map`，补齐了它缺少的东西：**嵌套子表、按 key 的过期回调、深拷贝、JSON 编组、与原生 map 的双向转换**，同时保证所有回调都在锁外异步执行。

## 特性

- **并发安全**：内部 `sync.RWMutex`，所有导出方法可并发调用（`*Map` 指针语义，禁止按值复制）
- **TTL**：登记时可指定过期时间，支持到期/删除/关闭三种回调
- **嵌套**：值为 `*Map` 即可多层组合，提供 `GetNewMaps` / `IndexHas` 链式访问
- **深拷贝**：`Clone` / `Copy` 复制嵌套子表、切片、结构体，并正确处理环引用
- **JSON 编组**：实现 `json.Marshaler` / `json.Unmarshaler`，键名冲突自动消歧
- **互操作**：`ReadAll` / `WriteTo` / `ReadFrom` 在 `*Map` 与任意 `map` 之间转换，带溢出检查
- **健壮性**：深度上限、环引用检测、panic 回调观测、索引自愈

## 导入

```go
import "github.com/456vv/vmap/v2" // package vmap
```

## 快速开始

```go
m := vmap.NewMap()
m.Set("name", "gopher")
m.Set("age", 3)

fmt.Println(m.Get("name"))       // gopher
v, ok := m.GetHas("age")         // 3, true
fmt.Println(m.Len())             // 2

m.Del("age")
fmt.Println(m.Has("age"))        // false

// 不存在时返回默认值；键存在且值为 nil 时仍返回 nil
m.GetOrDefault("missing", "dft") // dft
```

## API 概览

### 构造与生命周期

| 方法 | 说明 |
| --- | --- |
| `NewMap()` | 创建空表 |
| `NewMapSize(n int)` | 预分配约 `n` 个键 |
| `Reset()` | 静默清空（不触发回调），保留小容量，可继续使用 |
| `ExpireAll()` | 清空：停止所有定时器、释放底层结构、异步触发已登记回调；表仍可继续使用 |

### 读写

| 方法 | 说明 |
| --- | --- |
| `Set(key, val any)` | 写入；键不可比较或含 NaN 时 panic |
| `Get(key) any` / `GetHas(key) (any, bool)` | 读取；后者可区分「不存在」与「值为 nil」 |
| `Has(key) bool` | 键是否存在 |
| `GetOrDefault(key, def any) any` | 键不存在时返回 `def` |
| `LoadOrStore(key, val any) (any, bool)` | 存在则返回已有值，否则写入 |
| `Del(key)` / `Dels(keys []any)` | 删除，触发已登记的过期回调 |
| `LoadAndDelete(key) (any, bool)` | 原子取出并删除 |
| `Len() int` | 元素个数（以 `data` 为准） |

### 嵌套与链式访问

```go
user := m.GetNewMap("user")      // 不存在则创建
user.Set("id", 1)

cfg := m.GetNewMaps("a", "b", "c") // 逐层读取或创建；空参数返回接收者本身
cfg.Set("deep", true)

v, ok := m.IndexHas("a", "b", "c", "deep") // true, true
```

| 方法 | 说明 |
| --- | --- |
| `New(key) *Map` | 删掉旧值后写入一个新的空子表 |
| `GetNewMap(key) *Map` | 读取子表，不存在或类型不符则创建 |
| `GetNewMaps(keys ...any) *Map` | 链式版本 |
| `Index(keys ...any) any` / `IndexHas(...)` | 逐层读取，中间节点必须是非 nil `*Map` |

### TTL

```go
m.SetExpired("token", 10*time.Minute)

// 带回调：到期、到期前被 Del、ExpireAll 时都会调用 f
m.SetExpiredCall("session", 30*time.Second, func(v any) {
    log.Printf("session expired: %v", v)
})

d, ok := m.TTL("session") // 剩余时间，可能为负
```

- `d > 0` 设置有效期；`d == 0` 取消过期且不调用回调；`d < 0` 立即过期并调用回调
- 回调 `f == nil` 时保留旧回调
- 回调统一在**释放锁之后**的独立 goroutine 中执行，且 panic 会被捕获转交 `SetPanicHandler`
- 键不存在时 `SetExpired*` 直接忽略

### 遍历与快照

```go
m.Range(func(k, v any) bool {
    fmt.Println(k, v)
    return true // 返回 false 提前终止
})

keys := m.Keys()     // 快照，不会随原表变化
values := m.Values() // 与 Keys 同序
```

`Range` 在读锁内取快照后释放锁再回调，因此回调中可以安全地写同一个表；`Keys` / `Values` 直接返回快照。

### 复制与克隆

```go
dup := m.Clone()               // 深拷贝，nil 接收者返回 nil

dst := vmap.NewMap()
err := dst.Copy(src, false)    // over=false 时不覆盖已存在的键
```

- 嵌套 `*Map`、`[]any`、`[]*Map`、`map[string]any`、`map[any]any`、结构体字段都会被深拷贝
- 环引用通过 `repl` 映射复用已创建的副本，不会无限递归
- TTL 一律复制；已过期但定时器尚未触发的键会立即过期，而不是退化成永久键
- 克隆出的子表在提交后统一激活定时器，避免与用户锁形成 ABBA 死锁

### JSON

```go
b, _ := json.Marshal(m)        // 嵌套 *Map 展开为 JSON 对象

var m2 vmap.Map
err := json.Unmarshal(b, &m2)  // 要求 JSON 对象；null 会清空接收者

fmt.Println(m.String())        // 等价于 MarshalJSON 的文本
```

键名规则：`string` / 数值 / `bool` 直接字符串化；类型冲突时追加后缀，且后缀本身也会避让：

```json
{"1": "string-key", "1#2(int)": "int-key"}
```

### 与原生 map 互操作

```go
var raw map[string]any
err := m.WriteTo(&raw)         // *Map -> map，nil map 会自动 make

m2 := vmap.NewMap()
err = m2.ReadFrom(raw)         // map -> *Map，合并而非清空

all := m.ReadAll()             // 递归展开为 map[any]any
```

转换过程带类型与范围检查：整数溢出、负数转无符号、非有限浮点、含小数转整型、切片长度与数组不匹配等都会返回错误而不是静默截断。

### 可观测性

```go
vmap.SetPanicHandler(func(r any) {
    log.Printf("vmap recovered panic: %v", r)
})
```

过期回调、快照、JSON 编组等路径中的用户代码 panic 都会被 recover 并转发到这里，避免打垮进程；传 `nil` 恢复静默。

## 语义与边界

| 场景 | 行为 |
| --- | --- |
| 键为切片 / map / 函数 / 含 NaN | `Set` 系列 panic；`Get` 系列视为不存在 |
| 存入 `*Map` | 保留引用 |
| 存入按值 `Map` | 自动转为独立的 `*Map` 副本（含 TTL 快照） |
| 存入原生 `map` / slice / struct | 原样保存，`Clone` 时才深拷贝 |
| `Del` 一个不存在的键 | 不触发回调 |
| `ExpireAll` 之后 | 表被清空，读取返回零值；仍可继续写入 |
| `Keys` 顺序 | 插入序经交换删除扰动，不是严格有序 |
| 嵌套 / 编组 / 解析深度 | 上限 **1024** 层，超限返回错误或占位串 |
| 复制 `Map` 值 | **禁止**，会复制锁并与源共享底层桶 |
| 回调执行时机 | 锁外异步，不保证顺序 |

## 并发模型与设计要点

- **三表索引**：`data` + `keys` + `keyIndex` 共同维护顺序与 O(1) 删除；三者长度不一致或下标错位时由 `reindexLocked` 自愈，读路径有兜底快照，写路径不为一致性付出全表扫描代价。
- **交换删除**：删除把末位键搬到空位，容量超过 `64` 且利用率低于 25% 时紧缩。
- **TTL 代次**：每个过期登记带自增 `gen`，重新设置 TTL 后旧定时器即使触发也会被识别为过期代次而丢弃。
- **锁外回调**：所有 `Del` / 过期 / `ExpireAll` 的清理回调都先收集成 `call`，释放写锁后再异步执行。
- **克隆两阶段**：先在无锁状态下建好整张副本图（`repl` / `made`），提交后再逐个激活定时器，且只锁本次新建的表。
- **JSON 重入计数**：`MarshalJSON` 用原子计数限制同一张表的未返回层数（上限 1024），配合 `seen` 集合截断环引用；嵌套错误被剥回同一个 `errMarshalCycle`。
- **类型缓存**：`typeMayContainMap` / `typeHasMap` / `typeNeedsDeepCopy` 三种类型查询共用 `sync.Map` 缓存，避免每次反射都重走类型树。

## 复杂度

| 操作 | 复杂度 |
| --- | --- |
| `Set` / `Get` / `Has` / `Del` | 均摊 O(1) |
| `Len` | O(1) |
| `Keys` / `Values` / `Range` | O(n) |
| `Clone` / `Copy` | O(n)（按元素深度） |
| `MarshalJSON` | O(n)，仅在键名字符串冲突时排序 |

## 限制

- 仅支持 `*Map`，按值使用会导致锁复制（`go vet` 的 `copylocks` 会报错）
- 键必须可比较且不含 NaN，等价于 Go 原生 map 的键规则
- 不保证键的稳定插入序
- `String()` 返回的是 JSON 文本，不是调试格式

## License

Apache-2.0