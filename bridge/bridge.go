package main

/*
#include <stdlib.h>
#include "bridge.h"
#cgo LDFLAGS: -lpthread
*/
import "C"
import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

type GoRingBuffer struct {
	ptr *C.RingBuffer
}

func NewRingBuffer() *GoRingBuffer {
	// Allocate on C heap to ensure the pointer is stable for C-land
	size := C.sizeof_RingBuffer
	p := (*C.RingBuffer)(C.malloc(size))
	
	// Zero out memory
	C.memset(unsafe.Pointer(p), 0, size)
	
	C.pthread_mutex_init(&p.mutex, nil)
	C.pthread_cond_init(&p.cond, nil)
	
	return &GoRingBuffer{ptr: p}
}

func (rb *GoRingBuffer) Push(val uint64) {
	// Use atomic.Load to bypass Go's register caching
	head := atomic.LoadUint64((*uint64)(unsafe.Pointer(&rb.ptr.head)))

	// Busy-wait if buffer is full
	for head-atomic.LoadUint64((*uint64)(unsafe.Pointer(&rb.ptr.tail))) >= uint64(C.RING_SIZE) {
		runtime.Gosched()
	}

	slot := head % uint64(C.RING_SIZE)
	rb.ptr.buffer[slot] = C.uint64_t(val)
	
	// Atomic Add serves as a Store-Release on most architectures
	atomic.AddUint64((*uint64)(unsafe.Pointer(&rb.ptr.head)), 1)

	// Check if we need to wake up the C worker
	if atomic.LoadInt32((*int32)(unsafe.Pointer(&rb.ptr.is_sleeping))) == 1 {
		C.ring_buffer_signal(rb.ptr)
	}
}

func (rb *GoRingBuffer) StartWorker() {
	go func() {
		runtime.LockOSThread()
		C.ring_buffer_worker(rb.ptr)
	}()
}