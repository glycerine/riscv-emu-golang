// kperf_helpers.h — kperf/kperfdata/kdebug declarations and library loading
// Extracted from ibireme's kpc_demo.c (public domain, unlicense.org)
// https://gist.github.com/ibireme/173517c208c7dc333ba962c1f0d67d12

#ifndef KPERF_HELPERS_H
#define KPERF_HELPERS_H

#include <stdio.h>
#include <stdint.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <dlfcn.h>
#include <sys/sysctl.h>
#include <sys/kdebug.h>

typedef uint8_t  u8;
typedef uint16_t u16;
typedef uint32_t u32;
typedef uint64_t u64;
typedef int32_t  i32;
typedef size_t   usize;

// ---- KPC constants ----

#define KPC_CLASS_FIXED             (0)
#define KPC_CLASS_CONFIGURABLE      (1)
#define KPC_CLASS_FIXED_MASK        (1u << KPC_CLASS_FIXED)
#define KPC_CLASS_CONFIGURABLE_MASK (1u << KPC_CLASS_CONFIGURABLE)
#define KPC_MAX_COUNTERS            32

#define KPERF_SAMPLER_PMC_THREAD    (1U << 4)
#define KPERF_ACTION_MAX            (32)
#define KPERF_TIMER_MAX             (8)

typedef u64 kpc_config_t;

// ---- kperf.framework function pointers ----

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
static int  (*kperf_action_count_set)(u32 count);
static int  (*kperf_action_count_get)(u32 *count);
static int  (*kperf_action_samplers_set)(u32 actionid, u32 sample);
static int  (*kperf_action_samplers_get)(u32 actionid, u32 *sample);
static int  (*kperf_action_filter_set_by_task)(u32 actionid, i32 port);
static int  (*kperf_action_filter_set_by_pid)(u32 actionid, i32 pid);
static int  (*kperf_timer_count_set)(u32 count);
static int  (*kperf_timer_count_get)(u32 *count);
static int  (*kperf_timer_period_set)(u32 actionid, u64 tick);
static int  (*kperf_timer_period_get)(u32 actionid, u64 *tick);
static int  (*kperf_timer_action_set)(u32 actionid, u32 timerid);
static int  (*kperf_timer_action_get)(u32 actionid, u32 *timerid);
static int  (*kperf_timer_pet_set)(u32 timerid);
static int  (*kperf_timer_pet_get)(u32 *timerid);
static int  (*kperf_sample_set)(u32 enabled);
static int  (*kperf_sample_get)(u32 *enabled);
static int  (*kperf_reset)(void);
static u64  (*kperf_ns_to_ticks)(u64 ns);
static u64  (*kperf_ticks_to_ns)(u64 ticks);
static u64  (*kperf_tick_frequency)(void);

static int kperf_lightweight_pet_set(u32 enabled) {
    return sysctlbyname("kperf.lightweight_pet", NULL, NULL, &enabled, 4);
}

// ---- kperfdata.framework types and function pointers ----

typedef struct kpep_event {
    const char *name, *description, *errata, *alias, *fallback;
    u32 mask; u8 number, umask, reserved, is_fixed;
} kpep_event;

typedef struct kpep_db {
    const char *name, *cpu_id, *marketing_name;
    void *plist_data, *event_map;
    kpep_event *event_arr;
    kpep_event **fixed_event_arr;
    void *alias_map;
    usize reserved_1, reserved_2, reserved_3;
    usize event_count, alias_count;
    usize fixed_counter_count, config_counter_count, power_counter_count;
    u32 archtecture, fixed_counter_bits, config_counter_bits, power_counter_bits;
} kpep_db;

typedef struct kpep_config {
    kpep_db *db;
    kpep_event **ev_arr;
    usize *ev_map, *ev_idx;
    u32 *flags;
    u64 *kpc_periods;
    usize event_count, counter_count;
    u32 classes, config_counter, power_counter, reserved;
} kpep_config;

static const char *kpep_config_error_names[] = {
    "none","invalid argument","out of memory","I/O","buffer too small",
    "current system unknown","database path invalid","database not found",
    "database architecture unsupported","database version unsupported",
    "database corrupt","event not found","conflicting events",
    "all counters must be forced","event unavailable","check errno"
};
static const char *kpep_config_error_desc(int code) {
    if (0 <= code && code < 16) return kpep_config_error_names[code];
    return "unknown error";
}

static int  (*kpep_config_create)(kpep_db *db, kpep_config **cfg_ptr);
static void (*kpep_config_free)(kpep_config *cfg);
static int  (*kpep_config_add_event)(kpep_config *cfg, kpep_event **ev_ptr, u32 flag, u32 *err);
static int  (*kpep_config_remove_event)(kpep_config *cfg, usize idx);
static int  (*kpep_config_force_counters)(kpep_config *cfg);
static int  (*kpep_config_events_count)(kpep_config *cfg, usize *count_ptr);
static int  (*kpep_config_events)(kpep_config *cfg, kpep_event **buf, usize buf_size);
static int  (*kpep_config_kpc)(kpep_config *cfg, kpc_config_t *buf, usize buf_size);
static int  (*kpep_config_kpc_count)(kpep_config *cfg, usize *count_ptr);
static int  (*kpep_config_kpc_classes)(kpep_config *cfg, u32 *classes_ptr);
static int  (*kpep_config_kpc_map)(kpep_config *cfg, usize *buf, usize buf_size);
static int  (*kpep_db_create)(const char *name, kpep_db **db_ptr);
static void (*kpep_db_free)(kpep_db *db);
static int  (*kpep_db_name)(kpep_db *db, const char **name);
static int  (*kpep_db_aliases_count)(kpep_db *db, usize *count);
static int  (*kpep_db_aliases)(kpep_db *db, const char **buf, usize buf_size);
static int  (*kpep_db_counters_count)(kpep_db *db, u8 classes, usize *count);
static int  (*kpep_db_events_count)(kpep_db *db, usize *count);
static int  (*kpep_db_events)(kpep_db *db, kpep_event **buf, usize buf_size);
static int  (*kpep_db_event)(kpep_db *db, const char *name, kpep_event **ev_ptr);
static int  (*kpep_event_name)(kpep_event *ev, const char **name_ptr);
static int  (*kpep_event_alias)(kpep_event *ev, const char **alias_ptr);
static int  (*kpep_event_description)(kpep_event *ev, const char **str_ptr);

// ---- Library loading ----

typedef struct { const char *name; void **impl; } lib_symbol;
#define lib_nelems(x) (sizeof(x) / sizeof((x)[0]))
#define lib_symbol_def(name) { #name, (void **)&name }

static const lib_symbol lib_symbols_kperf[] = {
    lib_symbol_def(kpc_pmu_version), lib_symbol_def(kpc_cpu_string),
    lib_symbol_def(kpc_set_counting), lib_symbol_def(kpc_get_counting),
    lib_symbol_def(kpc_set_thread_counting), lib_symbol_def(kpc_get_thread_counting),
    lib_symbol_def(kpc_get_config_count), lib_symbol_def(kpc_get_counter_count),
    lib_symbol_def(kpc_set_config), lib_symbol_def(kpc_get_config),
    lib_symbol_def(kpc_get_cpu_counters), lib_symbol_def(kpc_get_thread_counters),
    lib_symbol_def(kpc_force_all_ctrs_set), lib_symbol_def(kpc_force_all_ctrs_get),
    lib_symbol_def(kperf_action_count_set), lib_symbol_def(kperf_action_count_get),
    lib_symbol_def(kperf_action_samplers_set), lib_symbol_def(kperf_action_samplers_get),
    lib_symbol_def(kperf_action_filter_set_by_task),
    lib_symbol_def(kperf_action_filter_set_by_pid),
    lib_symbol_def(kperf_timer_count_set), lib_symbol_def(kperf_timer_count_get),
    lib_symbol_def(kperf_timer_period_set), lib_symbol_def(kperf_timer_period_get),
    lib_symbol_def(kperf_timer_action_set), lib_symbol_def(kperf_timer_action_get),
    lib_symbol_def(kperf_sample_set), lib_symbol_def(kperf_sample_get),
    lib_symbol_def(kperf_reset),
    lib_symbol_def(kperf_timer_pet_set), lib_symbol_def(kperf_timer_pet_get),
    lib_symbol_def(kperf_ns_to_ticks), lib_symbol_def(kperf_ticks_to_ns),
    lib_symbol_def(kperf_tick_frequency),
};

static const lib_symbol lib_symbols_kperfdata[] = {
    lib_symbol_def(kpep_config_create), lib_symbol_def(kpep_config_free),
    lib_symbol_def(kpep_config_add_event), lib_symbol_def(kpep_config_remove_event),
    lib_symbol_def(kpep_config_force_counters), lib_symbol_def(kpep_config_events_count),
    lib_symbol_def(kpep_config_events), lib_symbol_def(kpep_config_kpc),
    lib_symbol_def(kpep_config_kpc_count), lib_symbol_def(kpep_config_kpc_classes),
    lib_symbol_def(kpep_config_kpc_map),
    lib_symbol_def(kpep_db_create), lib_symbol_def(kpep_db_free),
    lib_symbol_def(kpep_db_name), lib_symbol_def(kpep_db_aliases_count),
    lib_symbol_def(kpep_db_aliases), lib_symbol_def(kpep_db_counters_count),
    lib_symbol_def(kpep_db_events_count), lib_symbol_def(kpep_db_events),
    lib_symbol_def(kpep_db_event), lib_symbol_def(kpep_event_name),
    lib_symbol_def(kpep_event_alias), lib_symbol_def(kpep_event_description),
};

#define LIB_PATH_KPERF     "/System/Library/PrivateFrameworks/kperf.framework/kperf"
#define LIB_PATH_KPERFDATA "/System/Library/PrivateFrameworks/kperfdata.framework/kperfdata"

static bool lib_inited = false;
static bool lib_has_err = false;
static char lib_err_msg[256];
static void *lib_handle_kperf = NULL;
static void *lib_handle_kperfdata = NULL;

static void lib_deinit(void) {
    lib_inited = false; lib_has_err = false;
    if (lib_handle_kperf) dlclose(lib_handle_kperf);
    if (lib_handle_kperfdata) dlclose(lib_handle_kperfdata);
    lib_handle_kperf = lib_handle_kperfdata = NULL;
    for (usize i = 0; i < lib_nelems(lib_symbols_kperf); i++)
        *lib_symbols_kperf[i].impl = NULL;
    for (usize i = 0; i < lib_nelems(lib_symbols_kperfdata); i++)
        *lib_symbols_kperfdata[i].impl = NULL;
}

static bool lib_init(void) {
#define return_err() do { lib_deinit(); lib_inited=true; lib_has_err=true; return false; } while(0)
    if (lib_inited) return !lib_has_err;
    lib_handle_kperf = dlopen(LIB_PATH_KPERF, RTLD_LAZY);
    if (!lib_handle_kperf) {
        snprintf(lib_err_msg, sizeof(lib_err_msg), "load kperf: %s", dlerror());
        return_err();
    }
    lib_handle_kperfdata = dlopen(LIB_PATH_KPERFDATA, RTLD_LAZY);
    if (!lib_handle_kperfdata) {
        snprintf(lib_err_msg, sizeof(lib_err_msg), "load kperfdata: %s", dlerror());
        return_err();
    }
    for (usize i = 0; i < lib_nelems(lib_symbols_kperf); i++) {
        *lib_symbols_kperf[i].impl = dlsym(lib_handle_kperf, lib_symbols_kperf[i].name);
        if (!*lib_symbols_kperf[i].impl) {
            snprintf(lib_err_msg, sizeof(lib_err_msg), "sym: %s", lib_symbols_kperf[i].name);
            return_err();
        }
    }
    for (usize i = 0; i < lib_nelems(lib_symbols_kperfdata); i++) {
        *lib_symbols_kperfdata[i].impl = dlsym(lib_handle_kperfdata, lib_symbols_kperfdata[i].name);
        if (!*lib_symbols_kperfdata[i].impl) {
            snprintf(lib_err_msg, sizeof(lib_err_msg), "sym: %s", lib_symbols_kperfdata[i].name);
            return_err();
        }
    }
    lib_inited = true; lib_has_err = false; return true;
#undef return_err
}

// ---- kdebug utilities ----

#if defined(__arm64__)
typedef uint64_t kd_buf_argtype;
#else
typedef uintptr_t kd_buf_argtype;
#endif

typedef struct {
    uint64_t timestamp;
    kd_buf_argtype arg1, arg2, arg3, arg4, arg5; /* arg5 = thread ID */
    uint32_t debugid;
#if defined(__LP64__) || defined(__arm64__)
    uint32_t cpuid;
    kd_buf_argtype unused;
#endif
} kd_buf;

typedef struct {
    unsigned int type, value1, value2, value3, value4;
} kd_regtype;

#define KDBG_VALCHECK 0x00200000U

static int kdebug_reset(void) {
    int mib[3] = { CTL_KERN, KERN_KDEBUG, KERN_KDREMOVE };
    return sysctl(mib, 3, NULL, NULL, NULL, 0);
}
static int kdebug_reinit(void) {
    int mib[3] = { CTL_KERN, KERN_KDEBUG, KERN_KDSETUP };
    return sysctl(mib, 3, NULL, NULL, NULL, 0);
}
static int kdebug_setreg(kd_regtype *kdr) {
    int mib[3] = { CTL_KERN, KERN_KDEBUG, KERN_KDSETREG };
    usize size = sizeof(kd_regtype);
    return sysctl(mib, 3, kdr, &size, NULL, 0);
}
static int kdebug_trace_setbuf(int nbufs) {
    int mib[4] = { CTL_KERN, KERN_KDEBUG, KERN_KDSETBUF, nbufs };
    return sysctl(mib, 4, NULL, NULL, NULL, 0);
}
static int kdebug_trace_enable(bool enable) {
    int mib[4] = { CTL_KERN, KERN_KDEBUG, KERN_KDENABLE, enable };
    return sysctl(mib, 4, NULL, 0, NULL, 0);
}
static int kdebug_trace_read(void *buf, usize len, usize *count) {
    if (count) *count = 0;
    if (!buf || !len) return -1;
    int mib[3] = { CTL_KERN, KERN_KDEBUG, KERN_KDREADTR };
    int ret = sysctl(mib, 3, buf, &len, NULL, 0);
    if (ret != 0) return ret;
    *count = len;
    return 0;
}

#define PERF_KPC             (6)
#define PERF_KPC_DATA_THREAD (8)

#endif // KPERF_HELPERS_H
