package tipsyabconfig

import (
	"encoding/json"
	"reflect"
	"sync"
	"time"
)

// json_cache.go caches the decoded form of JSON config values for
// GetConfigJSON / GetConfigStaticJSON, so a hot path that reads a large value
// (e.g. a 100 KB+ JSON) does not decode it again on every read.
//
// A published config_version never changes and its id is globally unique, so
// one version always decodes to the same result: entries need no
// invalidation. They live on the namespace snapshot (NamespaceSnapshot.parsed)
// and are dropped with it when a newer snapshot replaces it.
//
// The caller still owns out: on every read the cached result is deep-copied
// into out. Maps, slices, pointers and interfaces are copied; strings are
// immutable and shared. Copying is much cheaper than decoding (no tokenizing,
// no string allocation) and keeps GetConfigJSON's contract that out is the
// caller's to modify.
//
// Reads fall back to a plain json.Unmarshal into out, exactly as before, when
// the cached result could not reproduce it:
//   - *out is not the zero value: json.Unmarshal merges into existing values
//     (e.g. defaults filled in before the call), a copy would overwrite them;
//   - the type contains a struct with unexported fields (other than
//     time.Time), which reflection cannot deep-copy, or a chan / func /
//     unsafe.Pointer;
//   - out is not a non-nil pointer (json.Unmarshal reports the error);
//   - the snapshot has no cache (built outside applyProto).

// jsonCacheKey identifies one cached decode inside a snapshot. Version ids are
// globally unique, so the config key is not part of it.
type jsonCacheKey struct {
	typ     reflect.Type // *out's type
	version int64
}

type jsonCacheEntry struct {
	once     sync.Once
	val      reflect.Value // decoded value of typ; partial when err != nil
	err      error
	panicked bool
	panicVal any
}

// decodeJSONCached unmarshals raw (the value of version in snap) into out,
// reusing the decode cached on snap when possible. The returned error is
// json.Unmarshal's.
func decodeJSONCached(snap *NamespaceSnapshot, version int64, raw string, out any) error {
	rv := reflect.ValueOf(out)
	if snap == nil || snap.parsed == nil || !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return json.Unmarshal([]byte(raw), out)
	}
	dst := rv.Elem()
	typ := dst.Type()
	if !dst.IsZero() || !jsonTypeCacheable(typ) {
		return json.Unmarshal([]byte(raw), out)
	}

	k := jsonCacheKey{typ: typ, version: version}
	got, ok := snap.parsed.Load(k)
	if !ok {
		got, _ = snap.parsed.LoadOrStore(k, &jsonCacheEntry{})
	}
	e := got.(*jsonCacheEntry)
	e.once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				e.panicked, e.panicVal = true, r
			}
		}()
		v := reflect.New(typ)
		e.err = json.Unmarshal([]byte(raw), v.Interface())
		e.val = v.Elem()
	})
	if e.panicked {
		// A custom UnmarshalJSON panicked: re-panic on every read, as an
		// uncached json.Unmarshal would.
		panic(e.panicVal)
	}
	// Copy even on error: json.Unmarshal fills what it can before reporting a
	// type mismatch, and the caller used to see that partial state in out.
	dst.Set(e.val)
	unshare(dst)
	return e.err
}

var timeType = reflect.TypeOf(time.Time{})

// cacheableTypes memoises jsonTypeCacheable per top-level type (bool).
var cacheableTypes sync.Map

// jsonTypeCacheable reports whether a decoded value of t can be deep-copied by
// deepCopyJSONValue without losing or sharing mutable state.
func jsonTypeCacheable(t reflect.Type) bool {
	if v, ok := cacheableTypes.Load(t); ok {
		return v.(bool)
	}
	ok := typeCacheable(t, map[reflect.Type]bool{})
	cacheableTypes.Store(t, ok)
	return ok
}

// typeCacheable walks t. Results for types inside a recursion cycle are
// provisional, which is why only the top-level answer is memoised.
func typeCacheable(t reflect.Type, visiting map[reflect.Type]bool) bool {
	if visiting[t] {
		return true // the rest of the cycle is checked by the outer call
	}
	visiting[t] = true
	defer delete(visiting, t)
	switch t.Kind() {
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return false
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeCacheable(t.Elem(), visiting)
	case reflect.Map:
		return typeCacheable(t.Key(), visiting) && typeCacheable(t.Elem(), visiting)
	case reflect.Struct:
		if t == timeType {
			return true
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || !typeCacheable(f.Type, visiting) {
				return false
			}
		}
		return true
	default: // bool, numbers, string, interface
		return true
	}
}

// refTypes memoises typeHasRefs (bool). Memoising nested results is safe: a
// type inside a cycle necessarily reaches itself through a pointer, slice or
// map, so the provisional answer (true) is also the real one.
var refTypes sync.Map

// typeHasRefs reports whether a value of t can share mutable memory with a
// copy made by plain assignment, i.e. whether it contains a pointer, slice,
// map or interface. Strings are immutable and do not count.
func typeHasRefs(t reflect.Type) bool {
	if v, ok := refTypes.Load(t); ok {
		return v.(bool)
	}
	has := computeHasRefs(t, map[reflect.Type]bool{})
	refTypes.Store(t, has)
	return has
}

func computeHasRefs(t reflect.Type, visiting map[reflect.Type]bool) bool {
	if visiting[t] {
		return true
	}
	visiting[t] = true
	defer delete(visiting, t)
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return true
	case reflect.Array:
		return computeHasRefs(t.Elem(), visiting)
	case reflect.Struct:
		if t == timeType {
			return false // its *Location is treated as immutable
		}
		for i := 0; i < t.NumField(); i++ {
			if computeHasRefs(t.Field(i).Type, visiting) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// deepCopyJSONValue returns a copy of v that shares no maps, slices, pointers
// or interfaces with it. v's type must satisfy jsonTypeCacheable.
func deepCopyJSONValue(v reflect.Value) reflect.Value {
	n := reflect.New(v.Type()).Elem()
	n.Set(v)
	unshare(n)
	return n
}

// unshare turns dst, which holds a plain assignment copy of a cached value,
// into a deep copy: every map, slice and pointer it reaches is replaced by a
// fresh one. dst must be settable and its type must satisfy
// jsonTypeCacheable. It works in place so slice elements and struct fields
// need no temporary values.
func unshare(dst reflect.Value) {
	t := dst.Type()
	if !typeHasRefs(t) {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		if dst.IsNil() {
			return
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(dst.Elem())
		unshare(p.Elem())
		dst.Set(p)
	case reflect.Interface:
		if dst.IsNil() {
			return
		}
		src := dst.Elem()
		if !typeHasRefs(src.Type()) {
			return // a boxed string / number / bool is immutable: share it
		}
		c := reflect.New(src.Type()).Elem()
		c.Set(src)
		unshare(c)
		dst.Set(c)
	case reflect.Slice:
		if dst.IsNil() {
			return
		}
		n := reflect.MakeSlice(t, dst.Len(), dst.Len())
		reflect.Copy(n, dst)
		if typeHasRefs(t.Elem()) {
			for i := 0; i < n.Len(); i++ {
				unshare(n.Index(i))
			}
		}
		dst.Set(n)
	case reflect.Array:
		for i := 0; i < dst.Len(); i++ {
			unshare(dst.Index(i))
		}
	case reflect.Map:
		if dst.IsNil() {
			return
		}
		n := reflect.MakeMapWithSize(t, dst.Len())
		// One key and one value holder for the whole map: SetIterKey /
		// SetIterValue avoid an allocation per entry, and SetMapIndex copies
		// them into the new map.
		k := reflect.New(t.Key()).Elem()
		v := reflect.New(t.Elem()).Elem()
		keyRefs, valRefs := typeHasRefs(t.Key()), typeHasRefs(t.Elem())
		for it := dst.MapRange(); it.Next(); {
			k.SetIterKey(it)
			v.SetIterValue(it)
			if keyRefs {
				unshare(k)
			}
			if valRefs {
				unshare(v)
			}
			n.SetMapIndex(k, v)
		}
		dst.Set(n)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if typeHasRefs(t.Field(i).Type) {
				unshare(dst.Field(i))
			}
		}
	}
}
