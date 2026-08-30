package riscv

import "os"

const defaultBreadcrumbInterval = uint64(1000)

// BreadcrumbConfig configures interpreted-PC breadcrumb tracing.
//
// The real tracer is compiled only with -tags breadcrumb. The config type is
// always available so command-line flag parsing can produce a clear error in
// default builds instead of silently ignoring requested tracing.
type BreadcrumbConfig struct {
	Path              string
	Outfile           *os.File
	Interval          uint64
	StartAt           uint64
	StopAt            uint64
	AfterInterval     uint64
	AfterAt           uint64
	StartPaused       bool
	GuestControl      bool
	IncludePrivileged bool
}

func (cfg BreadcrumbConfig) requested() bool {
	return cfg.Path != "" ||
		cfg.Outfile != nil ||
		cfg.Interval != 0 ||
		cfg.StartAt != 0 ||
		cfg.StopAt != 0 ||
		cfg.AfterInterval != 0 ||
		cfg.AfterAt != 0 ||
		cfg.StartPaused ||
		cfg.GuestControl ||
		cfg.IncludePrivileged
}

func (cfg BreadcrumbConfig) normalizedForEmu(bios bool) BreadcrumbConfig {
	if cfg.Interval == 0 {
		cfg.Interval = defaultBreadcrumbInterval
	}
	if bios {
		cfg.GuestControl = true
		cfg.StartPaused = true
	}
	return cfg
}
