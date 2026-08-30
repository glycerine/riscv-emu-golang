//go:build !breadcrumb

package riscv

import (
	"errors"
	"fmt"
)

const breadcrumbEnabled = false

var ErrBreadcrumbDisabled = errors.New("riscv: breadcrumb tracing requires building with -tags breadcrumb")

type BreadcrumbTracer struct{}

type breadcrumbIODevice struct{}

func NewBreadcrumbTracer(BreadcrumbConfig) (*BreadcrumbTracer, error) {
	return nil, ErrBreadcrumbDisabled
}

func (c *CPU) SetBreadcrumbTracer(*BreadcrumbTracer) {}

func (c *CPU) BreadcrumbTracer() *BreadcrumbTracer { return nil }

func (b *BreadcrumbTracer) RecordPC(_, _, _ uint64, _ PrivilegeMode) error {
	return nil
}

func (b *BreadcrumbTracer) Activate(_, _, _ uint64, _ bool) error {
	return nil
}

func (b *BreadcrumbTracer) Pause(_, _, _ uint64) error {
	return nil
}

func (b *BreadcrumbTracer) Flush() error {
	return nil
}

func (b *BreadcrumbTracer) Close() error {
	return nil
}

func validateBreadcrumbConfig(c *EmuConfig) error {
	if c.Breadcrumb.requested() {
		return fmt.Errorf("%w", ErrBreadcrumbDisabled)
	}
	return nil
}

func prepareBreadcrumbTracer(_ *EmuConfig, _ bool) (*BreadcrumbTracer, error) {
	return nil, nil
}

func breadcrumbRecordPC(_ *CPU, _, _, _ uint64, _ PrivilegeMode) error {
	return nil
}

func breadcrumbAfterAttempt(_ *CPU, _, _, _ uint64, _ PrivilegeMode) error {
	return nil
}

func breadcrumbFlush(_ *CPU) error {
	return nil
}

func (m *biosMMIO) enableBreadcrumb(*BreadcrumbTracer) {}

func (m *biosMMIO) closeBreadcrumb() error {
	return nil
}

func (d *breadcrumbIODevice) Load(_, _ uint64) uint64 {
	return 0
}

func (d *breadcrumbIODevice) Store(_, _, _ uint64) *MemFault {
	return nil
}

func (d *breadcrumbIODevice) InterruptPending() bool {
	return false
}
