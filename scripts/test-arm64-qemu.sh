#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
QEMU_AARCH64="${QEMU_AARCH64:-}"
ARM64_QEMU_CACHE="${ARM64_QEMU_CACHE:-/tmp/riscv-arm64-qemu}"
ARM64_QEMU_KERNEL="${ARM64_QEMU_KERNEL:-${ROOT}/vmlinuz-virt}"
ARM64_QEMU_MACHINE="${ARM64_QEMU_MACHINE:-virt}"
ARM64_QEMU_CPU="${ARM64_QEMU_CPU:-cortex-a72}"
ARM64_QEMU_MEM="${ARM64_QEMU_MEM:-1024M}"
ARM64_QEMU_CPUS="${ARM64_QEMU_CPUS:-2}"
ARM64_QEMU_TIMEOUT="${ARM64_QEMU_TIMEOUT:-0}"
ARM64_QEMU_LOCKSTEP_TIMEOUT="${ARM64_QEMU_LOCKSTEP_TIMEOUT:-0}"
ARM64_QEMU_MAIN="${ARM64_QEMU_MAIN:-1}"
ARM64_QEMU_LOCKSTEP="${ARM64_QEMU_LOCKSTEP:-1}"
ARM64_QEMU_PACKAGE="${ARM64_QEMU_PACKAGE:-.}"
ARM64_QEMU_RUN="${ARM64_QEMU_RUN:-^(Test(GuestMemory.*|CPU.*|Decode.*|RunCached.*|LoadELF.*|FindSymbolAddr.*|FindExecLoads.*|FindTextSection.*|LowerARM64.*|JIT_.*|JITIC_MatchesInterpreter.*)|TestRISCVTests_(UI|UM|Smoke|UA|UF|UD|UC|UI_JIT_AOT|UM_JIT_AOT|UA_JIT_AOT|UF_JIT_AOT|UD_JIT_AOT|UC_JIT_AOT|UI_JIT_Lazy|UM_JIT_Lazy|UA_JIT_Lazy|UC_JIT_Lazy))$}"

if [[ -z "${QEMU_AARCH64}" ]]; then
	if command -v qemu-system-aarch64 >/dev/null 2>&1; then
		QEMU_AARCH64=qemu-system-aarch64
	elif command -v qemu-system-arm64 >/dev/null 2>&1; then
		QEMU_AARCH64=qemu-system-arm64
	fi
fi

if [[ -z "${QEMU_AARCH64}" || ! -x "$(command -v "${QEMU_AARCH64}" 2>/dev/null)" ]]; then
	echo "qemu-system-aarch64/qemu-system-arm64 not found. Set QEMU_AARCH64=/path/to/qemu-system-aarch64." >&2
	exit 127
fi

if [[ ! -f "${ARM64_QEMU_KERNEL}" ]]; then
	cat >&2 <<EOF
Missing ARM64 kernel: ${ARM64_QEMU_KERNEL}

Place an Alpine aarch64 virt kernel at the repo root, or override the path:

  curl -L -o "${ROOT}/vmlinuz-virt" \\
    https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/aarch64/netboot/vmlinuz-virt

Override with ARM64_QEMU_KERNEL=/path/to/vmlinuz-virt if you already have one.
EOF
	exit 2
fi

mkdir -p "${ARM64_QEMU_CACHE}"
testbin="${ARM64_QEMU_CACHE}/riscv-arm64.test"
initbin="${ARM64_QEMU_CACHE}/init"
rootfs="${ARM64_QEMU_CACHE}/rootfs"
initramfs="${ARM64_QEMU_CACHE}/riscv-arm64-initramfs.cpio.gz"
log="${ARM64_QEMU_CACHE}/qemu.log"

echo "── cross-building linux/arm64 test binary (${ARM64_QEMU_PACKAGE})"
(
	cd "${ROOT}"
	GOCACHE="${GOCACHE:-/tmp/gocache-riscv-arm64}" \
	GOCPU_VIZJIT_OFF=1 \
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	"${GO}" test -c -o "${testbin}" "${ARM64_QEMU_PACKAGE}"
)

echo "── building tiny linux/arm64 init"
(
	cd "${ROOT}"
	GOCACHE="${GOCACHE:-/tmp/gocache-riscv-arm64}" \
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	"${GO}" build -o "${initbin}" ./scripts/arm64-qemu-init
)

rm -rf "${rootfs}"
mkdir -p "${rootfs}/tmp" "${rootfs}/dev"
cp "${initbin}" "${rootfs}/init"
cp "${testbin}" "${rootfs}/riscv-arm64.test"
chmod 0755 "${rootfs}/init" "${rootfs}/riscv-arm64.test"

stage_bench_elfs() {
	local missing=0
	mkdir -p "${rootfs}/libriscv_guest"
	if [[ -f "${ROOT}/bench/libriscv_guest/bench_guest.elf" ]]; then
		cp "${ROOT}/bench/libriscv_guest/bench_guest.elf" "${rootfs}/libriscv_guest/bench_guest.elf"
	else
		echo "missing bench/libriscv_guest/bench_guest.elf (run make guest-elf)" >&2
		missing=1
	fi
	if [[ -f "${ROOT}/bench/coremark.elf" ]]; then
		cp "${ROOT}/bench/coremark.elf" "${rootfs}/coremark.elf"
	fi
	if [[ -f "${ROOT}/bench/dhrystone.elf" ]]; then
		cp "${ROOT}/bench/dhrystone.elf" "${rootfs}/dhrystone.elf"
	fi
	return "${missing}"
}

if [[ "${ARM64_QEMU_STAGE_BENCH_ELFS:-0}" != "0" ]]; then
	echo "── staging benchmark guest ELFs"
	stage_bench_elfs
fi

if [[ "$#" -gt 0 ]]; then
	require_riscv_tests="${ARM64_QEMU_REQUIRE_RISCV_TESTS:-0}"
else
	require_riscv_tests="${ARM64_QEMU_REQUIRE_RISCV_TESTS:-1}"
fi

shopt -s nullglob
riscv_test_elfs=(
	"${ROOT}"/riscv-elf-tests/rv64ui-p-*
	"${ROOT}"/riscv-elf-tests/rv64um-p-*
	"${ROOT}"/riscv-elf-tests/rv64ua-p-*
	"${ROOT}"/riscv-elf-tests/rv64uc-p-*
	"${ROOT}"/riscv-elf-tests/rv64uf-p-*
	"${ROOT}"/riscv-elf-tests/rv64ud-p-*
)
if [[ "${#riscv_test_elfs[@]}" -eq 0 ]]; then
	if [[ "${require_riscv_tests}" != "0" ]]; then
		echo "missing prebuilt riscv-test ELFs under ${ROOT}/riscv-elf-tests" >&2
		echo "expected files like riscv-elf-tests/rv64ui-p-add" >&2
		exit 2
	fi
else
	echo "── staging prebuilt RISC-V test ELFs (${#riscv_test_elfs[@]})"
	mkdir -p "${rootfs}/riscv-elf-tests"
	cp "${riscv_test_elfs[@]}" "${rootfs}/riscv-elf-tests/"
fi

write_test_env() {
	rm -f "${rootfs}/test-env"
	if [[ "${ARM64_QEMU_VIZJIT:-}" != "" || "${ARM64_QEMU_DEBUG_JIT:-}" != "" ]]; then
		: > "${rootfs}/test-env"
	fi
	if [[ "${ARM64_QEMU_VIZJIT:-}" != "" ]]; then
		cat >> "${rootfs}/test-env" <<EOF
GOCPU_VIZJIT=/tmp/vizjit
GOCPU_QEMU_DUMP_VIZJIT=1
EOF
	fi
	if [[ "${ARM64_QEMU_DEBUG_JIT:-}" != "" ]]; then
		cat >> "${rootfs}/test-env" <<EOF
GOCPU_DEBUG_JIT=1
EOF
	fi
}

run_qemu_case() {
	local label="$1"
	shift
	local case_log="${ARM64_QEMU_CACHE}/qemu-${label}.log"

	printf '%s\n' "$@" > "${rootfs}/test-argv"
	write_test_env

	echo "── packing initramfs (${label})"
	(
		cd "${rootfs}"
		find . -print | cpio -o -H newc 2>/dev/null
	) | gzip -9 > "${initramfs}"

	echo "── booting ${QEMU_AARCH64} (${label})"
	echo "   machine=${ARM64_QEMU_MACHINE} cpu=${ARM64_QEMU_CPU} smp=${ARM64_QEMU_CPUS} mem=${ARM64_QEMU_MEM}"
	set +e
	"${QEMU_AARCH64}" \
		-M "${ARM64_QEMU_MACHINE}" \
		-cpu "${ARM64_QEMU_CPU}" \
		-smp "${ARM64_QEMU_CPUS}" \
		-m "${ARM64_QEMU_MEM}" \
		-display none \
		-chardev stdio,id=serial0,signal=on \
		-serial chardev:serial0 \
		-no-reboot \
		-kernel "${ARM64_QEMU_KERNEL}" \
		-initrd "${initramfs}" \
		-append "console=ttyAMA0 panic=-1" \
		2>&1 | tee "${case_log}"
	local qemu_status="${PIPESTATUS[0]}"
	set -e
	cp "${case_log}" "${log}"

	local test_status
	test_status="$(sed -n 's/^GOCPU_QEMU_TEST_EXIT=//p' "${case_log}" | tail -1 | tr -d '\r')"
	if [[ -z "${test_status}" ]]; then
		echo "qemu exited without a GOCPU_QEMU_TEST_EXIT marker; qemu status=${qemu_status}" >&2
		exit "${qemu_status:-1}"
	fi
	if [[ "${test_status}" != "0" ]]; then
		exit "${test_status}"
	fi
}

if [[ "$#" -gt 0 ]]; then
	run_qemu_case custom "$@"
	exit 0
fi

ran=0
if [[ "${ARM64_QEMU_MAIN}" != "0" ]]; then
	run_qemu_case main -test.v -test.timeout "${ARM64_QEMU_TIMEOUT}" -test.run "${ARM64_QEMU_RUN}"
	ran=1
fi

if [[ "${ARM64_QEMU_LOCKSTEP}" != "0" ]]; then
	run_qemu_case lockstep-ui-1 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UI/(add|addi|addiw|addw|and|andi|auipc|beq|bge|bgeu|blt|bltu|bne)$'
	run_qemu_case lockstep-ui-2 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UI/(fence_i|jal|jalr|lb|lbu|ld|ld_st|lh|lhu|lui|lw|lwu|ma_data)$'
	run_qemu_case lockstep-ui-3 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UI/(or|ori|sb|sd|sh|simple|sll|slli|slliw|sllw|slt|slti|sltiu)$'
	run_qemu_case lockstep-ui-4 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UI/(sltu|sra|srai|sraiw|sraw|srl|srli|srliw|srlw|st_ld|sub|subw|sw|xor|xori)$'
	run_qemu_case lockstep-um-1 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UM/(div|divu|divuw|divw|rem|remu|remuw|remw)$'
	run_qemu_case lockstep-um-2 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UM/(mul|mulh|mulhsu|mulhu|mulw)$'
	run_qemu_case lockstep-ua-1 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UA/(amoadd_d|amoadd_w|amoand_d|amoand_w|amomax_d|amomax_w|amomaxu_d)$'
	run_qemu_case lockstep-ua-2 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UA/(amomaxu_w|amomin_d|amomin_w|amominu_d|amominu_w|amoor_d|amoor_w)$'
	run_qemu_case lockstep-ua-3 -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UA/(amoswap_d|amoswap_w|amoxor_d|amoxor_w|lrsc)$'
	run_qemu_case lockstep-uc -test.v -test.timeout "${ARM64_QEMU_LOCKSTEP_TIMEOUT}" -test.run '^TestRISCVTests_Lockstep_UC/(rvc)$'
	ran=1
fi

if [[ "${ran}" == "0" ]]; then
	echo "no ARM64 QEMU test lanes selected (ARM64_QEMU_MAIN=0 and ARM64_QEMU_LOCKSTEP=0)" >&2
	exit 2
fi
