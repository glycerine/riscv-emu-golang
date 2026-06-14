#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
ARM64_QEMU_COMPARE_BENCHTIME="${ARM64_QEMU_COMPARE_BENCHTIME:-1x}"
ARM64_QEMU_BENCH_TIMEOUT="${ARM64_QEMU_BENCH_TIMEOUT:-20m}"
ARM64_QEMU_CACHE="${ARM64_QEMU_CACHE:-/tmp/riscv-arm64-qemu-bench}"

mkdir -p "${ARM64_QEMU_CACHE}"
log="${ARM64_QEMU_CACHE}/bench-arm64-qemu.log"

bench_re='^BenchmarkCPU_FullExecution(_JIT_Rv8|_JIT_ABJIT)?$'

set +e
ARM64_QEMU_CACHE="${ARM64_QEMU_CACHE}" \
ARM64_QEMU_PACKAGE=./bench \
ARM64_QEMU_REQUIRE_RISCV_TESTS=0 \
ARM64_QEMU_MAIN=0 \
ARM64_QEMU_LOCKSTEP=0 \
ARM64_QEMU_STAGE_BENCH_ELFS=1 \
GO="${GO}" \
"${ROOT}/scripts/test-arm64-qemu.sh" \
	-test.run '^$' \
	-test.bench "${bench_re}" \
	-test.benchtime="${ARM64_QEMU_COMPARE_BENCHTIME}" \
	-test.benchmem \
	-test.timeout "${ARM64_QEMU_BENCH_TIMEOUT}" \
	>"${log}" 2>&1
status=$?
set -e

if [[ "${status}" != "0" ]]; then
	cat "${log}"
	exit "${status}"
fi

awk '
function scan_mips(   i, val) {
	val = ""
	for (i = 1; i <= NF; i++) {
		if ($i == "MIPS") {
			val = $(i - 1)
		}
	}
	return val
}
/^BenchmarkCPU_FullExecution_JIT_Rv8-/ {
	rv8 = scan_mips()
}
/^BenchmarkCPU_FullExecution_JIT_ABJIT-/ {
	abjit = scan_mips()
}
/^BenchmarkCPU_FullExecution-[0-9]+/ {
	interp = scan_mips()
}
END {
	missing = 0
	if (rv8 == "") {
		print "missing BenchmarkCPU_FullExecution_JIT_Rv8 MIPS result" > "/dev/stderr"
		missing = 1
	}
	if (abjit == "") {
		print "missing BenchmarkCPU_FullExecution_JIT_ABJIT MIPS result" > "/dev/stderr"
		missing = 1
	}
	if (interp == "") {
		print "missing BenchmarkCPU_FullExecution MIPS result" > "/dev/stderr"
		missing = 1
	}
	if (missing) {
		exit 1
	}

	printf "  %-50s %s MIPS\n", "Go JIT — rv8 Fixed Static Mapping (arm64/qemu):", rv8
	printf "  %-50s %s MIPS\n", "Go JIT — abjit (arm64/qemu):", abjit
	printf "  %-50s %s MIPS\n", "Go interpreter (no JIT, arm64/qemu):", interp
}
' "${log}" || {
	cat "${log}"
	exit 1
}
