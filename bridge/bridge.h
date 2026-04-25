#ifndef BRIDGE_H
#define BRIDGE_H

#include <stdint.h>
#include <stdatomic.h>
#include <pthread.h>

#define RING_SIZE 1024
#define CACHE_LINE 64
#define IDLE_TIMEOUT_NS 100000000 // 100ms in nanoseconds

typedef struct {
    // Producer (Go) writes head, Consumer (C) reads head
    _Atomic uint64_t head; 
    char pad1[CACHE_LINE - sizeof(_Atomic uint64_t)];

    // Consumer (C) writes tail, Producer (Go) reads tail
    _Atomic uint64_t tail;
    char pad2[CACHE_LINE - sizeof(_Atomic uint64_t)];

    // Data buffer
    uint64_t buffer[RING_SIZE];

    // Control block
    pthread_mutex_t mutex;
    pthread_cond_t cond;
    _Atomic int32_t is_sleeping; 
} RingBuffer;

void ring_buffer_worker(RingBuffer* rb);
void ring_buffer_signal(RingBuffer* rb);

#endif