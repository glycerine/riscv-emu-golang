// =============================================================================
// kpc_perf.c — macOS "perf stat" for Intel Ice Lake
// Measures L1D cache misses, hits, cycles, and instructions for a child process.
//
// Based on ibireme's kpc_demo.c (public domain).
// Adapted for Intel Ice Lake (i7-1068NG7) with L1 cache miss counters.
//
// https://gist.github.com/ibireme/173517c208c7dc333ba962c1f0d67d12
// https://gist.github.com/glycerine/e3cfbaf95ba8a2d0ba7f3344dd5d946a
//
// Created by YaoYuan <ibireme@gmail.com> on 2021.
// Released into the public domain (unlicense.org).
//
// Build:
//   clang -O2 -o kpc_perf kpc_perf.c -framework CoreFoundation
//
// Usage (requires root):
//   sudo ./kpc_perf ./bench.test [args...]
//   sudo ./kpc_perf -self        # run built-in test function
//
// Strategy (child mode):
//   Uses kperf PET (Profile Every Thread) with kdebug tracing, filtered by
//   PID. The kernel snapshots each thread's PMC values every 1ms into trace
//   buffers. We take the delta (last - first snapshot) per thread, then sum
//   across all threads. This gives exact per-process totals for multi-threaded
//   programs like Go.
//
// =============================================================================

#include <sys/wait.h>
#include <mach/mach_time.h>
#include "kperf_helpers.h"

// ---- Events: only L1D in configurable slots; cycles/instrs from fixed ----

#define EVENT_NAME_MAX 8
typedef struct { const char *alias; const char *names[EVENT_NAME_MAX]; } event_alias;

static const event_alias profile_events[] = {
    { "L1D-load-misses",   { "MEM_LOAD_RETIRED.L1_MISS"          }},
    { "L1D-load-hits",     { "MEM_LOAD_RETIRED.L1_HIT"           }},
    { "L1D-replacements",  { "L1D.REPLACEMENT"                   }},
    { "L1D-stall-cycles",  { "CYCLE_ACTIVITY.STALLS_L1D_MISS"    }},
};

static kpep_event *get_event(kpep_db *db, const event_alias *alias) {
    for (usize j = 0; j < EVENT_NAME_MAX && alias->names[j]; j++) {
        kpep_event *ev = NULL;
        if (kpep_db_event(db, alias->names[j], &ev) == 0) return ev;
    }
    return NULL;
}

// ---- Per-thread data from kdebug trace ----

typedef struct {
    u32 tid;
    u64 ts_first, ts_last;
    u64 ctrs_first[KPC_MAX_COUNTERS];
    u64 ctrs_last[KPC_MAX_COUNTERS];
    bool has_first, has_last;
} thr_data;

// ---- Built-in self-test ----

static void self_test_func(void) {
    const size_t N = 16 * 1024 * 1024;
    volatile char *arr = (volatile char *)malloc(N);
    if (!arr) return;
    for (size_t i = 0; i < N; i += 4096) arr[i] = (char)i;
    volatile char sink = 0;
    for (size_t i = 0; i < N; i += 64) sink += arr[i];
    free((void *)arr);
    (void)sink;
}

// ---- Setup PMC config (shared by both modes) ----

static kpep_db *db;
static kpep_event *ev_arr[8];
static usize ev_count;
static u32 classes;
static usize reg_count;
static kpc_config_t regs[KPC_MAX_COUNTERS];
static usize counter_map[KPC_MAX_COUNTERS];
static u32 counter_count;

static int setup_pmc(void) {
    int ret;
    if ((ret = kpep_db_create(NULL, &db)))
        { fprintf(stderr, "PMC db: %d (%s)\n", ret, kpep_config_error_desc(ret)); return 1; }

    fprintf(stderr, "PMC database: %s (%s)\n", db->name,
        db->marketing_name ? db->marketing_name : "?");
    fprintf(stderr, "Fixed: %zu, Configurable: %zu\n",
        db->fixed_counter_count, db->config_counter_count);

    kpep_config *cfg = NULL;
    if ((ret = kpep_config_create(db, &cfg)))
        { fprintf(stderr, "config create: %d\n", ret); return 1; }
    if ((ret = kpep_config_force_counters(cfg)))
        { fprintf(stderr, "force counters: %d\n", ret); return 1; }

    ev_count = sizeof(profile_events) / sizeof(profile_events[0]);
    for (usize i = 0; i < ev_count; i++) {
        ev_arr[i] = get_event(db, &profile_events[i]);
        if (!ev_arr[i]) {
            fprintf(stderr, "Warning: event '%s' not found\n", profile_events[i].alias);
            continue;
        }
        if ((ret = kpep_config_add_event(cfg, &ev_arr[i], 0, NULL))) {
            fprintf(stderr, "Warning: add '%s': %d (%s)\n",
                profile_events[i].alias, ret, kpep_config_error_desc(ret));
            ev_arr[i] = NULL;
        }
    }

    if ((ret = kpep_config_kpc_classes(cfg, &classes))) return 1;
    if ((ret = kpep_config_kpc_count(cfg, &reg_count))) return 1;
    if ((ret = kpep_config_kpc_map(cfg, counter_map, sizeof(counter_map)))) return 1;
    if ((ret = kpep_config_kpc(cfg, regs, sizeof(regs)))) return 1;

    classes |= KPC_CLASS_FIXED_MASK;

    // Debug
    fprintf(stderr, " DEBUG: classes=0x%x reg_count=%zu\n", classes, reg_count);
    for (usize i = 0; i < reg_count; i++)
        fprintf(stderr, "   reg[%zu]=0x%llx (ev=0x%02x umask=0x%02x)\n",
            i, regs[i], (u32)(regs[i]&0xFF), (u32)((regs[i]>>8)&0xFF));
    fprintf(stderr, " DEBUG: counter_map:");
    for (usize i = 0; i < ev_count; i++)
        fprintf(stderr, " [%zu]=%zu", i, counter_map[i]);
    fprintf(stderr, "\n");

    if ((ret = kpc_force_all_ctrs_set(1)))
        { fprintf(stderr, "force_all_ctrs: %d\n", ret); return 1; }
    if ((classes & KPC_CLASS_CONFIGURABLE_MASK) && reg_count) {
        if ((ret = kpc_set_config(classes, regs)))
            { fprintf(stderr, "set_config: %d\n", ret); return 1; }
    }

    counter_count = kpc_get_counter_count(classes);
    fprintf(stderr, " DEBUG: counter_count=%u\n", counter_count);

    if ((ret = kpc_set_counting(classes))) return 1;
    if ((ret = kpc_set_thread_counting(classes))) return 1;
    return 0;
}

static void teardown_pmc(void) {
    kpc_set_counting(0);
    kpc_set_thread_counting(0);
    kpc_force_all_ctrs_set(0);
}

// ---- Print results from a counter delta array ----

static void print_results(u64 *sum, double elapsed_s, pid_t pid, usize n_threads) {
    // Fixed counters (Ice Lake): [0]=INST_RETIRED [1]=CLK_UNHALTED.THREAD
    //   [2]=CLK_UNHALTED.REF_TSC [3]=TOPDOWN.SLOTS
    u64 instructions = sum[0];
    u64 cycles       = sum[1];
    u64 ref_cycles   = sum[2];

    fprintf(stderr, "\n Performance counter stats");
    if (pid > 0) fprintf(stderr, " (pid %d, %zu threads)", pid, n_threads);
    fprintf(stderr, ":\n");
    fprintf(stderr, " ─────────────────────────────────────────────────\n");
    fprintf(stderr, " %16llu   instructions          (fixed)\n", instructions);
    fprintf(stderr, " %16llu   cycles                (fixed)\n", cycles);
    fprintf(stderr, " %16llu   ref-cycles            (fixed)\n", ref_cycles);

    u64 l1_miss = 0, l1_hit = 0, l1_repl = 0, l1_stall = 0;
    for (usize i = 0; i < ev_count; i++) {
        if (!ev_arr[i]) continue;
        // counter_map[i] is index within configurable region (0-based).
        // In per-thread arrays, configurable starts after fixed counters.
        usize idx = counter_map[i] + db->fixed_counter_count;
        u64 val = sum[idx];
        fprintf(stderr, " %16llu   %-22s(configurable)\n", val, profile_events[i].alias);
        if (i == 0) l1_miss  = val;
        if (i == 1) l1_hit   = val;
        if (i == 2) l1_repl  = val;
        if (i == 3) l1_stall = val;
    }

    fprintf(stderr, " ─────────────────────────────────────────────────\n");
    if (cycles > 0)
        fprintf(stderr, " %16.2f   IPC\n", (double)instructions / cycles);
    if (l1_hit + l1_miss > 0)
        fprintf(stderr, " %15.2f%%   L1D load miss rate\n",
            100.0 * l1_miss / (l1_hit + l1_miss));
    if (cycles > 0 && l1_stall > 0)
        fprintf(stderr, " %15.2f%%   cycles stalled on L1D\n",
            100.0 * l1_stall / cycles);
    fprintf(stderr, " %14.6f s   elapsed\n\n", elapsed_s);
}

// ---- Main ----

int main(int argc, const char *argv[]) {
    if (argc < 2) {
        fprintf(stderr,
            "Usage: sudo %s <command> [args...]\n"
            "       sudo %s -self\n", argv[0], argv[0]);
        return 1;
    }
    bool self_mode = (strcmp(argv[1], "-self") == 0);

    if (!lib_init()) { fprintf(stderr, "Error: %s\n", lib_err_msg); return 1; }

    int tmp = 0;
    if (kpc_force_all_ctrs_get(&tmp))
        { fprintf(stderr, "Permission denied. Run with sudo.\n"); return 1; }

    if (setup_pmc()) return 1;

    // ==== Self-test mode: per-thread counters, no PET needed ====
    if (self_mode) {
        u64 c0[KPC_MAX_COUNTERS] = {0}, c1[KPC_MAX_COUNTERS] = {0};
        kpc_get_thread_counters(0, KPC_MAX_COUNTERS, c0);
        u64 t0 = mach_absolute_time();

        fprintf(stderr, "\nRunning built-in self-test...\n");
        self_test_func();

        u64 t1 = mach_absolute_time();
        kpc_get_thread_counters(0, KPC_MAX_COUNTERS, c1);
        teardown_pmc();

        u64 delta[KPC_MAX_COUNTERS];
        for (u32 i = 0; i < counter_count; i++) delta[i] = c1[i] - c0[i];

        mach_timebase_info_data_t tb; mach_timebase_info(&tb);
        double elapsed = (double)(t1-t0) * tb.numer / tb.denom / 1e9;
        print_results(delta, elapsed, 0, 1);
        return 0;
    }

    // ==== Child process mode: PET + kdebug ====

    fprintf(stderr, "exec:");
    for (int i = 1; i < argc; i++) fprintf(stderr, " [%s]", argv[i]);
    fprintf(stderr, "\n");

    // Fork child — it sleeps briefly while we set up PET
    pid_t child = fork();
    if (child < 0) { perror("fork"); return 1; }
    if (child == 0) {
        usleep(100000); // 100ms for parent to set up tracing
        execvp(argv[1], (char *const *)&argv[1]);
        perror("execvp");
        _exit(127);
    }
    fprintf(stderr, "child pid: %d\n", child);

    // Set up PET sampling filtered to child PID
    kperf_action_count_set(KPERF_ACTION_MAX);
    kperf_timer_count_set(KPERF_TIMER_MAX);

    u32 actionid = 1, timerid = 1;
    kperf_action_samplers_set(actionid, KPERF_SAMPLER_PMC_THREAD);
    kperf_action_filter_set_by_pid(actionid, child);

    u64 tick = kperf_ns_to_ticks(1000000); // 1ms period
    kperf_timer_period_set(actionid, tick);
    kperf_timer_action_set(actionid, timerid);
    kperf_timer_pet_set(timerid);
    kperf_lightweight_pet_set(1);
    kperf_sample_set(1);

    // Set up kdebug trace
    kdebug_reset();
    int nbufs = 1000000;
    kdebug_trace_setbuf(nbufs);
    kdebug_reinit();

    kd_regtype kdr = {0};
    kdr.type = KDBG_VALCHECK;
    kdr.value1 = KDBG_EVENTID(DBG_PERF, PERF_KPC, PERF_KPC_DATA_THREAD);
    kdebug_setreg(&kdr);
    kdebug_trace_enable(1);

    u64 time_start = mach_absolute_time();

    // Collect trace while child runs
    usize buf_cap = nbufs * 2;
    kd_buf *buf_hdr = (kd_buf *)malloc(sizeof(kd_buf) * buf_cap);
    kd_buf *buf_cur = buf_hdr;
    kd_buf *buf_end = buf_hdr + buf_cap;

    while (buf_hdr) {
        int status = 0;
        pid_t w = waitpid(child, &status, WNOHANG);
        bool child_done = (w == child);

        if (child_done && WIFEXITED(status) && WEXITSTATUS(status) != 0)
            fprintf(stderr, "[child exited %d]\n", WEXITSTATUS(status));

        // Expand buffer if needed
        if (buf_end - buf_cur < nbufs) {
            usize new_cap = buf_cap * 2;
            kd_buf *nb = (kd_buf *)realloc(buf_hdr, sizeof(kd_buf) * new_cap);
            if (!nb) { free(buf_hdr); buf_hdr = NULL; break; }
            buf_cur = nb + (buf_cur - buf_hdr);
            buf_end = nb + new_cap;
            buf_hdr = nb;
            buf_cap = new_cap;
        }

        // Read and filter trace records
        usize count = 0;
        kdebug_trace_read(buf_cur, sizeof(kd_buf) * nbufs, &count);
        for (kd_buf *b = buf_cur, *e = buf_cur + count; b < e; b++) {
            u32 cls = KDBG_EXTRACT_CLASS(b->debugid);
            u32 sub = KDBG_EXTRACT_SUBCLASS(b->debugid);
            u32 cod = KDBG_EXTRACT_CODE(b->debugid);
            if (cls == DBG_PERF && sub == PERF_KPC && cod == PERF_KPC_DATA_THREAD) {
                memmove(buf_cur, b, sizeof(kd_buf));
                buf_cur++;
            }
        }

        if (child_done) {
            usleep(10000); // final flush
            count = 0;
            if (buf_end - buf_cur >= nbufs) {
                kdebug_trace_read(buf_cur, sizeof(kd_buf) * nbufs, &count);
                for (kd_buf *b = buf_cur, *e = buf_cur + count; b < e; b++) {
                    u32 cls = KDBG_EXTRACT_CLASS(b->debugid);
                    u32 sub = KDBG_EXTRACT_SUBCLASS(b->debugid);
                    u32 cod = KDBG_EXTRACT_CODE(b->debugid);
                    if (cls == DBG_PERF && sub == PERF_KPC && cod == PERF_KPC_DATA_THREAD) {
                        memmove(buf_cur, b, sizeof(kd_buf));
                        buf_cur++;
                    }
                }
            }
            break;
        }

        usleep(2000); // 2ms poll
    }

    u64 time_end = mach_absolute_time();
    waitpid(child, NULL, 0); // reap if not yet

    // Stop everything
    kdebug_trace_enable(0);
    kdebug_reset();
    kperf_sample_set(0);
    kperf_lightweight_pet_set(0);
    teardown_pmc();

    if (!buf_hdr) { fprintf(stderr, "Trace buffer alloc failed\n"); return 1; }

    usize total_records = buf_cur - buf_hdr;
    fprintf(stderr, " DEBUG: %zu kdebug PMC records collected\n", total_records);
    if (total_records == 0) {
        fprintf(stderr, "No PMC data collected. PID filter may not have matched.\n");
        return 1;
    }

    // Aggregate per-thread: first/last snapshot
    usize thr_cap = 64, thr_count = 0;
    thr_data *threads = (thr_data *)calloc(thr_cap, sizeof(thr_data));

    for (kd_buf *b = buf_hdr; b < buf_cur; b++) {
        u32 func = b->debugid & KDBG_FUNC_MASK;
        if (func != DBG_FUNC_START) continue;
        u32 tid = (u32)b->arg5;
        if (!tid) continue;

        // Read counter snapshot (4 values per kd_buf, continuation records follow)
        u32 ci = 0;
        u64 ctrs[KPC_MAX_COUNTERS] = {0};
        ctrs[ci++] = b->arg1; ctrs[ci++] = b->arg2;
        ctrs[ci++] = b->arg3; ctrs[ci++] = b->arg4;
        if (ci < counter_count) {
            for (kd_buf *b2 = b+1; b2 < buf_cur; b2++) {
                if ((u32)b2->arg5 != tid) break;
                if ((b2->debugid & KDBG_FUNC_MASK) == DBG_FUNC_START) break;
                if (ci < counter_count) ctrs[ci++] = b2->arg1;
                if (ci < counter_count) ctrs[ci++] = b2->arg2;
                if (ci < counter_count) ctrs[ci++] = b2->arg3;
                if (ci < counter_count) ctrs[ci++] = b2->arg4;
                if (ci >= counter_count) break;
            }
        }
        if (ci < counter_count) continue; // truncated

        // Find or create thread entry
        thr_data *td = NULL;
        for (usize i = 0; i < thr_count; i++)
            if (threads[i].tid == tid) { td = &threads[i]; break; }
        if (!td) {
            if (thr_count == thr_cap) {
                thr_cap *= 2;
                threads = (thr_data *)realloc(threads, thr_cap * sizeof(thr_data));
            }
            td = &threads[thr_count++];
            memset(td, 0, sizeof(*td));
            td->tid = tid;
        }

        if (!td->has_first) {
            td->has_first = true;
            td->ts_first = b->timestamp;
            memcpy(td->ctrs_first, ctrs, counter_count * sizeof(u64));
        }
        td->has_last = true;
        td->ts_last = b->timestamp;
        memcpy(td->ctrs_last, ctrs, counter_count * sizeof(u64));
    }

    // Sum deltas across threads
    u64 sum[KPC_MAX_COUNTERS] = {0};
    usize good_threads = 0;
    for (usize i = 0; i < thr_count; i++) {
        thr_data *td = &threads[i];
        if (!td->has_first || !td->has_last) continue;
        if (td->ts_first == td->ts_last) continue;
        good_threads++;
        for (u32 c = 0; c < counter_count; c++)
            sum[c] += td->ctrs_last[c] - td->ctrs_first[c];
    }

    fprintf(stderr, " DEBUG: %zu threads, %zu with >=2 snapshots\n",
        thr_count, good_threads);

    mach_timebase_info_data_t tb; mach_timebase_info(&tb);
    double elapsed = (double)(time_end - time_start) * tb.numer / tb.denom / 1e9;

    print_results(sum, elapsed, child, good_threads);

    // Per-thread breakdown (if not too many)
    if (thr_count <= 32 && good_threads > 1) {
        fprintf(stderr, " Per-thread breakdown:\n");
        fprintf(stderr, "   %8s %14s %14s", "TID", "instructions", "cycles");
        for (usize i = 0; i < ev_count; i++)
            if (ev_arr[i]) fprintf(stderr, " %16s", profile_events[i].alias);
        fprintf(stderr, "\n");

        for (usize t = 0; t < thr_count; t++) {
            thr_data *td = &threads[t];
            if (!td->has_first || !td->has_last || td->ts_first == td->ts_last) continue;
            fprintf(stderr, "   %8u %14llu %14llu", td->tid,
                td->ctrs_last[0]-td->ctrs_first[0], td->ctrs_last[1]-td->ctrs_first[1]);
            for (usize i = 0; i < ev_count; i++) {
                if (!ev_arr[i]) continue;
                usize idx = counter_map[i] + db->fixed_counter_count;
                fprintf(stderr, " %16llu", td->ctrs_last[idx]-td->ctrs_first[idx]);
            }
            fprintf(stderr, "\n");
        }
        fprintf(stderr, "\n");
    }

    free(threads);
    free(buf_hdr);
    return 0;
}
