// =============================================================================
// kpc_perf.c — macOS "perf stat" for Intel Ice Lake
// Measures L1D cache misses, hits, cycles, and instructions for a child process.
//
// Based on ibireme's kpc_demo.c (public domain).
// 
// https://gist.github.com/ibireme/173517c208c7dc333ba962c1f0d67d12
// https://gist.github.com/glycerine/e3cfbaf95ba8a2d0ba7f3344dd5d946a
//
// Created by YaoYuan <ibireme@gmail.com> on 2021.
// Released into the public domain (unlicense.org).
//
// Adapted for Intel Ice Lake (i7-1068NG7) with L1 cache miss counters.
//
// Build:
//   clang -O2 -o kpc_perf kpc_perf.c -framework CoreFoundation
//
// Usage (requires root):
//   sudo ./kpc_perf ./bench.test [args...]
//   sudo ./kpc_perf -self        # run built-in test function
//
// =============================================================================

#include <stdio.h>
#include <stdint.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <dlfcn.h>
#include <sys/sysctl.h>
#include <sys/wait.h>
#include <mach/mach_time.h>
#include <signal.h>

typedef uint8_t  u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef uint64_t u64;
typedef int32_t  i32;
typedef size_t   usize;

// -----------------------------------------------------------------------------
// kperf.framework header (reverse engineered, from ibireme's kpc_demo)
// -----------------------------------------------------------------------------

#define KPC_CLASS_FIXED             (0)
#define KPC_CLASS_CONFIGURABLE      (1)
#define KPC_CLASS_FIXED_MASK        (1u << KPC_CLASS_FIXED)
#define KPC_CLASS_CONFIGURABLE_MASK (1u << KPC_CLASS_CONFIGURABLE)
#define KPC_MAX_COUNTERS            32

typedef u64 kpc_config_t;

static int  (*kpc_cpu_string)(char *buf, usize buf_size);
static u32  (*kpc_pmu_version)(void);
static u32  (*kpc_get_counting)(void);
static int  (*kpc_set_counting)(u32 classes);
static u32  (*kpc_get_thread_counting)(void);
static int  (*kpc_set_thread_counting)(u32 classes);
static u32  (*kpc_get_config_count)(u32 classes);
static int  (*kpc_get_config)(u32 classes, kpc_config_t *config);
static int  (*kpc_set_config)(u32 classes, kpc_config_t *config);
static u32  (*kpc_get_counter_count)(u32 classes);
static int  (*kpc_get_cpu_counters)(bool all_cpus, u32 classes, int *curcpu, u64 *buf);
static int  (*kpc_get_thread_counters)(u32 tid, u32 buf_count, u64 *buf);
static int  (*kpc_force_all_ctrs_set)(int val);
static int  (*kpc_force_all_ctrs_get)(int *val_out);

// -----------------------------------------------------------------------------
// kperfdata.framework header (reverse engineered)
// -----------------------------------------------------------------------------

#define KPEP_ARCH_X86_64 1

typedef struct kpep_event {
    const char *name;
    const char *description;
    const char *errata;
    const char *alias;
    const char *fallback;
    u32 mask;
    u8  number;
    u8  umask;
    u8  reserved;
    u8  is_fixed;
} kpep_event;

typedef struct kpep_db {
    const char *name;
    const char *cpu_id;
    const char *marketing_name;
    void *plist_data;
    void *event_map;
    kpep_event *event_arr;
    kpep_event **fixed_event_arr;
    void *alias_map;
    usize reserved_1;
    usize reserved_2;
    usize reserved_3;
    usize event_count;
    usize alias_count;
    usize fixed_counter_count;
    usize config_counter_count;
    usize power_counter_count;
    u32 archtecture;
    u32 fixed_counter_bits;
    u32 config_counter_bits;
    u32 power_counter_bits;
} kpep_db;

typedef struct kpep_config {
    kpep_db *db;
    kpep_event **ev_arr;
    usize *ev_map;
    usize *ev_idx;
    u32 *flags;
    u64 *kpc_periods;
    usize event_count;
    usize counter_count;
    u32 classes;
    u32 config_counter;
    u32 power_counter;
    u32 reserved;
} kpep_config;

static const char *kpep_config_error_names[] = {
    "none", "invalid argument", "out of memory", "I/O",
    "buffer too small", "current system unknown",
    "database path invalid", "database not found",
    "database architecture unsupported", "database version unsupported",
    "database corrupt", "event not found", "conflicting events",
    "all counters must be forced", "event unavailable", "check errno"
};

static const char *kpep_config_error_desc(int code) {
    if (0 <= code && code < 16) return kpep_config_error_names[code];
    return "unknown error";
}

static int (*kpep_config_create)(kpep_db *db, kpep_config **cfg_ptr);
static void (*kpep_config_free)(kpep_config *cfg);
static int (*kpep_config_add_event)(kpep_config *cfg, kpep_event **ev_ptr, u32 flag, u32 *err);
static int (*kpep_config_remove_event)(kpep_config *cfg, usize idx);
static int (*kpep_config_force_counters)(kpep_config *cfg);
static int (*kpep_config_events_count)(kpep_config *cfg, usize *count_ptr);
static int (*kpep_config_events)(kpep_config *cfg, kpep_event **buf, usize buf_size);
static int (*kpep_config_kpc)(kpep_config *cfg, kpc_config_t *buf, usize buf_size);
static int (*kpep_config_kpc_count)(kpep_config *cfg, usize *count_ptr);
static int (*kpep_config_kpc_classes)(kpep_config *cfg, u32 *classes_ptr);
static int (*kpep_config_kpc_map)(kpep_config *cfg, usize *buf, usize buf_size);
static int (*kpep_db_create)(const char *name, kpep_db **db_ptr);
static void (*kpep_db_free)(kpep_db *db);
static int (*kpep_db_name)(kpep_db *db, const char **name);
static int (*kpep_db_aliases_count)(kpep_db *db, usize *count);
static int (*kpep_db_aliases)(kpep_db *db, const char **buf, usize buf_size);
static int (*kpep_db_counters_count)(kpep_db *db, u8 classes, usize *count);
static int (*kpep_db_events_count)(kpep_db *db, usize *count);
static int (*kpep_db_events)(kpep_db *db, kpep_event **buf, usize buf_size);
static int (*kpep_db_event)(kpep_db *db, const char *name, kpep_event **ev_ptr);
static int (*kpep_event_name)(kpep_event *ev, const char **name_ptr);
static int (*kpep_event_alias)(kpep_event *ev, const char **alias_ptr);
static int (*kpep_event_description)(kpep_event *ev, const char **str_ptr);


// -----------------------------------------------------------------------------
// Dynamic library loading
// -----------------------------------------------------------------------------

typedef struct { const char *name; void **impl; } lib_symbol;
#define lib_nelems(x) (sizeof(x) / sizeof((x)[0]))
#define lib_symbol_def(name) { #name, (void **)&name }

static const lib_symbol lib_symbols_kperf[] = {
    lib_symbol_def(kpc_pmu_version),
    lib_symbol_def(kpc_cpu_string),
    lib_symbol_def(kpc_set_counting),
    lib_symbol_def(kpc_get_counting),
    lib_symbol_def(kpc_set_thread_counting),
    lib_symbol_def(kpc_get_thread_counting),
    lib_symbol_def(kpc_get_config_count),
    lib_symbol_def(kpc_get_counter_count),
    lib_symbol_def(kpc_set_config),
    lib_symbol_def(kpc_get_config),
    lib_symbol_def(kpc_get_cpu_counters),
    lib_symbol_def(kpc_get_thread_counters),
    lib_symbol_def(kpc_force_all_ctrs_set),
    lib_symbol_def(kpc_force_all_ctrs_get),
};

static const lib_symbol lib_symbols_kperfdata[] = {
    lib_symbol_def(kpep_config_create),
    lib_symbol_def(kpep_config_free),
    lib_symbol_def(kpep_config_add_event),
    lib_symbol_def(kpep_config_remove_event),
    lib_symbol_def(kpep_config_force_counters),
    lib_symbol_def(kpep_config_events_count),
    lib_symbol_def(kpep_config_events),
    lib_symbol_def(kpep_config_kpc),
    lib_symbol_def(kpep_config_kpc_count),
    lib_symbol_def(kpep_config_kpc_classes),
    lib_symbol_def(kpep_config_kpc_map),
    lib_symbol_def(kpep_db_create),
    lib_symbol_def(kpep_db_free),
    lib_symbol_def(kpep_db_name),
    lib_symbol_def(kpep_db_aliases_count),
    lib_symbol_def(kpep_db_aliases),
    lib_symbol_def(kpep_db_counters_count),
    lib_symbol_def(kpep_db_events_count),
    lib_symbol_def(kpep_db_events),
    lib_symbol_def(kpep_db_event),
    lib_symbol_def(kpep_event_name),
    lib_symbol_def(kpep_event_alias),
    lib_symbol_def(kpep_event_description),
};

#define lib_path_kperf     "/System/Library/PrivateFrameworks/kperf.framework/kperf"
#define lib_path_kperfdata "/System/Library/PrivateFrameworks/kperfdata.framework/kperfdata"

static bool lib_inited = false;
static bool lib_has_err = false;
static char lib_err_msg[256];
static void *lib_handle_kperf = NULL;
static void *lib_handle_kperfdata = NULL;

static void lib_deinit(void) {
    lib_inited = false;
    lib_has_err = false;
    if (lib_handle_kperf) dlclose(lib_handle_kperf);
    if (lib_handle_kperfdata) dlclose(lib_handle_kperfdata);
    lib_handle_kperf = NULL;
    lib_handle_kperfdata = NULL;
    for (usize i = 0; i < lib_nelems(lib_symbols_kperf); i++)
        *lib_symbols_kperf[i].impl = NULL;
    for (usize i = 0; i < lib_nelems(lib_symbols_kperfdata); i++)
        *lib_symbols_kperfdata[i].impl = NULL;
}

static bool lib_init(void) {
#define return_err() do { lib_deinit(); lib_inited = true; lib_has_err = true; return false; } while(0)

    if (lib_inited) return !lib_has_err;

    lib_handle_kperf = dlopen(lib_path_kperf, RTLD_LAZY);
    if (!lib_handle_kperf) {
        snprintf(lib_err_msg, sizeof(lib_err_msg),
            "Failed to load kperf.framework: %s", dlerror());
        return_err();
    }
    lib_handle_kperfdata = dlopen(lib_path_kperfdata, RTLD_LAZY);
    if (!lib_handle_kperfdata) {
        snprintf(lib_err_msg, sizeof(lib_err_msg),
            "Failed to load kperfdata.framework: %s", dlerror());
        return_err();
    }

    for (usize i = 0; i < lib_nelems(lib_symbols_kperf); i++) {
        const lib_symbol *s = &lib_symbols_kperf[i];
        *s->impl = dlsym(lib_handle_kperf, s->name);
        if (!*s->impl) {
            snprintf(lib_err_msg, sizeof(lib_err_msg),
                "Failed to load kperf symbol: %s", s->name);
            return_err();
        }
    }
    for (usize i = 0; i < lib_nelems(lib_symbols_kperfdata); i++) {
        const lib_symbol *s = &lib_symbols_kperfdata[i];
        *s->impl = dlsym(lib_handle_kperfdata, s->name);
        if (!*s->impl) {
            snprintf(lib_err_msg, sizeof(lib_err_msg),
                "Failed to load kperfdata symbol: %s", s->name);
            return_err();
        }
    }

    lib_inited = true;
    lib_has_err = false;
    return true;
#undef return_err
}


// -----------------------------------------------------------------------------
// Event configuration
// -----------------------------------------------------------------------------

// We try multiple names per logical event for cross-CPU compatibility.
// Your Ice Lake will match the Intel names.
#define EVENT_NAME_MAX 8
typedef struct {
    const char *alias;
    const char *names[EVENT_NAME_MAX];
} event_alias;

static const event_alias profile_events[] = {
    { "cycles", {
        "CPU_CLK_UNHALTED.THREAD",       // Intel Core
        "FIXED_CYCLES",                   // Apple Silicon
    }},
    { "instructions", {
        "INST_RETIRED.ANY",               // Intel
        "FIXED_INSTRUCTIONS",             // Apple Silicon
    }},
    { "L1D-misses", {
        "MEM_LOAD_RETIRED.L1_MISS",       // Intel Ice Lake / Skylake+
        "L1D_CACHE_MISS_LD",              // alias (resolves to same)
    }},
    { "L1D-hits", {
        "MEM_LOAD_RETIRED.L1_HIT",        // Intel Ice Lake / Skylake+
    }},
};

static kpep_event *get_event(kpep_db *db, const event_alias *alias) {
    for (usize j = 0; j < EVENT_NAME_MAX; j++) {
        const char *name = alias->names[j];
        if (!name) break;
        kpep_event *ev = NULL;
        if (kpep_db_event(db, name, &ev) == 0)
            return ev;
    }
    return NULL;
}


// -----------------------------------------------------------------------------
// Built-in self-test function (used with -self flag)
// -----------------------------------------------------------------------------

static void self_test_func(void) {
    // Deliberately cause L1 cache misses: stride through a large array
    // with a stride bigger than a cache line (64 bytes).
    const size_t N = 16 * 1024 * 1024; // 16M entries = 128 MB
    volatile char *arr = (volatile char *)malloc(N);
    if (!arr) { fprintf(stderr, "malloc failed\n"); return; }

    // Write with large stride to cause misses
    for (size_t i = 0; i < N; i += 4096) {
        arr[i] = (char)i;
    }
    // Read back with large stride
    volatile char sink = 0;
    for (size_t i = 0; i < N; i += 64) {
        sink += arr[i];
    }

    free((void *)arr);
    (void)sink;
}


// -----------------------------------------------------------------------------
// Main: "perf stat" style wrapper
//
// Strategy:
//   -self mode: uses kpc_get_thread_counters (precise, same thread)
//   child mode: uses kpc_get_cpu_counters (system-wide, all CPUs summed)
//               This includes noise from other processes, but for a short
//               benchmark that dominates the CPU it's a good approximation.
//               Think of it like "perf stat -a" (system-wide).
// -----------------------------------------------------------------------------

/// Read system-wide counters summed across all CPUs.
/// buf must hold at least ncpu * counter_count entries.
/// out[] receives the per-counter sums (counter_count entries).
static int read_cpu_counters_summed(u32 classes, u32 counter_count,
                                     u64 *out) {
    // Get CPU count
    int ncpu = 0;
    size_t sz = sizeof(ncpu);
    sysctlbyname("hw.ncpu", &ncpu, &sz, NULL, 0);
    if (ncpu <= 0) ncpu = 1;

    u64 *all = (u64 *)calloc(ncpu * KPC_MAX_COUNTERS, sizeof(u64));
    if (!all) return -1;

    int curcpu = 0;
    int ret = kpc_get_cpu_counters(true, classes, &curcpu, all);
    if (ret != 0) { free(all); return ret; }

    // Sum across CPUs for each counter slot
    memset(out, 0, sizeof(u64) * KPC_MAX_COUNTERS);
    for (int cpu = 0; cpu < ncpu; cpu++) {
        for (u32 c = 0; c < counter_count && c < KPC_MAX_COUNTERS; c++) {
            out[c] += all[cpu * counter_count + c];
        }
    }

    free(all);
    return 0;
}

int main(int argc, const char *argv[]) {
    int ret = 0;
    bool self_mode = false;

    if (argc < 2) {
        fprintf(stderr,
            "Usage: sudo %s <command> [args...]\n"
            "       sudo %s -self           (built-in cache-miss test)\n"
            "\n"
            "Measures CPU cycles, instructions, L1D cache hits & misses.\n"
            "  -self mode:  precise per-thread counters\n"
            "  child mode:  system-wide counters (like 'perf stat -a')\n"
            "               Best results when system is otherwise idle.\n",
            argv[0], argv[0]);
        return 1;
    }

    if (strcmp(argv[1], "-self") == 0)
        self_mode = true;

    // --- Load frameworks ---
    if (!lib_init()) {
        fprintf(stderr, "Error: %s\n", lib_err_msg);
        return 1;
    }

    // --- Check root ---
    int force_ctrs = 0;
    if (kpc_force_all_ctrs_get(&force_ctrs)) {
        fprintf(stderr, "Permission denied. Run with sudo.\n");
        return 1;
    }

    // --- Load PMC database ---
    kpep_db *db = NULL;
    if ((ret = kpep_db_create(NULL, &db))) {
        fprintf(stderr, "Cannot load PMC database: %d (%s)\n",
            ret, kpep_config_error_desc(ret));
        return 1;
    }
    fprintf(stderr, "PMC database: %s (%s)\n", db->name,
        db->marketing_name ? db->marketing_name : "unknown");
    fprintf(stderr, "Fixed counters: %zu, Configurable counters: %zu\n",
        db->fixed_counter_count, db->config_counter_count);

    // --- Create config ---
    kpep_config *cfg = NULL;
    if ((ret = kpep_config_create(db, &cfg))) {
        fprintf(stderr, "Failed to create config: %d (%s)\n",
            ret, kpep_config_error_desc(ret));
        return 1;
    }
    if ((ret = kpep_config_force_counters(cfg))) {
        fprintf(stderr, "Failed to force counters: %d (%s)\n",
            ret, kpep_config_error_desc(ret));
        return 1;
    }

    // --- Look up and add events ---
    const usize ev_count = sizeof(profile_events) / sizeof(profile_events[0]);
    kpep_event *ev_arr[ev_count];
    memset(ev_arr, 0, sizeof(ev_arr));

    for (usize i = 0; i < ev_count; i++) {
        const event_alias *alias = &profile_events[i];
        ev_arr[i] = get_event(db, alias);
        if (!ev_arr[i]) {
            fprintf(stderr, "Warning: cannot find event '%s', skipping.\n",
                alias->alias);
            continue;
        }
        if ((ret = kpep_config_add_event(cfg, &ev_arr[i], 0, NULL))) {
            fprintf(stderr, "Warning: failed to add event '%s': %d (%s)\n",
                alias->alias, ret, kpep_config_error_desc(ret));
            ev_arr[i] = NULL;
        }
    }

    // --- Get KPC register config ---
    u32 classes = 0;
    usize reg_count = 0;
    kpc_config_t regs[KPC_MAX_COUNTERS] = { 0 };
    usize counter_map[KPC_MAX_COUNTERS] = { 0 };
    u64 counters_0[KPC_MAX_COUNTERS] = { 0 };
    u64 counters_1[KPC_MAX_COUNTERS] = { 0 };

    if ((ret = kpep_config_kpc_classes(cfg, &classes))) {
        fprintf(stderr, "Failed to get kpc classes: %d\n", ret);
        return 1;
    }
    if ((ret = kpep_config_kpc_count(cfg, &reg_count))) {
        fprintf(stderr, "Failed to get kpc count: %d\n", ret);
        return 1;
    }
    if ((ret = kpep_config_kpc_map(cfg, counter_map, sizeof(counter_map)))) {
        fprintf(stderr, "Failed to get kpc map: %d\n", ret);
        return 1;
    }
    if ((ret = kpep_config_kpc(cfg, regs, sizeof(regs)))) {
        fprintf(stderr, "Failed to get kpc registers: %d\n", ret);
        return 1;
    }

    // Add fixed counters class so we also read fixed PMCs
    if (!(classes & KPC_CLASS_FIXED_MASK)) {
        classes |= KPC_CLASS_FIXED_MASK;
        for (usize i = 0; i < ev_count; i++) {
            if (ev_arr[i]) counter_map[i] += db->fixed_counter_count;
        }
    }

    // --- Apply config to kernel ---
    if ((ret = kpc_force_all_ctrs_set(1))) {
        fprintf(stderr, "Failed to force all counters: %d\n", ret);
        return 1;
    }
    if ((classes & KPC_CLASS_CONFIGURABLE_MASK) && reg_count) {
        if ((ret = kpc_set_config(classes, regs))) {
            fprintf(stderr, "Failed to set kpc config: %d\n", ret);
            return 1;
        }
    }

    // --- Start counting ---
    if ((ret = kpc_set_counting(classes))) {
        fprintf(stderr, "Failed to start counting: %d\n", ret);
        return 1;
    }
    if ((ret = kpc_set_thread_counting(classes))) {
        fprintf(stderr, "Failed to start thread counting: %d\n", ret);
        return 1;
    }

    // Total counter count (fixed + configurable)
    u32 total_counter_count = kpc_get_counter_count(classes);

    // --- Read counters BEFORE ---
    if (self_mode) {
        // Per-thread: precise
        if ((ret = kpc_get_thread_counters(0, KPC_MAX_COUNTERS, counters_0))) {
            fprintf(stderr, "Failed to read counters (before): %d\n", ret);
            return 1;
        }
    } else {
        // System-wide: sum across all CPUs
        if ((ret = read_cpu_counters_summed(classes, total_counter_count, counters_0))) {
            fprintf(stderr, "Failed to read CPU counters (before): %d\n", ret);
            return 1;
        }
    }

    u64 time_start = mach_absolute_time();

    // --- Run the target ---
    if (self_mode) {
        fprintf(stderr, "\nRunning built-in self-test (L1 cache stress)...\n\n");
        self_test_func();
    } else {
        // Fork and exec, then wait
        pid_t pid = fork();
        if (pid < 0) {
            perror("fork");
            return 1;
        }
        if (pid == 0) {
            // Child: exec the command
            execvp(argv[1], (char *const *)&argv[1]);
            perror("execvp");
            _exit(127);
        }
        // Parent: wait for child
        int status = 0;
        waitpid(pid, &status, 0);
        if (WIFEXITED(status) && WEXITSTATUS(status) != 0) {
            fprintf(stderr, "\n[child exited with status %d]\n",
                WEXITSTATUS(status));
        }
    }

    u64 time_end = mach_absolute_time();

    // --- Read counters AFTER ---
    if (self_mode) {
        if ((ret = kpc_get_thread_counters(0, KPC_MAX_COUNTERS, counters_1))) {
            fprintf(stderr, "Failed to read counters (after): %d\n", ret);
            return 1;
        }
    } else {
        if ((ret = read_cpu_counters_summed(classes, total_counter_count, counters_1))) {
            fprintf(stderr, "Failed to read CPU counters (after): %d\n", ret);
            return 1;
        }
    }

    // --- Stop counting ---
    kpc_set_counting(0);
    kpc_set_thread_counting(0);
    kpc_force_all_ctrs_set(0);

    // --- Compute elapsed time ---
    mach_timebase_info_data_t tb;
    mach_timebase_info(&tb);
    double elapsed_ns = (double)(time_end - time_start) * tb.numer / tb.denom;
    double elapsed_s  = elapsed_ns / 1e9;

    // --- Print results ---
    fprintf(stderr, "\n");
    fprintf(stderr, " Performance counter stats%s:\n",
        self_mode ? " (per-thread)" : " (system-wide, all CPUs)");
    fprintf(stderr, " ─────────────────────────────────────────\n");

    u64 val_cycles = 0, val_instrs = 0, val_l1_miss = 0, val_l1_hit = 0;

    for (usize i = 0; i < ev_count; i++) {
        if (!ev_arr[i]) continue;
        const event_alias *alias = &profile_events[i];
        usize idx = counter_map[i];
        u64 val = counters_1[idx] - counters_0[idx];

        fprintf(stderr, " %16llu   %s\n", val, alias->alias);

        if (strcmp(alias->alias, "cycles") == 0)       val_cycles = val;
        if (strcmp(alias->alias, "instructions") == 0)  val_instrs = val;
        if (strcmp(alias->alias, "L1D-misses") == 0)    val_l1_miss = val;
        if (strcmp(alias->alias, "L1D-hits") == 0)      val_l1_hit = val;
    }

    fprintf(stderr, " ─────────────────────────────────────────\n");

    if (val_cycles > 0 && val_instrs > 0) {
        fprintf(stderr, " %16.2f   IPC (instructions per cycle)\n",
            (double)val_instrs / val_cycles);
    }
    if (val_l1_hit + val_l1_miss > 0) {
        double miss_rate = 100.0 * val_l1_miss / (val_l1_hit + val_l1_miss);
        fprintf(stderr, " %15.2f%%   L1D cache miss rate\n", miss_rate);
    }
    fprintf(stderr, " %14.6f s   elapsed time\n", elapsed_s);
    if (!self_mode) {
        fprintf(stderr, "\n Note: system-wide counters include activity from all\n"
                        " processes. Run on an idle system for best accuracy.\n");
    }
    fprintf(stderr, "\n");

    // Also print fixed counters if available
    fprintf(stderr, " Fixed counter values:\n");
    for (usize i = 0; i < db->fixed_counter_count; i++) {
        kpep_event *ev = db->fixed_event_arr[i];
        u64 val = counters_1[i] - counters_0[i];
        if (ev && ev->name) {
            fprintf(stderr, " %16llu   %s (fixed)\n", val, ev->name);
        }
    }
    fprintf(stderr, "\n");

    return 0;
}
