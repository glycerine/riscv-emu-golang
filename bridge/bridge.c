#include "bridge.h"
#include <time.h>

static uint64_t get_now_ns() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
}

void ring_buffer_worker(RingBuffer* rb) {
    uint64_t current_tail = 0;
    uint64_t last_work_time = get_now_ns();

    while (1) {
        // Acquire-load: ensure we see data written by Go
        uint64_t current_head = atomic_load_explicit(&rb->head, memory_order_acquire);

        if (current_head != current_tail) {
            uint64_t slot = current_tail % RING_SIZE;
            
            // --- EMULATOR EXECUTION STEP ---
            rb->buffer[slot] *= 2; 
            // -------------------------------

            current_tail++;
            // Release-store: ensure Go sees our result
            atomic_store_explicit(&rb->tail, memory_order_release, current_tail);
            last_work_time = get_now_ns();
        } else {
            if ((get_now_ns() - last_work_time) > IDLE_TIMEOUT_NS) {
                pthread_mutex_lock(&rb->mutex);
                atomic_store_explicit(&rb->is_sleeping, 1, memory_order_relaxed);
                
                // Re-check head while holding mutex to avoid the "Lost Wakeup"
                if (atomic_load_explicit(&rb->head, memory_order_acquire) == current_tail) {
                    pthread_cond_wait(&rb->cond, &rb->mutex);
                }
                
                atomic_store_explicit(&rb->is_sleeping, 0, memory_order_relaxed);
                pthread_mutex_unlock(&rb->mutex);
                last_work_time = get_now_ns();
            }
        }
    }
}

void ring_buffer_signal(RingBuffer* rb) {
    pthread_mutex_lock(&rb->mutex);
    pthread_cond_signal(&rb->cond);
    pthread_mutex_unlock(&rb->mutex);
}