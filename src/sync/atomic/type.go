// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic

import "unsafe"

// A Bool is an atomic boolean value.
// The zero value is false.
//
// Bool must not be copied after first use.
type Bool struct {
	_ noCopy
	v uint32
}

// Load atomically loads and returns the value stored in x.
func (x *Bool) Load() bool {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return LoadUint32(&x.v) != 0
}

// Store atomically stores val into x.
func (x *Bool) Store(val bool) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StoreUint32(&x.v, b32(val))
}

// Swap atomically stores new into x and returns the previous value.
func (x *Bool) Swap(new bool) (old bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return SwapUint32(&x.v, b32(new)) != 0
}

// CompareAndSwap executes the compare-and-swap operation for the boolean value x.
func (x *Bool) CompareAndSwap(old, new bool) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapUint32(&x.v, b32(old), b32(new))
}

// b32 returns a uint32 0 or 1 representing b.
func b32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// For testing *Pointer[T]'s methods can be inlined.
// Keep in sync with cmd/compile/internal/test/inl_test.go:TestIntendedInlining.
var _ = &Pointer[int]{}

// A Pointer is an atomic pointer of type *T. The zero value is a nil *T.
//
// Pointer must not be copied after first use.
type Pointer[T any] struct {
	// Mention *T in a field to disallow conversion between Pointer types.
	// See go.dev/issue/56603 for more details.
	// Use *T, not T, to avoid spurious recursive type definition errors.
	_ [0]*T

	_ noCopy
	v unsafe.Pointer
}

// Load atomically loads and returns the value stored in x.
func (x *Pointer[T]) Load() *T {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return (*T)(LoadPointer(&x.v))
}

// Store atomically stores val into x.
func (x *Pointer[T]) Store(val *T) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StorePointer(&x.v, unsafe.Pointer(val))
}

// Swap atomically stores new into x and returns the previous value.
func (x *Pointer[T]) Swap(new *T) (old *T) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return (*T)(SwapPointer(&x.v, unsafe.Pointer(new)))
}

// CompareAndSwap executes the compare-and-swap operation for x.
func (x *Pointer[T]) CompareAndSwap(old, new *T) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapPointer(&x.v, unsafe.Pointer(old), unsafe.Pointer(new))
}

// An Int32 is an atomic int32. The zero value is zero.
//
// Int32 must not be copied after first use.
type Int32 struct {
	_ noCopy
	v int32
}

// Load atomically loads and returns the value stored in x.
func (x *Int32) Load() int32 {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return LoadInt32(&x.v)
}

// Store atomically stores val into x.
func (x *Int32) Store(val int32) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StoreInt32(&x.v, val)
}

// Swap atomically stores new into x and returns the previous value.
func (x *Int32) Swap(new int32) (old int32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return SwapInt32(&x.v, new)
}

// CompareAndSwap executes the compare-and-swap operation for x.
func (x *Int32) CompareAndSwap(old, new int32) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapInt32(&x.v, old, new)
}

// Add atomically adds delta to x and returns the new value.
func (x *Int32) Add(delta int32) (new int32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AddInt32(&x.v, delta)
}

// And atomically performs a bitwise AND operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Int32) And(mask int32) (old int32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AndInt32(&x.v, mask)
}

// Or atomically performs a bitwise OR operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Int32) Or(mask int32) (old int32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return OrInt32(&x.v, mask)
}

// An Int64 is an atomic int64. The zero value is zero.
//
// Int64 must not be copied after first use.
type Int64 struct {
	_ noCopy
	_ align64
	v int64
}

// Load atomically loads and returns the value stored in x.
func (x *Int64) Load() int64 {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return LoadInt64(&x.v)
}

// Store atomically stores val into x.
func (x *Int64) Store(val int64) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StoreInt64(&x.v, val)
}

// Swap atomically stores new into x and returns the previous value.
func (x *Int64) Swap(new int64) (old int64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return SwapInt64(&x.v, new)
}

// CompareAndSwap executes the compare-and-swap operation for x.
func (x *Int64) CompareAndSwap(old, new int64) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapInt64(&x.v, old, new)
}

// Add atomically adds delta to x and returns the new value.
func (x *Int64) Add(delta int64) (new int64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AddInt64(&x.v, delta)
}

// And atomically performs a bitwise AND operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Int64) And(mask int64) (old int64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AndInt64(&x.v, mask)
}

// Or atomically performs a bitwise OR operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Int64) Or(mask int64) (old int64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return OrInt64(&x.v, mask)
}

// A Uint32 is an atomic uint32. The zero value is zero.
//
// Uint32 must not be copied after first use.
type Uint32 struct {
	_ noCopy
	v uint32
}

// Load atomically loads and returns the value stored in x.
func (x *Uint32) Load() uint32 {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return LoadUint32(&x.v)
}

// Store atomically stores val into x.
func (x *Uint32) Store(val uint32) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StoreUint32(&x.v, val)
}

// Swap atomically stores new into x and returns the previous value.
func (x *Uint32) Swap(new uint32) (old uint32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return SwapUint32(&x.v, new)
}

// CompareAndSwap executes the compare-and-swap operation for x.
func (x *Uint32) CompareAndSwap(old, new uint32) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapUint32(&x.v, old, new)
}

// Add atomically adds delta to x and returns the new value.
func (x *Uint32) Add(delta uint32) (new uint32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AddUint32(&x.v, delta)
}

// And atomically performs a bitwise AND operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Uint32) And(mask uint32) (old uint32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AndUint32(&x.v, mask)
}

// Or atomically performs a bitwise OR operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Uint32) Or(mask uint32) (old uint32) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return OrUint32(&x.v, mask)
}

// A Uint64 is an atomic uint64. The zero value is zero.
//
// Uint64 must not be copied after first use.
type Uint64 struct {
	_ noCopy
	_ align64
	v uint64
}

// Load atomically loads and returns the value stored in x.
func (x *Uint64) Load() uint64 {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return LoadUint64(&x.v)
}

// Store atomically stores val into x.
func (x *Uint64) Store(val uint64) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StoreUint64(&x.v, val)
}

// Swap atomically stores new into x and returns the previous value.
func (x *Uint64) Swap(new uint64) (old uint64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return SwapUint64(&x.v, new)
}

// CompareAndSwap executes the compare-and-swap operation for x.
func (x *Uint64) CompareAndSwap(old, new uint64) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapUint64(&x.v, old, new)
}

// Add atomically adds delta to x and returns the new value.
func (x *Uint64) Add(delta uint64) (new uint64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AddUint64(&x.v, delta)
}

// And atomically performs a bitwise AND operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Uint64) And(mask uint64) (old uint64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AndUint64(&x.v, mask)
}

// Or atomically performs a bitwise OR operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Uint64) Or(mask uint64) (old uint64) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return OrUint64(&x.v, mask)
}

// A Uintptr is an atomic uintptr. The zero value is zero.
//
// Uintptr must not be copied after first use.
type Uintptr struct {
	_ noCopy
	v uintptr
}

// Load atomically loads and returns the value stored in x.
func (x *Uintptr) Load() uintptr {
	weaveAtomic(weaveOpAtomicLoad, unsafe.Pointer(&x.v))
	return LoadUintptr(&x.v)
}

// Store atomically stores val into x.
func (x *Uintptr) Store(val uintptr) {
	weaveAtomic(weaveOpAtomicStore, unsafe.Pointer(&x.v))
	StoreUintptr(&x.v, val)
}

// Swap atomically stores new into x and returns the previous value.
func (x *Uintptr) Swap(new uintptr) (old uintptr) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return SwapUintptr(&x.v, new)
}

// CompareAndSwap executes the compare-and-swap operation for x.
func (x *Uintptr) CompareAndSwap(old, new uintptr) (swapped bool) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return CompareAndSwapUintptr(&x.v, old, new)
}

// Add atomically adds delta to x and returns the new value.
func (x *Uintptr) Add(delta uintptr) (new uintptr) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AddUintptr(&x.v, delta)
}

// And atomically performs a bitwise AND operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Uintptr) And(mask uintptr) (old uintptr) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return AndUintptr(&x.v, mask)
}

// Or atomically performs a bitwise OR operation on x using the bitmask
// provided as mask and returns the old value.
func (x *Uintptr) Or(mask uintptr) (old uintptr) {
	weaveAtomic(weaveOpAtomicRMW, unsafe.Pointer(&x.v))
	return OrUintptr(&x.v, mask)
}

// noCopy may be added to structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
//
// Note that it must not be embedded, due to the Lock and Unlock methods.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// align64 may be added to structs that must be 64-bit aligned.
// This struct is recognized by a special case in the compiler
// and will not work if copied to any other package.
type align64 struct{}
