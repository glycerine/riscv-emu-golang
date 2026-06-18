//go:build !tsnet

package main

func newVirtioNetPacketStack(cfg EmuConfig) (virtioNetPacketStack, error) {
	return newVirtioNetMemoryStack(), nil
}
