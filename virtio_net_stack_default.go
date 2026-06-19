//go:build !tsnet

package riscv

func newVirtioNetPacketStack(cfg EmuConfig) (virtioNetPacketStack, error) {
	return newVirtioNetMemoryStack(), nil
}
