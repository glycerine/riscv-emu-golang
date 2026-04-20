#!/bin/bash
# =============================================================================
# perf_l1.sh — Linux "perf stat" for L1D cache miss measurement
# Run the same events as kpc_perf.c for cross-validation.
#
# Usage:
#   chmod +x perf_l1.sh
#   sudo ./perf_l1.sh ./bench.test -test.v -test.count=1 -test.benchtime=1x \
#       -test.benchmem -test.run=xxx '-test.bench=^BenchmarkCPU_FullExecution$'
#
# Or without sudo if /proc/sys/kernel/perf_event_paranoid <= 1
# =============================================================================

if [ $# -lt 1 ]; then
    echo "Usage: $0 <command> [args...]"
    echo ""
    echo "Measures: instructions, cycles, L1D load hits/misses, L1D replacements,"
    echo "          L1D stall cycles. Same events as kpc_perf.c on macOS."
    exit 1
fi

# These are the exact same Intel PMC events used in kpc_perf.c:
#
#   MEM_LOAD_RETIRED.L1_MISS   = event 0xd1 umask 0x08
#   MEM_LOAD_RETIRED.L1_HIT    = event 0xd1 umask 0x01
#   L1D.REPLACEMENT            = event 0x51 umask 0x01
#   CYCLE_ACTIVITY.STALLS_L1D_MISS = event 0xa3 umask 0x0c cmask 0x0c
#
# perf uses r<CMASK><INV><UMASK><EVENT> encoding for raw events.
# Or we can use the symbolic names if perf knows them.

exec perf stat \
    -e instructions \
    -e cycles \
    -e ref-cycles \
    -e L1-dcache-load-misses \
    -e L1-dcache-loads \
    -e r0108d1 \
    -e r0001d1 \
    -e r000151 \
    -e r0c0ca3 \
    -- "$@"

# Raw event encoding explanation:
#   r<config> where config = (CMASK<<24)|(INV<<23)|(UMASK<<8)|EVENT
#   but perf uses a shorter hex: rUUEE or rCCUUEE
#
#   r0108d1 = umask=08 event=d1 = MEM_LOAD_RETIRED.L1_MISS
#   r0001d1 = umask=01 event=d1 = MEM_LOAD_RETIRED.L1_HIT
#   r000151 = umask=01 event=51 = L1D.REPLACEMENT
#   r0c0ca3 = cmask=0c umask=0c event=a3 = CYCLE_ACTIVITY.STALLS_L1D_MISS
#
# The first 4 events (instructions, cycles, ref-cycles, L1-dcache-*) are
# Linux generic events for quick reference. The raw events (r...) are the
# exact same PMC programming as kpc_perf.c uses on macOS.
