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
#
# notes:
#
# cache-references and cache-misses are Last Level Cache (LLC) events, 
# not L1. On your AMD Threadripper that's L3.
# So the Linux output is saying:
#
# L1D: 32.5B loads, 4.3M misses → 0.01% L1 miss rate (almost everything hits L1)
# LLC (L3): 16.3M references, 4.6M misses → 28.47% L3 miss rate (of 
# the loads that make it past L1 and L2 to L3, about a quarter miss L3 and go to DRAM)
#
# The funnel looks like: 32.5B loads → 4.3M miss L1 → some fraction 
# hits L2 → 16.3M reach L3 → 4.6M miss L3 → DRAM.
# The macOS tool isn't measuring LLC because we're only using 
# L1D events in the 3 configurable counter slots. You could add 
# LONGEST_LAT_CACHE.MISS (0x2e/0x01) as a 4th event on macOS to get 
# the equivalent of Linux's cache-misses, though you'd also want 
# LONGEST_LAT_CACHE.REFERENCE (0x2e/0x4f) for the denominator — and 
# that would use all 4 of the accessible configurable counter slots 
# (you have 8 total but only 4 in the counter mask group 4-7 
# that most events require).
#
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
