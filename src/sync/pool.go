// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Pool是一个临时对象的集合，可以分开存取。
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// 任何存储在池中的对象可能被随时移除，而不会通知。当只有Pool持有引用的时候，
// 对象可能被释放。
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool可能被多个goroutine同时使用。
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// Pool的目标是缓存已经分配的对象并稍后使用，释放GC的压力。
// 所以，它可以用来构建free list。然后它并不适合与所有的free list.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
type Pool struct {
	// 每个P的固定大小的池，实际的类型是[P]poolLocal
	local unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal

	// 池的大小
	localSize uintptr // size of the local array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	New func() interface{}
}

// Local per-P Pool appendix.
type poolLocal struct {
	// 只能由每个P单独使用
	private interface{} // Can be used only by the respective P.

	// 任何P都可以使用
	shared []interface{} // Can be used by any P.

	// 互斥锁保护shared字段
	Mutex // Protects shared.

	// 内存填充，防止cache line失效
	pad [128]byte // Prevents false sharing.
}

// Put adds x to the pool.
func (p *Pool) Put(x interface{}) {
	if raceenabled {
		// Under race detector the Pool degenerates into no-op.
		// It's conforming, simple and does not introduce excessive
		// happens-before edges between unrelated goroutines.
		return
	}
	if x == nil {
		return
	}
	l := p.pin()
	if l.private == nil {
		l.private = x
		x = nil
	}
	runtime_procUnpin()
	if x == nil {
		return
	}
	l.Lock()
	l.shared = append(l.shared, x)
	l.Unlock()
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
func (p *Pool) Get() interface{} {
	if raceenabled {
		if p.New != nil {
			return p.New()
		}
		return nil
	}
	l := p.pin()   //获取poolLocal
	x := l.private // 当前P私有的
	l.private = nil
	// procUnpin()将之前goroutine和P的绑定的解除
	runtime_procUnpin()
	// 如果当前私有的不是空
	// 则返回当前P私有的
	if x != nil {
		return x
	}
	// 加锁去poolLocal中的共享队列获取最后一个
	l.Lock()
	last := len(l.shared) - 1
	if last >= 0 {
		x = l.shared[last]
		l.shared = l.shared[:last]
	}
	l.Unlock()
	if x != nil {
		return x
	}
	// 如果共享队列也找不到
	// 就调用getSlow
	return p.getSlow()
}

/*
尝试从其他P的poolLocal的shared列表获取
*/
func (p *Pool) getSlow() (x interface{}) {
	// See the comment in pin regarding ordering of the loads.
	size := atomic.LoadUintptr(&p.localSize) // load-acquire
	local := p.local                         // load-consume
	// Try to steal one element from other procs.
	pid := runtime_procPin()
	runtime_procUnpin()
	for i := 0; i < int(size); i++ {
		// 循环从其他P获取poolLocal
		l := indexLocal(local, (pid+i+1)%int(size))
		l.Lock()
		last := len(l.shared) - 1
		// 如果某个P的poolLocal的shared列表不为空
		// 则获取shared列表的最后一个元素并跳出循环
		if last >= 0 {
			x = l.shared[last]
			l.shared = l.shared[:last]
			l.Unlock()
			break
		}
		l.Unlock()
	}

	// 如果循环所有P的poolLocal都没有找到
	// 则创建一个新的
	if x == nil && p.New != nil {
		x = p.New()
	}
	return x
}

// pin pins the current goroutine to P, disables preemption and returns poolLocal pool for the P.
// Caller must call runtime_procUnpin() when done with the pool.
// pin函数将当前goutine关联到P，禁止抢占并且返回P的poolLocal
// 当对pool的操作完成后， 调用者必须调用runtime_procUnpin
func (p *Pool) pin() *poolLocal {
	pid := runtime_procPin()
	// In pinSlow we store to localSize and then to local, here we load in opposite order.
	// Since we've disabled preemption, GC can not happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	//
	//
	s := atomic.LoadUintptr(&p.localSize) // load-acquire
	l := p.local                          // load-consume
	// 根据P的id从池中返回poolLocal
	if uintptr(pid) < s {
		return indexLocal(l, pid)
	}
	// 否则创建池
	return p.pinSlow()
}

func (p *Pool) pinSlow() *poolLocal {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	// 将当前goroutine绑定到P并禁止抢占，返回P的ID
	// 这样做是防止goroutine被挂起之后再次运行时P已经发生变化
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	// p.localsize是Pool的大小
	// p.local是池中的每个元素
	s := p.localSize
	l := p.local
	// 如果pid<s，说明池中有元素
	// 如果>s，说明s为0，也就是说池的大小为0
	// 在池中根据P的id索引一个poolLocal
	if uintptr(pid) < s {
		return indexLocal(l, pid)
	}
	// 如果p.local为nil（池为空)
	// 将当前Pool加入到allPools
	if p.local == nil {
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	// 获取当前P的数量
	size := runtime.GOMAXPROCS(0)
	// 根据P的数量生成poolLocal池
	local := make([]poolLocal, size)
	// 将poolLocal池和吃的大小保存到Pool变量
	atomic.StorePointer((*unsafe.Pointer)(&p.local), unsafe.Pointer(&local[0])) // store-release
	atomic.StoreUintptr(&p.localSize, uintptr(size))                            // store-release
	// 最后根据P的id返回池中的元素
	return &local[pid]
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.
	// Defensively zero out everything, 2 reasons:
	// 1. To prevent false retention of whole Pools.
	// 2. If GC happens while a goroutine works with l.shared in Put/Get,
	//    it will retain whole Pool. So next cycle memory consumption would be doubled.
	for i, p := range allPools {
		allPools[i] = nil
		for i := 0; i < int(p.localSize); i++ {
			l := indexLocal(p.local, i)
			l.private = nil
			for j := range l.shared {
				l.shared[j] = nil
			}
			l.shared = nil
		}
		p.local = nil
		p.localSize = 0
	}
	allPools = []*Pool{}
}

var (
	allPoolsMu Mutex
	allPools   []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	return &(*[1000000]poolLocal)(l)[i]
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()
