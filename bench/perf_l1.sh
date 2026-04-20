#!/bin/bash
# =============================================================================
# perf_l1.sh — Linux "perf stat" for L1D cache miss measurement
# Uses Linux generic PMC event names (works on both Intel and AMD).
#
# Usage:
#   chmod +x perf_l1.sh
#   sudo ./perf_l1.sh ./bench.test -test.v -test.count=1 -test.benchtime=1x \
#       -test.benchmem -test.run=xxx '-test.bench=^BenchmarkCPU_FullExecution$'
#
# For Intel-specific raw events matching kpc_perf.c on macOS, use:
#   perf stat -e r0108d1 -e r0001d1 -e r000151 -e r0c0ca3 -- <command>
#   (r0108d1 = MEM_LOAD_RETIRED.L1_MISS, r0001d1 = .L1_HIT,
#    r000151 = L1D.REPLACEMENT, r0c0ca3 = CYCLE_ACTIVITY.STALLS_L1D_MISS)
# =============================================================================

if [ $# -lt 1 ]; then
    echo "Usage: $0 <command> [args...]"
    exit 1
fi

exec perf stat \
    -e instructions \
    -e cycles \
    -e L1-dcache-loads \
    -e L1-dcache-load-misses \
    -e L1-dcache-stores \
    -e L1-dcache-store-misses \
    -e cache-references \
    -e cache-misses \
    -- "$@"
