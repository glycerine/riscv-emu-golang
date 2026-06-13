#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
QEMU_AARCH64="${QEMU_AARCH64:-qemu-system-aarch64}"
ARM64_QEMU_CACHE="${ARM64_QEMU_CACHE:-/tmp/riscv-arm64-qemu}"
ARM64_QEMU_KERNEL="${ARM64_QEMU_KERNEL:-${ROOT}/vmlinuz-virt}"
ARM64_QEMU_MEM="${ARM64_QEMU_MEM:-1024M}"
ARM64_QEMU_CPUS="${ARM64_QEMU_CPUS:-2}"
ARM64_QEMU_RUN="${ARM64_QEMU_RUN:-TestGuestMemory|TestCPU|TestDecode|TestRunCached|TestELF}"

if ! command -v "${QEMU_AARCH64}" >/dev/null 2>&1; then
	echo "qemu-system-aarch64 not found. Set QEMU_AARCH64=/path/to/qemu-system-aarch64." >&2
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

echo "── cross-building linux/arm64 test binary"
(
	cd "${ROOT}"
	GOCACHE="${GOCACHE:-/tmp/gocache-riscv-arm64}" \
	GOCPU_VIZJIT_OFF=1 \
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	"${GO}" test -c -o "${testbin}" .
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

if [[ "$#" -gt 0 ]]; then
	test_args=("$@")
else
	test_args=(-test.v -test.run "${ARM64_QEMU_RUN}")
fi
printf '%s\n' "${test_args[@]}" > "${rootfs}/test-argv"

echo "── packing initramfs"
(
	cd "${rootfs}"
	find . -print | cpio -o -H newc 2>/dev/null
) | gzip -9 > "${initramfs}"

echo "── booting qemu-system-aarch64"
set +e
"${QEMU_AARCH64}" \
	-M virt \
	-cpu max \
	-smp "${ARM64_QEMU_CPUS}" \
	-m "${ARM64_QEMU_MEM}" \
	-nographic \
	-no-reboot \
	-kernel "${ARM64_QEMU_KERNEL}" \
	-initrd "${initramfs}" \
	-append "console=ttyAMA0 panic=-1" \
	2>&1 | tee "${log}"
qemu_status="${PIPESTATUS[0]}"
set -e

test_status="$(sed -n 's/^GOCPU_QEMU_TEST_EXIT=//p' "${log}" | tail -1 | tr -d '\r')"
if [[ -z "${test_status}" ]]; then
	echo "qemu exited without a GOCPU_QEMU_TEST_EXIT marker; qemu status=${qemu_status}" >&2
	exit "${qemu_status:-1}"
fi

exit "${test_status}"
